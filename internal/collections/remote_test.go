package collections

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/gitinfo"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// remote_test.go pins part B of meerkat-mob#25 end to end, against real
// git repositories: the remote check's verdicts, the opt-in pull, and
// that a flag-mode check or a refused pull never touches the tree.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// gitT runs a helper git command in dir, with no global or system config.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// useFileRemotes lets the hardened runner reach bare repositories on
// disk for the duration of a test; production pins https, ssh and git.
func useFileRemotes(t *testing.T) {
	t.Helper()
	r := gitinfo.DefaultRunner
	r.AllowProtocols = "file"
	prev := remoteGit
	remoteGit = r
	t.Cleanup(func() { remoteGit = prev })
}

// remoteFixture is a bare remote holding one page, and a working-tree
// clone of it mounted as a `type: local` collection named "kb".
type remoteFixture struct {
	bare, dir string
	reg       *Registry
	kb        *Collection
	remote    refresh.Target
}

func newRemoteFixture(t *testing.T, onDivergence string) *remoteFixture {
	t.Helper()
	requireGit(t)
	useFileRemotes(t)
	seed := t.TempDir()
	gitT(t, seed, "init", "-q")
	writeLocalPage(t, seed, "notes/stable", "The stable page about lighthouses.")
	gitT(t, seed, "add", ".")
	gitT(t, seed, "commit", "-q", "-m", "seed")
	bare := filepath.Join(t.TempDir(), "kb.git")
	gitT(t, seed, "clone", "-q", "--bare", seed, bare)
	dir := filepath.Join(t.TempDir(), "kb")
	gitT(t, t.TempDir(), "clone", "-q", bare, dir)
	ageTree(t, filepath.Join(dir, "wiki"))

	src := contentsource.Source{
		Type: contentsource.TypeLocal, Path: dir, Layout: contentsource.Layout{Wiki: "wiki"},
		Refresh: &refresh.Spec{
			Interval:     refresh.Duration(time.Minute),
			RemoteCheck:  refresh.Duration(time.Minute),
			OnDivergence: onDivergence,
		},
	}
	// A second, plain collection: with more than one mounted, each reads
	// through its own filesystem rather than the process-global one (see
	// Open), which is what a probe of kb's directory needs.
	other := t.TempDir()
	writeLocalPage(t, other, "other/page", "An unrelated page.")
	reg, err := Open(context.Background(), []contentsource.ResolvedCollection{
		{Name: "kb", Dir: dir, Provenance: "disk:" + dir, Source: src},
		{Name: "other", Dir: other, Provenance: "disk:" + other, Source: contentsource.Source{Type: contentsource.TypeLocal, Path: other, Layout: contentsource.Layout{Wiki: "wiki"}}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	f := &remoteFixture{bare: bare, dir: dir, reg: reg, kb: mustGet(t, reg, "kb")}
	for _, tg := range reg.RefreshTargets() {
		if tg.Key().Kind == refresh.KindRemote {
			f.remote = tg
		}
	}
	if f.remote == nil {
		t.Fatal("remote_check is set but there is no remote target")
	}
	return f
}

// pushPage commits one page to the bare remote from a scratch clone and
// returns the new remote tip.
func (f *remoteFixture) pushPage(t *testing.T, id, body string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "pusher")
	gitT(t, t.TempDir(), "clone", "-q", f.bare, wt)
	writeLocalPage(t, wt, id, body)
	gitT(t, wt, "add", ".")
	gitT(t, wt, "commit", "-q", "-m", id)
	gitT(t, wt, "push", "-q", "origin", "main")
	return gitT(t, wt, "rev-parse", "HEAD")
}

func (f *remoteFixture) check(t *testing.T) (refresh.Outcome, Freshness) {
	t.Helper()
	out, err := f.remote.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("remote Reconcile returned an error; a remote check never fails: %v", err)
	}
	fr, ok := f.kb.Freshness()
	if !ok {
		t.Fatal("no freshness record")
	}
	return out, fr
}

// snapshotTree records every file's content under dir (outside .git), so
// a test can assert a tree is byte-identical afterwards.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRemote_CommitsAreDisplayedFromGitFiles(t *testing.T) {
	f := newRemoteFixture(t, "")
	head := gitT(t, f.dir, "rev-parse", "HEAD")
	fr, _ := f.kb.Freshness()
	if fr.LoadedCommit != head || fr.OnDiskCommit != head {
		t.Errorf("commits after mount = loaded %q, on disk %q; want %s", fr.LoadedCommit, fr.OnDiskCommit, head)
	}
}

func TestRemote_FlagCurrentBehindAndLocalAhead(t *testing.T) {
	f := newRemoteFixture(t, refresh.DivergenceFlag)
	head := gitT(t, f.dir, "rev-parse", "HEAD")

	if _, fr := f.check(t); fr.State != FreshCurrent || fr.Remote != head || fr.Note != "" {
		t.Errorf("in step: %+v, want current at %s", fr, head)
	}

	tip := f.pushPage(t, "notes/zebrafish", "About zebrafish.")
	before := snapshotTree(t, f.dir)
	_, fr := f.check(t)
	if fr.State != FreshBehindRemote || fr.Remote != tip || !fr.Behind {
		t.Errorf("remote ahead: %+v, want behind-remote at %s", fr, tip)
	}
	if !sameTree(before, snapshotTree(t, f.dir)) || gitT(t, f.dir, "rev-parse", "HEAD") != head {
		t.Error("flag mode touched the working tree")
	}
	if got := registrySearchIDs(t, f.reg, "zebrafish"); len(got) != 0 {
		t.Errorf("flag mode made the remote's page searchable: %v", got)
	}

	// Catch up by hand, then commit locally without pushing: the remote's
	// tip is in the local history, so local is ahead, and nothing is due.
	gitT(t, f.dir, "pull", "-q", "--ff-only")
	writeLocalPage(t, f.dir, "notes/local", "A local page.")
	gitT(t, f.dir, "add", ".")
	gitT(t, f.dir, "commit", "-q", "-m", "local")
	if _, fr := f.check(t); fr.State == FreshBehindRemote || fr.Note != noteLocalAhead || fr.Remote != tip {
		t.Errorf("local ahead: %+v, want not behind-remote, noted %q, remote %s", fr, noteLocalAhead, tip)
	}
}

