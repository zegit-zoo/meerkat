package contentsource

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- config / validate ---

func TestLoad_MissingFileIsNone(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Content.Type != TypeNone {
		t.Errorf("missing config -> type %q, want none", cfg.Content.Type)
	}
}

func TestLoad_AppliesLayoutDefaults(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ConfigFile), "content:\n  type: local\n  path: ./kb\n")
	cfg, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	l := cfg.Content.Layout
	if l.Wiki != "wiki" || l.Sources != "ingestion/sources.yaml" || l.Prompts != "ingestion/prompts" || l.Templates != "templates" {
		t.Errorf("layout defaults not applied: %+v", l)
	}
}

func TestValidate_Errors(t *testing.T) {
	validSha := strings.Repeat("a", 64)
	cases := map[string]Source{
		"bad type":         {Type: "ftp", Layout: defaultLayout()},
		"local no path":    {Type: TypeLocal, Layout: defaultLayout()},
		"git no repo":      {Type: TypeGit, Ref: "v1", Layout: defaultLayout()},
		"git no ref":       {Type: TypeGit, Repo: "o/r", Layout: defaultLayout()},
		"git bad host":     {Type: TypeGit, Repo: "o/r", Ref: "v1", Host: "bitbucket", Layout: defaultLayout()},
		"submodule none":   {Type: TypeSubmodule, Layout: defaultLayout()},
		"url no url":       {Type: TypeURL, SHA256: validSha, Layout: defaultLayout()},
		"url http scheme":  {Type: TypeURL, URL: "http://example.com/kb.tar.gz", SHA256: validSha, Layout: defaultLayout()},
		"url no scheme":    {Type: TypeURL, URL: "example.com/kb.tar.gz", SHA256: validSha, Layout: defaultLayout()},
		"url no sha256":    {Type: TypeURL, URL: "https://example.com/kb.tar.gz", Layout: defaultLayout()},
		"url short sha256": {Type: TypeURL, URL: "https://example.com/kb.tar.gz", SHA256: "abcd", Layout: defaultLayout()},
		"url non-hex sha256": {
			Type: TypeURL, URL: "https://example.com/kb.tar.gz",
			SHA256: strings.Repeat("g", 64), Layout: defaultLayout(),
		},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if err := s.Validate(); err == nil {
				t.Errorf("expected validation error for %q", name)
			}
		})
	}
}

func TestValidate_OK(t *testing.T) {
	for _, s := range []Source{
		{Type: TypeNone},
		{Type: TypeLocal, Path: "kb", Layout: defaultLayout()},
		{Type: TypeGit, Repo: "o/r", Ref: "v1", Host: "github", Layout: defaultLayout()},
		{Type: TypeSubmodule, Submodule: "kb", Layout: defaultLayout()},
		{Type: TypeURL, URL: "https://example.com/kb.tar.gz", SHA256: strings.Repeat("a", 64), Layout: defaultLayout()},
		// Scheme comparison is case-insensitive (RFC 3986); mixed-case
		// sha256 is tolerated too (isHex64 is case-insensitive).
		{Type: TypeURL, URL: "HTTPS://example.com/kb.tar.gz", SHA256: strings.Repeat("AbCd", 16), Layout: defaultLayout()},
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("type %s: unexpected error %v", s.Type, err)
		}
	}
}

