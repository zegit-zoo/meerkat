package collections

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/gitinfo"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// freshness.go is what a `type: local` collection with a `refresh:`
// block knows about how current its search index is (meerkat-mob#25).
//
// The record is filled by the reconciliation path in reload.go: the
// mount stamps the token the first snapshot is built from, every probe
// records what is on disk now, and every rebuild records what the new
// snapshot was built from. Nothing here reads a filesystem on a request.
//
// A collection without such a block has no record at all, so it reports
// nothing and costs nothing (MK-FRESH-09).

// Freshness states. The set is CLOSED: these six values are the whole
// vocabulary, fixed before every producer exists, so later work adds
// producers and never a seventh word a client has to learn.
//
// When more than one condition holds, the first in this list that
// applies is the state:
//
//	unknown > behind-disk > diverged > dirty > behind-remote > current
//
// Which part of meerkat-mob#25 produces each:
//
//	current        A: the served index was built from what the last probe saw
//	behind-disk    A: the on-disk token moved and the rebuild has not landed
//	unknown        A: no probe has succeeded yet, or the last one failed
//	               B: the tips differ and how they relate cannot be told
//	behind-remote  B: the tree is current, and the remote tip is ahead of it
//	dirty          B: a pull was due but the tree has uncommitted changes
//	               (decided inside the opt-in pull, so never under `flag`)
//	diverged       B: the local branch cannot fast-forward to the remote tip
const (
	FreshCurrent      = "current"
	FreshBehindDisk   = "behind-disk"
	FreshBehindRemote = "behind-remote"
	FreshDirty        = "dirty"
	FreshDiverged     = "diverged"
	FreshUnknown      = "unknown"
)

// FreshnessStates returns the closed state set in precedence order.
func FreshnessStates() []string {
	return []string{FreshUnknown, FreshBehindDisk, FreshDiverged, FreshDirty, FreshBehindRemote, FreshCurrent}
}

// RemoteUnknown is Freshness.Remote when no remote tip is known. It is
// the only value until a remote check exists (meerkat-mob#25 part B),
// and what a failed remote check reports afterwards: an unreachable
// remote never degrades the collection (MK-FRESH-04).
const RemoteUnknown = "unknown"

// Freshness is one collection's freshness record (MK-FRESH-06).
//
// The tokens are opaque version strings, compared for equality and
// never parsed: today the (path, size, mtime) fingerprint of the pages
// on disk (kb.FingerprintFS). They travel in structured status and the
// log, never as a metric label (MK-FRESH-10).
type Freshness struct {
	// Loaded is the token the serving index was built from.
	Loaded string `json:"loaded"`
	// OnDisk is the token the last probe computed, or "" when that probe
	// failed.
	OnDisk string `json:"on_disk"`
	// Remote is the remote tip, or RemoteUnknown.
	Remote string `json:"remote"`
	// Behind reports that the last check saw newer content than the index
	// was built from: behind-disk, behind-remote, dirty or diverged.
	Behind bool `json:"behind"`
	// State is one of FreshnessStates.
	State string `json:"state"`
	// CheckedAt is when the last probe ran, successful or not; zero until
	// the first.
	CheckedAt time.Time `json:"checked_at,omitzero"`

	// LoadedCommit and OnDiskCommit are the working tree's commit when
	// the serving index was built and at the last probe (part B). They
	// are for DISPLAY: equality is decided by the tokens above. They are
	// read from git's own files (internal/gitinfo), and are empty when the
	// directory is not a git working tree.
	LoadedCommit string `json:"loaded_commit,omitempty"`
	OnDiskCommit string `json:"on_disk_commit,omitempty"`
	// RemoteCheckedAt is when the last remote check ran. Zero without
	// `remote_check`.
	RemoteCheckedAt time.Time `json:"remote_checked_at,omitzero"`
	// Note is one fixed phrase explaining the remote half, such as
	// "local ahead of remote". It never carries a URL, a path or an error
	// text; those go to the log.
	Note string `json:"note,omitempty"`

	// remote is the remote check's verdict, the input to the remote half
	// of the ladder.
	remote remoteVerdict
}

// remoteVerdict is what the last remote check concluded.
type remoteVerdict int

const (
	remoteNone         remoteVerdict = iota // no check, or the remote could not be asked
	remoteCurrent                           // the remote has nothing the tree lacks
	remoteBehind                            // the remote is ahead (or diverged; flag cannot tell)
	remoteDirty                             // pull refused: uncommitted changes
	remoteDiverged                          // pull refused: not a fast-forward
	remoteUndetermined                      // the tips differ, and how they relate is not known
)