func TestRemote_PullFastForwardsAndRebuilds(t *testing.T) {
	f := newRemoteFixture(t, refresh.DivergencePull)
	tip := f.pushPage(t, "notes/zebrafish", "About zebrafish.")
	out, fr := f.check(t)
	if got := gitT(t, f.dir, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("after pull HEAD = %s, want %s", got, tip)
	}
	if !out.Changed || !strings.Contains(out.Note, "pulled to") {
		t.Errorf("outcome = %+v, want a changed cycle noted as a pull", out)
	}
	if fr.State != FreshCurrent || fr.Remote != tip || fr.LoadedCommit != tip {
		t.Errorf("after pull: %+v, want current with the index built at %s", fr, tip)
	}
	if got := registrySearchIDs(t, f.reg, "zebrafish"); len(got) != 1 {
		t.Errorf("the pulled page is not searchable: %v", got)
	}
}

func TestRemote_PullRefusesADirtyTreeAndLeavesIt(t *testing.T) {
	f := newRemoteFixture(t, refresh.DivergencePull)
	head := gitT(t, f.dir, "rev-parse", "HEAD")
	f.pushPage(t, "notes/zebrafish", "About zebrafish.")
	writeLocalPage(t, f.dir, "notes/stable", "An uncommitted edit to the stable page.")
	before := snapshotTree(t, f.dir)

	out, fr := f.check(t)
	if fr.State != FreshDirty || fr.Note != noteDirty {
		t.Errorf("dirty: %+v, want dirty", fr)
	}
	if !strings.Contains(out.Note, "uncommitted") {
		t.Errorf("log note = %q", out.Note)
	}
	if !sameTree(before, snapshotTree(t, f.dir)) || gitT(t, f.dir, "rev-parse", "HEAD") != head {
		t.Error("a refused pull touched the working tree")
	}
}

func TestRemote_PullRefusesDivergenceAndLeavesTheTree(t *testing.T) {
	f := newRemoteFixture(t, refresh.DivergencePull)
	writeLocalPage(t, f.dir, "notes/local", "A local page.")
	gitT(t, f.dir, "add", ".")
	gitT(t, f.dir, "commit", "-q", "-m", "local")
	local := gitT(t, f.dir, "rev-parse", "HEAD")
	f.pushPage(t, "notes/zebrafish", "About zebrafish.")
	before := snapshotTree(t, f.dir)

	out, fr := f.check(t)
	if fr.State != FreshDiverged || fr.Note != noteDiverged {
		t.Errorf("diverged: %+v, want diverged", fr)
	}
	if !strings.Contains(out.Note, "diverged") {
		t.Errorf("log note = %q", out.Note)
	}
	if !sameTree(before, snapshotTree(t, f.dir)) || gitT(t, f.dir, "rev-parse", "HEAD") != local {
		t.Error("a refused pull touched the working tree")
	}
}

// Everything that stops a check from answering leaves the remote unknown,
// with a fixed note, and never fails or degrades the collection.
func TestRemote_UnknownNeverDegrades(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, f *remoteFixture)
		note  string
	}{
		"unreachable remote": {
			setup: func(t *testing.T, f *remoteFixture) {
				gitT(t, f.dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
			},
			note: noteRemoteFailed,
		},
		"detached HEAD": {
			setup: func(t *testing.T, f *remoteFixture) { gitT(t, f.dir, "checkout", "-q", "--detach") },
			note:  noteDetached,
		},
		"no upstream": {
			setup: func(t *testing.T, f *remoteFixture) { gitT(t, f.dir, "checkout", "-q", "-b", "untracked") },
			note:  noteNoUpstream,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRemoteFixture(t, refresh.DivergencePull)
			tc.setup(t, f)
			out, fr := f.check(t)
			if fr.Remote != RemoteUnknown || fr.Note != tc.note {
				t.Errorf("%+v, want remote unknown noted %q", fr, tc.note)
			}
			if out.Note == "" {
				t.Error("no log note explained the unknown")
			}
			if reason, _ := f.kb.status.degraded(); reason != "" {
				t.Errorf("an unanswerable remote check degraded the collection: %s", reason)
			}
		})
	}
}

// No remote_check, no remote target, no git (MK-FRESH-09).
func TestRemote_NoRemoteCheckNoTarget(t *testing.T) {
	reg, _ := openLocalRefreshPair(t, time.Minute)
	for _, tg := range reg.RefreshTargets() {
		if tg.Key().Kind == refresh.KindRemote {
			t.Fatalf("a remote target without remote_check: %+v", tg.Key())
		}
	}
	if _, err := mustGet(t, reg, "notes").CheckRemote(context.Background()); err == nil {
		t.Error("CheckRemote ran on a collection without remote_check")
	}
}

func TestRemote_TargetRunsOnItsOwnCadence(t *testing.T) {
	f := newRemoteFixture(t, "")
	if got := f.remote.Spec().Every(); got != time.Minute {
		t.Errorf("remote target interval = %s, want the remote_check (1m)", got)
	}
}
