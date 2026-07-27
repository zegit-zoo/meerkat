package kbdir

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// TestAdapt_SymlinkEscapeIsBlocked guards the fix in adapt (kbdir.go):
// adapt now builds its fs.FS from os.OpenRoot/Root.FS() instead of
// os.DirFS. os.DirFS only rejects ".." in the path *argument* passed
// to Open — it does nothing to stop a symlink stored *inside* the
// tree from resolving outside it. Before this fix, a single
// `wiki/leak.md -> /etc/passwd`-style symlink committed into (or
// dropped into) a --kb-dir content repo was arbitrary file read via
// `mk show` / kb.Load, and remote arbitrary file read on any exposed
// http/mcp server.
//
// Each subtest below plants a secret file OUTSIDE the kb-dir root and
// a symlink INSIDE it pointing at that secret, then asserts both that
// the read fails AND — the property that actually matters, per the
// task brief — that the secret bytes never come back, through either
// the raw adapter fs.FS or kb.Load wired through Configure exactly as
// the CLI wires it at startup.
func TestAdapt_SymlinkEscapeIsBlocked(t *testing.T) {
	t.Run("absolute_target", func(t *testing.T) {
		root := t.TempDir()
		secretDir := t.TempDir()
		secretFile := filepath.Join(secretDir, "passwd")
		write(t, secretFile, "SECRET-ABS-BYTES")
		write(t, filepath.Join(root, "wiki", "index.md"), "---\nid: index\n---\n# Index\n\nhome\n")

		if err := os.Symlink(secretFile, filepath.Join(root, "wiki", "leak.md")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		assertSymlinkBlocked(t, root, "content/leak.md", "leak", "SECRET-ABS-BYTES")
	})

	t.Run("relative_target_dotdot_escape", func(t *testing.T) {
		root := t.TempDir()
		secretDir := t.TempDir()
		secretFile := filepath.Join(secretDir, "passwd")
		write(t, secretFile, "SECRET-REL-BYTES")
		write(t, filepath.Join(root, "wiki", "index.md"), "---\nid: index\n---\n# Index\n\nhome\n")

		rel, err := filepath.Rel(filepath.Join(root, "wiki"), secretFile)
		if err != nil {
			t.Fatalf("filepath.Rel: %v", err)
		}
		if !strings.HasPrefix(rel, "..") {
			t.Fatalf("test setup bug: relative target %q does not escape via .. as intended", rel)
		}
		if err := os.Symlink(rel, filepath.Join(root, "wiki", "leak-rel.md")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		assertSymlinkBlocked(t, root, "content/leak-rel.md", "leak-rel", "SECRET-REL-BYTES")
	})

	t.Run("symlinked_directory", func(t *testing.T) {
		root := t.TempDir()
		secretDir := t.TempDir()
		write(t, filepath.Join(secretDir, "inside.md"), "SECRET-DIR-BYTES")
		write(t, filepath.Join(root, "wiki", "index.md"), "---\nid: index\n---\n# Index\n\nhome\n")

		if err := os.Symlink(secretDir, filepath.Join(root, "wiki", "linkdir")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		assertSymlinkBlocked(t, root, "content/linkdir/inside.md", "linkdir/inside", "SECRET-DIR-BYTES")
	})
}

// assertSymlinkBlocked checks that neither the raw adapter fs.FS
// (fsPath, embed-style e.g. "content/leak.md") nor kb.Load (pageID,
// e.g. "leak") ever returns secretMarker, and that a legitimate page
// in the same root ("index") still loads — proving the defense isn't
// just "return an error for everything".
func assertSymlinkBlocked(t *testing.T, root, fsPath, pageID, secretMarker string) {
	t.Helper()

	fsys, err := FS(root)
	if err != nil {
		t.Fatalf("FS(%q): %v", root, err)
	}
	if body, err := fs.ReadFile(fsys, fsPath); err == nil || strings.Contains(string(body), secretMarker) {
		t.Fatalf("fs.ReadFile(%q) = body=%q err=%v, want an error and no leaked bytes", fsPath, body, err)
	}

	resetToEmbedded(t)
	if _, err := Configure(root); err != nil {
		t.Fatalf("Configure(%q): %v", root, err)
	}
	page, err := kb.Load(pageID)
	if err == nil {
		t.Fatalf("kb.Load(%q) succeeded, want an error; page=%+v", pageID, page)
	}
	if strings.Contains(page.Body, secretMarker) {
		t.Fatalf("kb.Load(%q) leaked external bytes: %q", pageID, page.Body)
	}

	if _, err := kb.Load("index"); err != nil {
		t.Errorf("legitimate page failed to load after symlink defense: %v", err)
	}
}
