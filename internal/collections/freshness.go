package collections

import (
	"io/fs"
	"time"

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
}

// settle recomputes State and Behind from the tokens. Part A knows only
// the on-disk half of the ladder.
func (f *Freshness) settle() {
	switch {
	case f.OnDisk == "" || f.Loaded == "":
		f.State = FreshUnknown
	case f.OnDisk != f.Loaded:
		f.State = FreshBehindDisk
	default:
		f.State = FreshCurrent
	}
	f.Behind = f.State != FreshCurrent && f.State != FreshUnknown
}

// probed records a successful probe that saw onDisk.
func (r *reloadState) probed(onDisk string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		return
	}
	r.fresh.OnDisk, r.fresh.CheckedAt = onDisk, at
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

// loaded records the token a newly installed snapshot was built from.
func (r *reloadState) loaded(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fresh == nil {
		return
	}
	r.fresh.Loaded = token
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
	fp kb.Fingerprint
	at time.Time
}

// stampLocal fingerprints fsys for a mount, reporting false on failure.
func stampLocal(fsys fs.FS) (localStamp, bool) {
	at := time.Now()
	fp, err := localFingerprint(fsys)
	if err != nil {
		return localStamp{}, false
	}
	return localStamp{fp: fp, at: at}, true
}

// applyStamp records a mount-time stamp in the freshness record and the
// settle flag. The snapshot's version is set by the caller, which
// installs the snapshot.
func (c *Collection) applyStamp(st localStamp) {
	c.localUnsettled = unsettled(st.fp, st.at)
	c.status.loaded(st.fp.Token)
	c.status.probed(st.fp.Token, st.at)
}
