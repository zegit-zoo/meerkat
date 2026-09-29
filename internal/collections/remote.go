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
// The verdicts, as agreed on meerkat-mob#25:
//
//	remote tip R = the local commit L        current
//	R != L, R absent from the local objects  behind-remote (flag cannot
//	                                          tell behind from diverged)
//	R != L, R present locally                current, noted "local ahead
//	                                          of remote"
//	pull, dirty tree                         dirty
//	pull, not a fast-forward                 diverged
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
	has, err := repo.HasObject(ctx, tip, gitinfo.DefaultLimits)
	if err != nil {
		return report(RemoteUnknown, remoteNone, noteCapped, "remote check inconclusive: "+err.Error())
	}
	if has {
		// The remote's tip is already here, so the local branch contains it
		// or has moved away from it: nothing to fetch either way.
		return report(tip, remoteCurrent, noteLocalAhead, "")
	}
	if !spec.Pulls() {
		return report(tip, remoteBehind, "", "")
	}
	return c.pull(ctx, repo, up, tip, report)
}

// pull fast-forwards a clean working tree to the remote's tip and
// rebuilds the index from it, holding the reload slot throughout so no
// timer tick or SIGHUP rebuilds a tree that is mid-update. It never
// merges, rebases, stashes, checks out or resets: a dirty tree or a
// branch that cannot fast-forward is reported, and left exactly as it
// was.
func (c *Collection) pull(ctx context.Context, repo *gitinfo.Repo, up gitinfo.Upstream, tip string,
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
	out, err := c.reloadLocalLocked(ctx)
	if err != nil {
		// The tree moved and the index did not: the rebuild's own failure
		// already marked the content degraded, and the next local probe
		// retries it.
		o, _ := report(tip, remoteCurrent, notePullRebuildKO, "pulled to "+short(tip)+", but the rebuild failed: "+err.Error())
		return o, nil
	}
	o, _ := report(tip, remoteCurrent, "", "pulled to "+short(tip))
	o.Changed = out.Changed
	return o, nil
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