// TestValidate_URL_ErrorMessages checks the specific, actionable wording
// (not just "an error occurred") for each type: url validation failure —
// the sha256 requirement in particular is deliberate (see Source.SHA256's
// doc comment) and its error must say so, not just "field required".
func TestValidate_URL_ErrorMessages(t *testing.T) {
	validSha := strings.Repeat("a", 64)

	err := Source{Type: TypeURL, SHA256: validSha, Layout: defaultLayout()}.Validate()
	if err == nil || !strings.Contains(err.Error(), "content.url") {
		t.Errorf("missing url: err = %v, want it to mention content.url", err)
	}

	err = Source{Type: TypeURL, URL: "http://example.com/kb.tar.gz", SHA256: validSha, Layout: defaultLayout()}.Validate()
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Errorf("http:// url: err = %v, want it to mention https://", err)
	}

	err = Source{Type: TypeURL, URL: "https://example.com/kb.tar.gz", Layout: defaultLayout()}.Validate()
	if err == nil || !strings.Contains(err.Error(), "content.sha256") {
		t.Errorf("missing sha256: err = %v, want it to mention content.sha256", err)
	}

	err = Source{Type: TypeURL, URL: "https://example.com/kb.tar.gz", SHA256: "deadbeef", Layout: defaultLayout()}.Validate()
	if err == nil || !strings.Contains(err.Error(), "64 hex") {
		t.Errorf("short sha256: err = %v, want it to mention the 64-hex-char requirement", err)
	}
}

func TestMovingRef(t *testing.T) {
	pinned := []string{"v1.2.0", "1.2.3", "0a1b2c3", "0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b"}
	moving := []string{"", "main", "develop", "release-candidate"}
	for _, r := range pinned {
		if MovingRef(r) {
			t.Errorf("MovingRef(%q) = true, want false (pinned)", r)
		}
	}
	for _, r := range moving {
		if !MovingRef(r) {
			t.Errorf("MovingRef(%q) = false, want true (moving)", r)
		}
	}
}

// --- sync: none ---

func TestSync_None_NoOp(t *testing.T) {
	root := newRepo(t)
	stale := filepath.Join(root, destContent, "stale.md")
	write(t, stale, "leftover")
	// No content-source.yaml -> none -> must not touch the embed dirs.
	commit, err := Sync(root, io.Discard)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if commit != "none" {
		t.Errorf("commit = %q, want none", commit)
	}
	if !exists(stale) {
		t.Error("none sync must not delete existing content")
	}
	if got := readStamp(t, root); got != "none" {
		t.Errorf("stamp = %q, want none", got)
	}
}

// --- sync: local ---

func TestSync_Local(t *testing.T) {
	root := newRepo(t)
	srcBase := filepath.Join(t.TempDir(), "kb")
	writeFixtureContent(t, srcBase)
	// A stale page that must be cleaned out.
	write(t, filepath.Join(root, destContent, "old", "stale.md"), "stale")

	writeConfig(t, root, "content:\n  type: local\n  path: "+srcBase+"\n")

	commit, err := Sync(root, io.Discard)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !strings.HasPrefix(commit, "local") {
		t.Errorf("commit = %q, want local prefix", commit)
	}
	// wiki copied (with subdirs), stale removed, placeholder kept.
	assertExists(t, filepath.Join(root, destContent, "concepts", "rate-limiting.md"))
	assertExists(t, filepath.Join(root, destContent, "index.md"))
	assertExists(t, filepath.Join(root, destContent, ".gitkeep"))
	if exists(filepath.Join(root, destContent, "old", "stale.md")) {
		t.Error("stale .md should have been cleaned before copy")
	}
	// sources/prompts/templates copied.
	assertExists(t, filepath.Join(root, destEtc, "sources.yaml"))
	assertExists(t, filepath.Join(root, destEtc, "prompts", "general.md"))
	assertExists(t, filepath.Join(root, destEtc, "templates", "default.md"))
	assertExists(t, filepath.Join(root, destEtc, ".gitkeep"))
}

func TestSync_Local_MissingPathErrors(t *testing.T) {
	root := newRepo(t)
	writeConfig(t, root, "content:\n  type: local\n  path: /no/such/dir\n")
	if _, err := Sync(root, io.Discard); err == nil {
		t.Error("expected error for a missing local path")
	}
}

// --- sync: git (local file:// repo, hermetic) ---