// Fixed notes. A Note is one of these, never free text.
const (
	noteLocalAhead    = "local ahead of remote"
	noteFetchedAhead  = "remote tip fetched but not merged; run the pull or check manually"
	notePulledElse    = "pulled, but the tree is not at the remote tip"
	noteNotGit        = "not a git working tree"
	noteDetached      = "HEAD is detached"
	noteNoUpstream    = "the branch tracks no remote branch"
	noteUnsafeName    = "the upstream's remote or branch name was refused"
	noteRemoteFailed  = "the remote could not be reached"
	noteCapped        = "cannot tell how local and remote relate: object lookup capped"
	notePullDeferred  = "pull deferred: a rebuild was in flight"
	notePullFailed    = "pull failed"
	noteDirty         = "not pulled: the working tree has uncommitted changes"
	noteDiverged      = "not pulled: local and remote have diverged"
	notePullRebuildKO = "pulled, but the rebuild failed"
)

// settle recomputes State and Behind, in the documented precedence:
// the on-disk half first (unknown, behind-disk), then the remote half.
//
// A remote verdict that the tips differ in an undetermined way reads as
// unknown: the tree is not at the remote tip, and claiming current, or
// "local ahead", from an object's mere presence would be wrong (#116
// review, M1). A behind-remote verdict clears once the index is built at
// the remote tip, as after a hand pull and a rebuild, without waiting for
// the next remote check (S2).
func (f *Freshness) settle() {
	behind := f.remote == remoteBehind && (f.LoadedCommit == "" || f.LoadedCommit != f.Remote)
	switch {
	case f.OnDisk == "" || f.Loaded == "" || f.remote == remoteUndetermined:
		f.State = FreshUnknown
	case f.OnDisk != f.Loaded:
		f.State = FreshBehindDisk
	case f.remote == remoteDiverged:
		f.State = FreshDiverged
	case f.remote == remoteDirty:
		f.State = FreshDirty
	case behind:
		f.State = FreshBehindRemote
	default:
		f.State = FreshCurrent
	}
	f.Behind = f.State != FreshCurrent && f.State != FreshUnknown
}

// Stale reports whether the record says the served index is behind what
// the source holds: the states an advisory is sent for (part C of
// meerkat-mob#25). current and unknown are not stale: unknown says
// nothing a caller could act on.
func (f Freshness) Stale() bool {
	switch f.State {
	case FreshBehindDisk, FreshBehindRemote, FreshDirty, FreshDiverged:
		return true
	}
	return false
}

// RemoteProblem classifies why the last remote check could not answer:
// "config" when the working tree itself stops it (not a git tree, a
// detached HEAD, no upstream, a refused name), "network" when the
// remote could not be reached, and "" when it answered or never ran.
// `mk collections status --check` fails on "config" and not on
// "network": a CI job must not fail because a remote was briefly down.
func (f Freshness) RemoteProblem() string {
	switch f.Note {
	case noteNotGit, noteDetached, noteNoUpstream, noteUnsafeName:
		return "config"
	case noteRemoteFailed:
		return "network"
	}
	return ""
}

// MaxAdvisory bounds an advisory line, in bytes.
const MaxAdvisory = 240

// advisoryReasons explains each stale state in a fixed phrase.
var advisoryReasons = map[string]string{
	FreshBehindDisk:   "its pages changed on disk and the index has not been rebuilt yet",
	FreshBehindRemote: "the remote has commits this server has not loaded",
	FreshDirty:        "the checkout has uncommitted changes, so it was not updated",
	FreshDiverged:     "the checkout and its remote have diverged, so it was not updated",
}

// Advisory is the one-line notice a search or show carries for a stale
// collection, or "" when the collection is not stale. It names the
// collection and the state, never a commit, token, path or URL, and is
// at most MaxAdvisory bytes.
func (f Freshness) Advisory(collection string) string {
	reason, ok := advisoryReasons[f.State]
	if !ok {
		return ""
	}
	line := fmt.Sprintf("freshness: collection %q is %s (%s)", collection, f.State, reason)
	if len(line) > MaxAdvisory {
		line = strings.ToValidUTF8(line[:MaxAdvisory-3], "") + "..."
	}
	return line
}

// probed records a successful probe that saw onDisk, and the working
// tree's commit at that moment ("" when unknown).
func (r *reloadState) probed(onDisk, commit string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		return
	}
	r.fresh.OnDisk, r.fresh.OnDiskCommit, r.fresh.CheckedAt = onDisk, commit, at
	r.fresh.settle()
}

