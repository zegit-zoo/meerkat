package contentsource

import (
	"os"
	"path/filepath"
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