func TestSync_Git_LocalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Redirect the content cache to a temp dir on both Linux and macOS.
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)

	// Build a real git repo with fixture content + a tag.
	gitsrc := filepath.Join(t.TempDir(), "kbrepo")
	writeFixtureContent(t, gitsrc)
	gitInit(t, gitsrc)

	root := newRepo(t)
	writeConfig(t, root, "content:\n  type: git\n  repo: file://"+gitsrc+"\n  ref: v0.0.1\n")

	commit, err := Sync(root, io.Discard)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if commit == "" || commit == "git" {
		t.Errorf("expected a resolved commit SHA, got %q", commit)
	}
	assertExists(t, filepath.Join(root, destContent, "concepts", "rate-limiting.md"))
	assertExists(t, filepath.Join(root, destEtc, "sources.yaml"))
	if got := readStamp(t, root); got != commit {
		t.Errorf("stamp %q != returned commit %q", got, commit)
	}
}

// TestSync_Git_AdversarialRefIsInert guards the "--end-of-options"
// hardening before src.Ref in the checkout invocation. Not exploitable
// today — checkout has no --upload-pack option to begin with, so git
// already rejects this as an unknown option — but the fix changes *why*
// it's rejected: with --end-of-options in place, the value can never be
// parsed as a flag at all, regardless of what options checkout gains in
// the future. Either way, Sync must fail cleanly and the marker file
// the ref tries to touch must never be created.
func TestSync_Git_AdversarialRefIsInert(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)

	gitsrc := filepath.Join(t.TempDir(), "kbrepo")
	writeFixtureContent(t, gitsrc)
	gitInit(t, gitsrc)

	marker := filepath.Join(t.TempDir(), "pwned")
	root := newRepo(t)
	cfg := fmt.Sprintf("content:\n  type: git\n  repo: file://%s\n  ref: %q\n",
		gitsrc, "--upload-pack=touch "+marker)
	writeConfig(t, root, cfg)

	if _, err := Sync(root, io.Discard); err == nil {
		t.Fatal("Sync with an --upload-pack=... ref should fail (not a real ref), not silently succeed")
	}
	if exists(marker) {
		t.Fatal("adversarial ref executed a command — the --end-of-options hardening regressed")
	}
}

// TestSync_Git_RefFormsStillWork proves the --end-of-options hardening
// doesn't change resolution for any normal ref shape: a branch name, an
// annotated tag, and a full commit SHA must all still resolve exactly
// as before.
func TestSync_Git_RefFormsStillWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)

	gitsrc := filepath.Join(t.TempDir(), "kbrepo")
	writeFixtureContent(t, gitsrc)
	gitInit(t, gitsrc)
	sha, err := gitOut(gitsrc, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	for _, ref := range []string{"main", "v0.0.1", sha} {
		t.Run(ref, func(t *testing.T) {
			root := newRepo(t)
			cfg := fmt.Sprintf("content:\n  type: git\n  repo: file://%s\n  ref: %q\n", gitsrc, ref)
			writeConfig(t, root, cfg)
			if _, err := Sync(root, io.Discard); err != nil {
				t.Fatalf("Sync ref=%q: %v", ref, err)
			}
			assertExists(t, filepath.Join(root, destContent, "index.md"))
		})
	}
}

// --- sync: submodule (local, hermetic) ---

