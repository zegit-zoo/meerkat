package gitinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitinfo_test.go builds real repositories with the git binary and checks
// what this package reads from their files, and what the hardened runner
// does. Every helper call runs git with no global or system config, so a
// developer's signing or credential setup cannot hang or skew a test.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// git runs a helper git command in dir and returns its trimmed stdout.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
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

// newRepoWithCommit makes a repository on branch main with one commit.
func newRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, "wiki", "a.md"), "# A\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "one")
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// testRunner is DefaultRunner with the file transport allowed, so tests
// can use bare repositories on disk as remotes.
func testRunner() Runner {
	r := DefaultRunner
	r.AllowProtocols = "file"
	r.RemoteTimeout, r.PullTimeout = 20*time.Second, 20*time.Second
	return r
}

func TestFind_FromTheRootASubdirectoryAndNowhere(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	for _, from := range []string{dir, filepath.Join(dir, "wiki")} {
		r, err := Find(from)
		if err != nil {
			t.Fatalf("Find(%s): %v", from, err)
		}
		if got, _ := filepath.EvalSymlinks(r.Root); got != mustEval(t, dir) {
			t.Errorf("Root = %s, want %s", r.Root, dir)
		}
	}
	if _, err := Find(t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Errorf("Find(no repo) = %v, want ErrNotRepo", err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHead_AttachedDetachedAndPacked(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	want := git(t, dir, "rev-parse", "HEAD")
	r, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	commit, branch, err := r.Head()
	if err != nil || commit != want || branch != "main" {
		t.Fatalf("Head = %s, %s, %v; want %s on main", commit, branch, err, want)
	}

	git(t, dir, "pack-refs", "--all")
	if _, err := os.Stat(filepath.Join(dir, ".git", "refs", "heads", "main")); err == nil {
		t.Fatal("precondition: the loose ref survived pack-refs")
	}
	if commit, _, err := r.Head(); err != nil || commit != want {
		t.Errorf("Head from packed-refs = %s, %v; want %s", commit, err, want)
	}

	git(t, dir, "checkout", "-q", "--detach")
	if commit, _, err := r.Head(); !errors.Is(err, ErrDetached) || commit != want {
		t.Errorf("detached Head = %s, %v; want %s and ErrDetached", commit, err, want)
	}
}

// A linked worktree has a `.git` FILE and shares refs through commondir.
func TestFind_LinkedWorktree(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, dir, "worktree", "add", "-q", "-b", "side", wt)
	r, err := Find(wt)
	if err != nil {
		t.Fatalf("Find(worktree): %v", err)
	}
	commit, branch, err := r.Head()
	if err != nil || branch != "side" || commit != git(t, wt, "rev-parse", "HEAD") {
		t.Errorf("worktree Head = %s, %s, %v", commit, branch, err)
	}
}

// A HEAD that names a ref outside refs/ is refused, never read.
func TestHead_RefusesARefThatEscapes(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/../../../etc/passwd\n")
	r, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Head(); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("Head = %v, want the ref refused", err)
	}
}

// The remote URL is the configured one, verbatim: an insteadOf rewrite in
// the knowledge base's own config is not applied (review of #25 part B).
func TestUpstream_IsReadVerbatimWithNoInsteadOf(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	git(t, dir, "remote", "add", "origin", "https://real.example/kb.git")
	git(t, dir, "config", "branch.main.remote", "origin")
	git(t, dir, "config", "branch.main.merge", "refs/heads/main")
	git(t, dir, "config", "url.https://evil.example/.insteadOf", "https://real.example/")
	r, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.Upstream("main")
	if err != nil {
		t.Fatal(err)
	}
	if up != (Upstream{Remote: "origin", URL: "https://real.example/kb.git", Branch: "main"}) {
		t.Errorf("Upstream = %+v", up)
	}
	if _, err := r.Upstream("nope"); !errors.Is(err, ErrNoUpstream) {
		t.Errorf("Upstream(untracked branch) = %v, want ErrNoUpstream", err)
	}
}

func TestParseConfig_Shapes(t *testing.T) {
	cfg := parseConfig(`# comment
[Branch "Main"]
	Remote = origin ; trailing
	merge = "refs/heads/Main"
[remote.legacy]
	url = x
[include]
	path = elsewhere
[core]
	bare
	quoted = "a # not a comment \" end"
`)
	for _, tc := range []struct{ sec, sub, key, want string }{
		{"branch", "Main", "remote", "origin"},
		{"BRANCH", "Main", "MERGE", "refs/heads/Main"},
		{"branch", "main", "remote", ""}, // a subsection is case-sensitive
		{"remote", "legacy", "url", "x"},
		{"core", "", "bare", "true"},
		{"core", "", "quoted", `a # not a comment " end`},
	} {
		if got := cfg.get(tc.sec, tc.sub, tc.key); got != tc.want {
			t.Errorf("get(%s, %s, %s) = %q, want %q", tc.sec, tc.sub, tc.key, got, tc.want)
		}
	}
}

func TestHasObject_LoosePackedAbsentAndCapped(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	dir := newRepoWithCommit(t)
	head := git(t, dir, "rev-parse", "HEAD")
	r, err := Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.HasObject(ctx, head, DefaultLimits); !ok || err != nil {
		t.Fatalf("loose: HasObject = %v, %v", ok, err)
	}
	git(t, dir, "gc", "-q")
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", head[:2], head[2:])); err == nil {
		t.Fatal("precondition: the object is still loose after gc")
	}
	if ok, err := r.HasObject(ctx, head, DefaultLimits); !ok || err != nil {
		t.Fatalf("packed: HasObject = %v, %v", ok, err)
	}
	absent := strings.Repeat("0", 39) + "1"
	if ok, err := r.HasObject(ctx, absent, DefaultLimits); ok || err != nil {
		t.Errorf("absent: HasObject = %v, %v; want false, nil", ok, err)
	}
	if _, err := r.HasObject(ctx, absent, Limits{MaxPacks: 0, Budget: time.Second}); !errors.Is(err, ErrCapped) {
		t.Errorf("over MaxPacks: err = %v, want ErrCapped", err)
	}
	writeFile(t, filepath.Join(dir, ".git", "objects", "info", "alternates"), "/elsewhere\n")
	if _, err := r.HasObject(ctx, absent, DefaultLimits); !errors.Is(err, ErrCapped) {
		t.Errorf("with alternates: err = %v, want ErrCapped (not knowable)", err)
	}
}

