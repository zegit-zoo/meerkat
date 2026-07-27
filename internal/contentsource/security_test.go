package contentsource

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyMarkdownDir_SymlinkNotDereferenced guards a build-time
// vulnerability: git stores symlinks verbatim (mode 120000), so
// anyone able to land a commit in the content repo could add
// `wiki/leak.md -> /home/runner/.ssh/id_rsa`. copyFile opens by path
// and would happily dereference such a symlink, materialising the
// target's bytes as an ordinary file under internal/kb/content —
// which then ships inside the signed release binary. copyMarkdownDir
// must skip non-regular entries instead of copying through them.
func TestCopyMarkdownDir_SymlinkNotDereferenced(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	write(t, filepath.Join(srcDir, "real.md"), "REAL CONTENT\n")

	secretDir := t.TempDir()
	secretFile := filepath.Join(secretDir, "id_rsa")
	write(t, secretFile, "SECRET EXTERNAL BYTES\n")
	if err := os.Symlink(secretFile, filepath.Join(srcDir, "leak.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	n, err := copyMarkdownDir(srcDir, dstDir)
	if err != nil {
		t.Fatalf("copyMarkdownDir: %v", err)
	}
	if n != 1 {
		t.Errorf("copied count = %d, want 1 (the symlink must be skipped, not counted)", n)
	}

	body, err := os.ReadFile(filepath.Join(dstDir, "real.md"))
	if err != nil {
		t.Fatalf("read copied real.md: %v", err)
	}
	if string(body) != "REAL CONTENT\n" {
		t.Errorf("real.md content = %q, want %q", body, "REAL CONTENT\n")
	}

	leakDst := filepath.Join(dstDir, "leak.md")
	if exists(leakDst) {
		b, _ := os.ReadFile(leakDst)
		t.Fatalf("leak.md must not be created in dst at all, got content %q", b)
	}
}

// TestSync_FailedSyncDoesNotTouchAmbientRepo guards a bug that bit this
// repository twice during development.
//
// resolveGit's deferred remote-scrub used to close over the *named
// return* `root`. Every error path returns `"", "", err`, which zeroes
// the named return before deferred functions run — so the scrub called
// runGit with an empty dir, leaving cmd.Dir unset and executing against
// whatever repository the process was standing in. In production that
// meant running `make sync` from inside any git repo, and having the
// sync fail, silently repointed that repo's origin at the content repo.
func TestSync_FailedSyncDoesNotTouchAmbientRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)

	// An innocent bystander repo that we run from inside.
	ambient := filepath.Join(t.TempDir(), "ambient")
	write(t, filepath.Join(ambient, "README.md"), "# ambient\n")
	gitInit(t, ambient)
	const sentinel = "git@example.invalid:someone/their-own-repo.git"
	if out, err := exec.Command("git", "-C", ambient, "remote", "add", "origin", sentinel).CombinedOutput(); err != nil {
		t.Fatalf("seed ambient origin: %v: %s", err, out)
	}
	t.Chdir(ambient)

	// A sync that is guaranteed to fail: the ref does not exist.
	gitsrc := filepath.Join(t.TempDir(), "kbrepo")
	writeFixtureContent(t, gitsrc)
	gitInit(t, gitsrc)
	root := newRepo(t)
	writeConfig(t, root, fmt.Sprintf(
		"content:\n  type: git\n  repo: file://%s\n  ref: no-such-ref\n", gitsrc))

	if _, err := Sync(root, io.Discard); err == nil {
		t.Fatal("Sync with a nonexistent ref should fail")
	}

	got, err := exec.Command("git", "-C", ambient, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("read ambient origin: %v", err)
	}
	if strings.TrimSpace(string(got)) != sentinel {
		t.Fatalf("failed sync rewrote the ambient repo's origin:\n  got  %q\n  want %q",
			strings.TrimSpace(string(got)), sentinel)
	}
}