// TestSync_Submodule_AdversarialPathIsInert guards the "--" hardening
// before src.Submodule in the submodule-update invocation. Verified
// manually against real git: without "--", a Submodule value of
// "--force" is silently parsed as `git submodule update`'s own --force
// flag — which, with no positional path left to restrict it, widens the
// operation to *every* registered submodule — instead of being rejected
// as an unrecognised path. With "--" in place, git must treat it purely
// as a pathspec, and Sync must fail (no path named "--force" is
// registered) rather than silently "succeeding" by force-updating an
// unrelated submodule.
func TestSync_Submodule_AdversarialPathIsInert(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)

	// A real subrepo, registered (via a real `git submodule add`, run
	// directly rather than through the package under test) at path
	// "realsub" in the parent repo that becomes repoRoot below.
	subsrc := filepath.Join(t.TempDir(), "subrepo")
	write(t, filepath.Join(subsrc, "f.txt"), "sub content\n")
	gitInit(t, subsrc)

	root := newRepo(t)
	testGit(t, root, "init", "-q", "-b", "main")
	testGit(t, root, "config", "user.email", "t@e")
	testGit(t, root, "config", "user.name", "t")
	testGit(t, root, "submodule", "add", "-q", subsrc, "realsub")
	testGit(t, root, "commit", "-q", "-m", "add submodule")

	writeConfig(t, root, "content:\n  type: submodule\n  submodule: \"--force\"\n")

	if _, err := Sync(root, io.Discard); err == nil {
		t.Fatal(`Sync with submodule: "--force" should fail (no path named --force is registered) — ` +
			"silent success means it was consumed as a flag instead of a path")
	}

	// The unrelated, legitimately-registered submodule must be
	// unaffected either way — belt-and-braces check that the adversarial
	// value didn't do *anything*, not just that it produced an error.
	if entries, _ := os.ReadDir(filepath.Join(root, "realsub")); len(entries) == 0 {
		t.Error("realsub should still be checked out from the earlier `git submodule add`, untouched by the failed adversarial update")
	}
}