// bareWithBranch makes a bare repository with main at a fresh commit, and
// returns its path and that commit.
func bareWithBranch(t *testing.T, content string) (string, string) {
	t.Helper()
	src := t.TempDir()
	git(t, src, "init", "-q")
	writeFile(t, filepath.Join(src, "f.md"), content)
	git(t, src, "add", ".")
	git(t, src, "commit", "-q", "-m", content)
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, src, "clone", "-q", "--bare", src, bare)
	return bare, git(t, src, "rev-parse", "HEAD")
}

func TestLsRemote_ReadsTheTipAndIgnoresTheKBsConfig(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	realRemote, realTip := bareWithBranch(t, "real")
	evilRemote, _ := bareWithBranch(t, "evil")
	tip, err := testRunner().LsRemote(ctx, realRemote, "main")
	if err != nil || tip != realTip {
		t.Fatalf("LsRemote = %s, %v; want %s", tip, err, realTip)
	}

	// A knowledge base whose own config rewrites the real remote to
	// another one. The URL meerkat reads is the real one, and ls-remote
	// runs outside the KB, so the rewrite never applies.
	kb := newRepoWithCommit(t)
	git(t, kb, "remote", "add", "origin", realRemote)
	git(t, kb, "config", "branch.main.remote", "origin")
	git(t, kb, "config", "branch.main.merge", "refs/heads/main")
	git(t, kb, "config", "url."+evilRemote+".insteadOf", realRemote)
	r, err := Find(kb)
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.Upstream("main")
	if err != nil {
		t.Fatal(err)
	}
	if tip, err := testRunner().LsRemote(ctx, up.URL, up.Branch); err != nil || tip != realTip {
		t.Errorf("LsRemote(the KB's upstream) = %s, %v; want the REAL remote's %s", tip, err, realTip)
	}
	if _, err := testRunner().LsRemote(ctx, realRemote, "no-such-branch"); err == nil {
		t.Error("a missing remote branch was not an error")
	}
}

