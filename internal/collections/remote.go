package collections

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/gitinfo"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// remote.go is part B of meerkat-mob#25: a `type: local` collection
// whose directory is a git working tree can ask its upstream for the
// remote tip (`refresh.remote_check`), and optionally fast-forward to it
// (`refresh.on_divergence: pull`).
//
// The verdicts, as agreed on meerkat-mob#25 and corrected in the review
// of #116 (M1: an object's presence says nothing about ancestry; any
// fetch, the pull's own included, makes the remote tip present):
//
//	remote tip R = the local commit L        current
//	flag, R absent from the local objects    behind-remote (flag cannot
//	                                          tell behind from diverged)
//	flag, R present locally                  unknown, noted "remote tip
//	                                          fetched but not merged"
//	pull, R != L                             pull, and let git decide:
//	  fast-forwarded to R                    current
//	  nothing to fast-forward                current, "local ahead"
//	  not a fast-forward                     diverged
//	  dirty tree                             dirty
//
// A remote check NEVER fails the cycle and never degrades the collection
// (MK-FRESH-04): anything that stops it from answering leaves the remote
// "unknown", with a fixed note on the record and the detail in the log.

// gitRunner is the three hardened git calls (internal/gitinfo), a package
// var so this package's tests can drive the verdicts without a remote.
type gitRunner interface {
	LsRemote(ctx context.Context, url, branch string) (string, error)
	Clean(ctx context.Context, root string) (bool, error)
	PullFFOnly(ctx context.Context, root, remote, branch string) error
}

var remoteGit gitRunner = gitinfo.DefaultRunner

// UseFileRemotesForTest lets the remote check reach bare repositories on
// disk (the file transport), which production refuses. It is for tests
// in OTHER packages (internal/mcp, internal/cli) that need a genuinely
// stale collection. It returns the function that restores production.
// Nothing outside a _test.go file calls it.
func UseFileRemotesForTest() (restore func()) {
	r := gitinfo.DefaultRunner
	r.AllowProtocols = "file"
	prev := remoteGit
	remoteGit = r
	return func() { remoteGit = prev }
}

// remoteTarget runs one collection's remote check on its own cadence.
type remoteTarget struct {
	c       *Collection
	ordinal int
	spec    *refresh.Spec
}

// newRemoteTarget returns the remote target of a local collection that
// asked for `remote_check`, or nil.
func newRemoteTarget(c *Collection, ordinal int) refresh.Target {
	r := c.Source.Refresh
	if c.Source.Type != contentsource.TypeLocal || r == nil || r.RemoteCheck <= 0 {
		return nil
	}
	every := r.RemoteCheck
	return &remoteTarget{c: c, ordinal: ordinal, spec: &refresh.Spec{Interval: every, Jitter: every / 10}}
}

func (t *remoteTarget) Key() refresh.Key {
	return refresh.Key{Ordinal: t.ordinal, Kind: refresh.KindRemote, Name: t.c.Name}
}

func (t *remoteTarget) Spec() *refresh.Spec { return t.spec }

func (t *remoteTarget) Reconcile(ctx context.Context) (refresh.Outcome, error) {
	return t.c.CheckRemote(ctx)
}

// CheckRemote asks the working tree's upstream for its tip and records
// the verdict on the freshness record. With `on_divergence: pull`, a
// clean tree that is behind is fast-forwarded and rebuilt.
//
// It returns an error only for a collection that has no remote check
// configured. Everything else — no git repository, a detached HEAD, no
// upstream, an unreachable remote, a refused pull — is a verdict, not a
// failure.
func (c *Collection) CheckRemote(ctx context.Context) (refresh.Outcome, error) {
	return c.checkRemote(ctx, true)
}

// ProbeRemote is CheckRemote that never pulls, whatever `on_divergence`
// says: the verdict flag mode would give, recorded on the freshness
// record. It is what `mk collections status` runs. A short-lived CLI
// reports, the long-running server acts.
func (c *Collection) ProbeRemote(ctx context.Context) (refresh.Outcome, error) {
	return c.checkRemote(ctx, false)
}