// testGit runs git against dir with the same clean environment
// (cleanGitEnv) the package's own runGit uses, plus a fixed author/
// committer identity — for ad-hoc fixture setup (e.g. registering a
// real submodule) that doesn't fit the newRepo/gitInit helpers.
func testGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture, fixed "git" + literal args.
	cmd.Env = append(cleanGitEnv(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// --- git auth: token never in argv, always scrubbed from the persisted remote ---

// TestCloneURL_NeverCarriesCredentials: the persisted-remote URL is always
// plain — no username, no token — regardless of Host.
func TestCloneURL_NeverCarriesCredentials(t *testing.T) {
	for _, s := range []Source{
		{Repo: "o/r", Host: "github"},
		{Repo: "o/r", Host: "gitlab"},
		{Repo: "o/r"}, // default host
	} {
		got := cloneURL(s)
		if strings.Contains(got, "@") {
			t.Errorf("cloneURL(%+v) = %q, want no embedded credentials", s, got)
		}
	}
	// A full URL/SSH spec in Repo is used verbatim (caller's own auth).
	if got := cloneURL(Source{Repo: "git@host:o/r.git"}); got != "git@host:o/r.git" {
		t.Errorf("cloneURL passthrough = %q", got)
	}
}

// TestAuthClone_GitLab_NeverBorrowsCredentials: token borrowing is
// GitHub-only. host: gitlab must always get the plain tokenless URL and
// no extra git args/env, regardless of any locally cached credentials —
// GitLab access relies on anonymous fetch or the user's own git
// credential configuration, never on meerkat borrowing anything.
func TestAuthClone_GitLab_NeverBorrowsCredentials(t *testing.T) {
	url, args, env := authClone(Source{Repo: "o/r", Host: "gitlab"})
	if url != "https://gitlab.com/o/r.git" {
		t.Errorf("url = %q", url)
	}
	if len(args) != 0 || len(env) != 0 {
		t.Errorf("expected no extra git args/env for gitlab, got args=%v env=%v", args, env)
	}
}

// TestAuthClone_GitHub_NoToken_FallsBackToPlainURL: with no cached gh
// token (the common case once meerkat's own repo is public), authClone
// must still hand back a usable, tokenless URL and no extra git args/env.
func TestAuthClone_GitHub_NoToken_FallsBackToPlainURL(t *testing.T) {
	orig := resolveGitHubToken
	defer func() { resolveGitHubToken = orig }()
	resolveGitHubToken = func() (string, error) {
		return "", errors.New("not logged into github.com")
	}

	url, args, env := authClone(Source{Repo: "o/r", Host: "github"})
	if url != "https://github.com/o/r.git" {
		t.Errorf("url = %q", url)
	}
	if len(args) != 0 || len(env) != 0 {
		t.Errorf("expected no extra git args/env without a cached token, got args=%v env=%v", args, env)
	}
}

// TestAuthClone_GitHub_WithToken_KeepsTokenOutOfURLAndArgv proves the
// actual security property: when a gh token IS cached, it shows up only
// in the returned env slice, never in the URL or the git args slice (the
// two things that end up on argv / in .git/config).
func TestAuthClone_GitHub_WithToken_KeepsTokenOutOfURLAndArgv(t *testing.T) {
	orig := resolveGitHubToken
	defer func() { resolveGitHubToken = orig }()
	resolveGitHubToken = func() (string, error) {
		return "ghs_supersecrettoken", nil
	}

	url, args, env := authClone(Source{Repo: "o/r", Host: "github"})
	if strings.Contains(url, "ghs_supersecrettoken") {
		t.Fatalf("token leaked into URL: %q", url)
	}
	if url != "https://x-access-token@github.com/o/r.git" {
		t.Errorf("url = %q, want a plain (secret-free) username@host form", url)
	}
	for _, a := range args {
		if strings.Contains(a, "ghs_supersecrettoken") {
			t.Fatalf("token leaked into git args (argv): %v", args)
		}
	}
	foundInEnv := false
	for _, e := range env {
		if e == "MEERKAT_GIT_TOKEN=ghs_supersecrettoken" {
			foundInEnv = true
		}
	}
	if !foundInEnv {
		t.Errorf("expected token in env, got %v", env)
	}
}

// credentialFillCmd builds a `git credential fill` invocation against
// credentialHelperScript, hermetically: the leading `-c
// credential.helper=` (empty value) resets any credential helper the
// ambient environment already has configured (a developer's machine
// routinely has one — e.g. `gh auth setup-git`, osxkeychain — and
// without the reset, git composes *every* configured helper, so a test
// asserting "no password came back" could pass for the wrong reason,
// or one asserting a specific password could observe a real cached
// credential instead of our script's output). The second -c adds
// exactly the one helper under test.
func credentialFillCmd(t *testing.T, token, stdin string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cmd := exec.Command("git",
		"-c", "credential.helper=",
		"-c", "credential.helper="+credentialHelperScript,
		"credential", "fill")
	cmd.Env = append(os.Environ(), "MEERKAT_GIT_TOKEN="+token)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd
}

// TestCredentialHelperScript_SuppliesTokenViaEnv is an end-to-end check of
// the actual git plumbing: invoking `git credential fill` with our -c
// credential.helper arg and MEERKAT_GIT_TOKEN in the environment (exactly
// as resolveGit does for clone/fetch) must resolve to the right password
// for the one host/protocol pair authClone ever actually requests,
// proving the mechanism works and that the token only ever needs to travel
// through the environment, never through a command-line argument.
func TestCredentialHelperScript_SuppliesTokenViaEnv(t *testing.T) {
	cmd := credentialFillCmd(t, "ghs_supersecrettoken",
		"protocol=https\nhost=github.com\nusername=x-access-token\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git credential fill: %v", err)
	}
	if !strings.Contains(string(out), "password=ghs_supersecrettoken") {
		t.Errorf("credential fill output = %q, want it to contain the password from env", out)
	}
}

// TestCredentialHelperScript_RefusesNonGitHubHost guards the fix for the
// finding that the old script answered a `get` request for *any* host,
// never inspecting the host=/protocol= fields git provides on stdin. Not
// reachable today (authClone only ever wires the helper up for
// host=github.com, and an explicit URL/SSH Repo bypasses the helper
// entirely) but the script itself must fail closed if that ever changes.
func TestCredentialHelperScript_RefusesNonGitHubHost(t *testing.T) {
	for _, host := range []string{"evil.example.com", "gitlab.com", "github.com.evil.example.com"} {
		t.Run(host, func(t *testing.T) {
			cmd := credentialFillCmd(t, "ghs_supersecrettoken",
				"protocol=https\nhost="+host+"\nusername=x-access-token\n\n")
			out, err := cmd.Output()
			// No other credential source is configured (helper reset, no
			// TTY), so git fails the fill outright when the helper stays
			// silent — that failure *is* the safe outcome. Either way, the
			// token must never appear in stdout.
			if strings.Contains(string(out), "ghs_supersecrettoken") {
				t.Fatalf("host %q: token leaked in output: %q (err=%v)", host, out, err)
			}
		})
	}
}

// TestCredentialHelperScript_RefusesNonHTTPSProtocol guards the protocol
// half of the same check: authClone only ever builds https:// URLs, so
// the helper must not answer for a plain-http request even to
// github.com — answering would hand the token over in cleartext.
func TestCredentialHelperScript_RefusesNonHTTPSProtocol(t *testing.T) {
	cmd := credentialFillCmd(t, "ghs_supersecrettoken",
		"protocol=http\nhost=github.com\nusername=x-access-token\n\n")
	out, _ := cmd.Output()
	if strings.Contains(string(out), "ghs_supersecrettoken") {
		t.Fatalf("token leaked in output for protocol=http: %q", out)
	}
}

// --- working copy + branch derivation ---

func TestResolvedBranch(t *testing.T) {
	nonRepo := t.TempDir() // git calls fail -> fall through
	if b := (Source{Ref: "v1.2.0"}).resolvedBranch(nonRepo); b != "main" {
		t.Errorf("tag ref -> %q, want main", b)
	}
	if b := (Source{Ref: "develop"}).resolvedBranch(nonRepo); b != "develop" {
		t.Errorf("branch-like ref -> %q, want develop", b)
	}
	if b := (Source{Branch: "release", Ref: "v1.0.0"}).resolvedBranch(nonRepo); b != "release" {
		t.Errorf("explicit branch -> %q, want release", b)
	}
}

func TestResolveWorkingCopy_NoneErrors(t *testing.T) {
	if _, err := ResolveWorkingCopy(t.TempDir(), io.Discard); err == nil {
		t.Error("expected error for a none/absent source")
	}
}

func TestResolveWorkingCopy_Local(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "kb")
	writeFixtureContent(t, src)
	writeConfig(t, root, "content:\n  type: local\n  path: "+src+"\n")
	wc, err := ResolveWorkingCopy(root, io.Discard)
	if err != nil {
		t.Fatalf("ResolveWorkingCopy: %v", err)
	}
	if wc.Path != src {
		t.Errorf("path = %q, want %q", wc.Path, src)
	}
	if wc.WikiDir != "wiki" {
		t.Errorf("wikiDir = %q, want wiki", wc.WikiDir)
	}
	if wc.Branch == "" {
		t.Error("branch should not be empty")
	}
}