// The transports are pinned, and a URL can never be read as an option.
func TestLsRemote_RefusesExtAndOptionShapedURLs(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "pwned")
	for _, url := range []string{
		"ext::sh -c touch% " + marker,
		"--upload-pack=touch " + marker,
	} {
		if _, err := DefaultRunner.LsRemote(ctx, url, "main"); err == nil {
			t.Errorf("LsRemote(%q) succeeded", url)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("LsRemote(%q) ran a command", url)
		}
	}
}

func TestClean_TrackedChangesOnly(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	dir := newRepoWithCommit(t)
	if ok, err := testRunner().Clean(ctx, dir); !ok || err != nil {
		t.Fatalf("fresh clone: Clean = %v, %v", ok, err)
	}
	writeFile(t, filepath.Join(dir, "scratch.txt"), "untracked\n")
	if ok, err := testRunner().Clean(ctx, dir); !ok || err != nil {
		t.Errorf("untracked file: Clean = %v, %v; want clean", ok, err)
	}
	writeFile(t, filepath.Join(dir, "wiki", "a.md"), "# A, edited\n")
	if ok, err := testRunner().Clean(ctx, dir); ok || err != nil {
		t.Errorf("tracked edit: Clean = %v, %v; want dirty", ok, err)
	}
}

// cloneOf clones bare into a fresh working tree tracking main.
func cloneOf(t *testing.T, bare string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, t.TempDir(), "clone", "-q", bare, wt)
	return wt
}

// pushNewCommit advances bare's main by one commit made in a scratch clone.
func pushNewCommit(t *testing.T, bare, file, content string) string {
	t.Helper()
	wt := cloneOf(t, bare)
	writeFile(t, filepath.Join(wt, file), content)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", content)
	git(t, wt, "push", "-q", "origin", "main")
	return git(t, wt, "rev-parse", "HEAD")
}

func TestPullFFOnly_FastForwardsAndRefusesDivergence(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	bare, _ := bareWithBranch(t, "v1")
	kb := cloneOf(t, bare)
	newTip := pushNewCommit(t, bare, "g.md", "v2")

	if err := testRunner().PullFFOnly(ctx, kb, "origin", "main"); err != nil {
		t.Fatalf("PullFFOnly: %v", err)
	}
	if got := git(t, kb, "rev-parse", "HEAD"); got != newTip {
		t.Fatalf("after pull HEAD = %s, want %s", got, newTip)
	}

	// Diverge: a local commit, and another remote one.
	writeFile(t, filepath.Join(kb, "local.md"), "local\n")
	git(t, kb, "add", ".")
	git(t, kb, "commit", "-q", "-m", "local")
	localHead := git(t, kb, "rev-parse", "HEAD")
	pushNewCommit(t, bare, "h.md", "v3")
	err := testRunner().PullFFOnly(ctx, kb, "origin", "main")
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("diverged PullFFOnly = %v, want ErrNotFastForward", err)
	}
	if got := git(t, kb, "rev-parse", "HEAD"); got != localHead {
		t.Errorf("a refused pull moved HEAD: %s, want %s", got, localHead)
	}
	if out := git(t, kb, "status", "--porcelain"); out != "" {
		t.Errorf("a refused pull left the tree dirty:\n%s", out)
	}
}

// Hooks do not run during a pull. OPT-IN (MEERKAT_TEST_EXEC=1): proving
// it plants an executable hook, which, if the protection ever failed,
// git would execute: the "drop and execute" pattern a watched CI runner
// alerts on.
func TestPullFFOnly_RunsNoHooks(t *testing.T) {
	if os.Getenv("MEERKAT_TEST_EXEC") != "1" {
		t.Skip("plants an executable hook; set MEERKAT_TEST_EXEC=1 to run")
	}
	requireGit(t)
	bare, _ := bareWithBranch(t, "v1")
	kb := cloneOf(t, bare)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(kb, ".git", "hooks", "post-merge")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil { //nolint:gosec // G306: the hook must be executable to prove it is not run.
		t.Fatal(err)
	}
	pushNewCommit(t, bare, "g.md", "v2")
	if err := testRunner().PullFFOnly(context.Background(), kb, "origin", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the post-merge hook ran during a pull")
	}
}