// remoteChecked records a remote check: the remote tip (or
// RemoteUnknown), what it means, and the fixed note explaining it.
func (r *reloadState) remoteChecked(tip string, v remoteVerdict, note string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		return
	}
	r.fresh.Remote, r.fresh.remote, r.fresh.Note, r.fresh.RemoteCheckedAt = tip, v, note, at
	r.fresh.settle()
}

// probeFailed records a probe that could not compute a token.
func (r *reloadState) probeFailed(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		return
	}
	r.fresh.OnDisk, r.fresh.CheckedAt = "", at
	r.fresh.settle()
}

// loaded records the token (and the commit, "" when unknown) a newly
// installed snapshot was built from.
func (r *reloadState) loaded(token, commit string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		return
	}
	r.fresh.Loaded, r.fresh.LoadedCommit = token, commit
	r.fresh.settle()
}

// freshness returns a copy of the record, and false when there is none.
func (r *reloadState) freshness() (Freshness, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.fresh == nil {
		return Freshness{}, false
	}
	return *r.fresh, true
}

// Freshness reports how current the collection's search index is, and
// false for a collection that has no record: every collection except a
// `type: local` one with a `refresh:` block.
func (c *Collection) Freshness() (Freshness, bool) { return c.status.freshness() }

// localFingerprint fingerprints a local collection's pages: through its
// own filesystem, or through the process globals when it has none (the
// single-collection case, see Open).
func localFingerprint(fsys fs.FS) (kb.Fingerprint, error) {
	if fsys == nil {
		return kb.FingerprintGlobal()
	}
	return kb.FingerprintFS(fsys)
}

// settleWindow is how recently a page may have been modified before a
// token taken over it is trusted.
//
// A modification time comes from the kernel's coarse clock (a tick of a
// few milliseconds) and is stored to the second or coarser by some
// filesystems (FAT, several network filesystems). A same-size rewrite
// inside one tick leaves the token unchanged. A probe that ran between
// the two writes would otherwise stamp a stale index current, and it
// would stay stale until some unrelated change moved the token. So a
// token taken while any page is younger than this window is UNSETTLED,
// and the next probe rebuilds whatever it sees. This is git's "racy
// clean" rule. Two seconds covers one-second timestamp granularity with
// room to spare.
const settleWindow = 2 * time.Second

// unsettled reports whether fp was taken too close to its newest page's
// modification to be trusted. A modification in the future counts too,
// within the same window, so that a small clock skew errs toward a spare
// rebuild. A far-future mtime does not count, so one badly dated file
// cannot make every probe rebuild forever.
func unsettled(fp kb.Fingerprint, probeStart time.Time) bool {
	if fp.Newest.IsZero() {
		return false
	}
	d := probeStart.Sub(fp.Newest)
	return d < settleWindow && d > -settleWindow
}

// localStamp is a mount-time fingerprint and when it was taken.
type localStamp struct {
	fp     kb.Fingerprint
	commit string
	at     time.Time
}

// stampLocal fingerprints fsys for a mount, and reads the commit of the
// working tree holding dir. It reports false when the fingerprint fails;
// a missing commit is only a missing display field.
func stampLocal(fsys fs.FS, dir string) (localStamp, bool) {
	at := time.Now()
	fp, err := localFingerprint(fsys)
	if err != nil {
		return localStamp{}, false
	}
	return localStamp{fp: fp, commit: headCommit(dir), at: at}, true
}

// workDir returns localDir, or "" when it was never set.
func (c *Collection) workDir() string {
	s, _ := c.localDir.Load().(string)
	return s
}

// headCommit is the commit the working tree holding dir is at, read from
// git's own files (never by running git, MK-SEC-12), or "" when dir is
// not in a git working tree or its HEAD cannot be read. A detached HEAD
// still has a commit.
func headCommit(dir string) string {
	if dir == "" {
		return ""
	}
	repo, err := gitinfo.Find(dir)
	if err != nil {
		return ""
	}
	commit, _, err := repo.Head()
	if err != nil && !errors.Is(err, gitinfo.ErrDetached) {
		return ""
	}
	return commit
}

// applyStamp records a mount-time stamp in the freshness record and the
// settle flag. The snapshot's version is set by the caller, which
// installs the snapshot.
func (c *Collection) applyStamp(st localStamp) {
	c.localUnsettled = unsettled(st.fp, st.at)
	c.status.loaded(st.fp.Token, st.commit)
	c.status.probed(st.fp.Token, st.commit, st.at)
}