// --- helpers ---

func newRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, destContent, ".gitkeep"), "")
	write(t, filepath.Join(root, destEtc, ".gitkeep"), "")
	return root
}

func writeFixtureContent(t *testing.T, base string) {
	t.Helper()
	write(t, filepath.Join(base, "wiki", "index.md"), "---\nid: index\ntitle: Index\n---\nhome")
	write(t, filepath.Join(base, "wiki", "concepts", "rate-limiting.md"), "---\nid: concepts/rate-limiting\ntitle: Rate Limiting\ncategory: concepts\n---\nthrottle")
	write(t, filepath.Join(base, "ingestion", "sources.yaml"), "sources: []\n")
	write(t, filepath.Join(base, "ingestion", "prompts", "general.md"), "# Instructions\n")
	write(t, filepath.Join(base, "templates", "default.md"), "---\n---\n")
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cleanGitEnv(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@e")
	run("config", "user.name", "t")
	run("add", ".")
	run("commit", "-q", "-m", "fixture")
	run("tag", "v0.0.1")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	write(t, filepath.Join(root, ConfigFile), body)
}

func readStamp(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, StampFile))
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func assertExists(t *testing.T, p string) {
	t.Helper()
	if !exists(p) {
		t.Errorf("expected file to exist: %s", p)
	}
}
