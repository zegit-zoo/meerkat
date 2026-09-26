package contentsource

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain makes every git this package's tests run (the credential
// helper tests, the clone/fetch paths in sync.go) unable to prompt.
//
// The credential-helper tests deliberately ask git for a credential that
// nobody supplies (TestCredentialHelperScript_RefusesNonGitHubHost). With
// no TTY, git then falls back to an askpass program: GIT_ASKPASS, then
// core.askPass, then SSH_ASKPASS — and a desktop session exports that
// last one (ksshaskpass, ssh-askpass-gnome), so `make test` or the
// pre-push race suite opened a password dialog on the developer's screen
// and hung until someone dismissed it.
//
// An empty GIT_ASKPASS stops that fall-through (git treats set-but-empty
// as "no askpass program", skipping core.askPass and SSH_ASKPASS too),
// and GIT_TERMINAL_PROMPT=0 makes git fail the request instead of
// reaching for a terminal. That failure is the outcome these tests
// assert. Set here once, whoever runs the suite and from wherever.
func TestMain(m *testing.M) {
	for key, value := range map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	if err := os.Unsetenv("SSH_ASKPASS"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// TestGitCannotReachAnAskpassProgram pins TestMain's claim against the
// real git binary: with SSH_ASKPASS pointing at a program, as a desktop
// session exports it, a credential request nobody answers must fail
// without ever running that program.
func TestGitCannotReachAnAskpassProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in askpass is a shell script")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "askpass-was-called")
	script := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho called >> '"+marker+"'\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: the test's own executable stand-in.
		t.Fatal(err)
	}
	t.Setenv("SSH_ASKPASS", script)

	cmd := credentialFillCmd(t, "", "protocol=https\nhost=evil.example.com\nusername=x-access-token\n\n")
	if err := cmd.Run(); err == nil {
		t.Error("git credential fill succeeded with nothing to answer it")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran the askpass program: under a desktop session this is a password dialog on the developer's screen")
	}
}