func (c *Collection) checkRemote(ctx context.Context, allowPull bool) (refresh.Outcome, error) {
	spec := c.Source.Refresh
	if c.Source.Type != contentsource.TypeLocal || spec == nil || spec.RemoteCheck <= 0 {
		return refresh.Outcome{}, fmt.Errorf("collection %q has no remote_check configured", c.Name)
	}
	dir := c.workDir()
	if c.IsCold() || dir == "" {
		return refresh.Outcome{}, nil // nothing loaded yet: nothing to compare
	}
	now := time.Now()
	report := func(tip string, v remoteVerdict, note, logNote string) (refresh.Outcome, error) {
		c.status.remoteChecked(tip, v, note, now)
		return refresh.Outcome{Note: logNote}, nil
	}

	repo, err := gitinfo.Find(dir)
	if err != nil {
		return report(RemoteUnknown, remoteNone, noteNotGit, "remote check skipped: "+err.Error())
	}
	local, branch, err := repo.Head()
	if errors.Is(err, gitinfo.ErrDetached) {
		return report(RemoteUnknown, remoteNone, noteDetached, "remote check skipped: HEAD is detached")
	}
	if err != nil {
		return report(RemoteUnknown, remoteNone, noteNotGit, "remote check skipped: "+err.Error())
	}
	up, err := repo.Upstream(branch)
	if errors.Is(err, gitinfo.ErrUnsafeName) {
		return report(RemoteUnknown, remoteNone, noteUnsafeName, fmt.Sprintf("remote check skipped: branch %q: %v", branch, err))
	}
	if err != nil {
		return report(RemoteUnknown, remoteNone, noteNoUpstream, fmt.Sprintf("remote check skipped: branch %q: %v", branch, err))
	}
	tip, err := remoteGit.LsRemote(ctx, up.URL, up.Branch)
	if err != nil {
		return report(RemoteUnknown, remoteNone, noteRemoteFailed, "remote check failed: "+err.Error())
	}
	if tip == local {
		return report(tip, remoteCurrent, "", "")
	}
	if allowPull && spec.Pulls() {
		// Let git decide: it knows the ancestry, this package only reads
		// files. A branch that is ahead fast-forwards to nothing, a behind
		// one to the tip, and a diverged one is refused.
		return c.pull(ctx, repo, up, local, tip, report)
	}
	has, err := repo.HasObject(ctx, tip, gitinfo.DefaultLimits)
	if err != nil {
		return report(tip, remoteUndetermined, noteCapped, "remote check inconclusive: "+err.Error())
	}
	if has {
		// Present is not contained: a plain `git fetch` makes the tip
		// present while the branch is still behind. Flag mode cannot walk
		// the history, so it says so rather than guessing.
		return report(tip, remoteUndetermined, noteFetchedAhead, "")
	}
	return report(tip, remoteBehind, "", "")
}

// pull fast-forwards a clean working tree to the remote's tip and
// rebuilds the index from it, holding the reload slot throughout so no
// timer tick or SIGHUP rebuilds a tree that is mid-update. It never
// merges, rebases, stashes, checks out or resets: a dirty tree or a
// branch that cannot fast-forward is reported, and left exactly as it
// was.
func (c *Collection) pull(ctx context.Context, repo *gitinfo.Repo, up gitinfo.Upstream, local, tip string,
	report func(string, remoteVerdict, string, string) (refresh.Outcome, error),
) (refresh.Outcome, error) {
	if !c.reloadMu.TryLock() {
		return report(tip, remoteBehind, notePullDeferred, "pull deferred to the next check: a rebuild is in flight")
	}
	defer c.reloadMu.Unlock()

	clean, err := remoteGit.Clean(ctx, repo.Root)
	if err != nil {
		return report(tip, remoteBehind, notePullFailed, "not pulled: "+err.Error())
	}
	if !clean {
		return report(tip, remoteDirty, noteDirty, "not pulled: the working tree has uncommitted changes")
	}
	err = remoteGit.PullFFOnly(ctx, repo.Root, up.Remote, up.Branch)
	if errors.Is(err, gitinfo.ErrNotFastForward) {
		return report(tip, remoteDiverged, noteDiverged, "not pulled: local and remote have diverged")
	}
	if err != nil {
		return report(tip, remoteBehind, notePullFailed, "pull failed: "+err.Error())
	}
	// Where did the pull land? git decided; the files say what it did.
	// A repository whose config rewrites URLs is refused before git runs
	// (meerkat-mob#31), but remote.*.url and refspecs can still steer a
	// fetch, so the landing commit is compared, not assumed (review N3).
	head, _, herr := repo.Head()
	verdict, note, logNote := remoteCurrent, "", "pulled to "+short(head)
	switch {
	case herr != nil:
		verdict, note, logNote = remoteUndetermined, notePulledElse, "pulled, but HEAD could not be read: "+herr.Error()
	case head == tip:
	case head == local:
		verdict, note, logNote = remoteCurrent, noteLocalAhead, "nothing to fast-forward: local is ahead of the remote tip"
	default:
		verdict, note, logNote = remoteUndetermined, notePulledElse, "pulled to "+short(head)+", not to the remote tip "+short(tip)
	}
	out, err := c.reloadLocalLocked(ctx)
	if err != nil {
		// The tree moved and the index did not: the rebuild's own failure
		// already marked the content degraded, and the next local probe
		// retries it.
		o, _ := report(tip, verdict, notePullRebuildKO, logNote+"; the rebuild failed: "+err.Error())
		return o, nil
	}
	o, _ := report(tip, verdict, note, logNote)
	o.Changed = out.Changed
	return o, nil
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
