package kb

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// withFS points List/Load at fsys for the duration of the test and
// restores the embedded default afterward — the same global-state
// convention internal/kbdir/kbdir_test.go documents and uses around
// Configure, applied directly to the exported UseFS entry point these
// tests exercise (List/Load themselves are what's under test here, so
// pointing UseFS at a fixture is the mechanism under test, not
// something to avoid).
func withFS(t *testing.T, fsys fs.FS) {
	t.Helper()
	UseFS(fsys)
	t.Cleanup(func() { UseFS(nil) })
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

// TestLoad_PageIDTraversalIsBlocked guards the fix in Load:
// path.Join("content", id+".md") cleans "..", so an id of
// "../etc/prompts/x" used to collapse to "etc/prompts/x" — exactly
// cancelling the "content" segment and escaping the wiki subtree into
// whatever the same backing FS serves under "etc/" (the ingestion
// prompts/templates, when the backing FS is a --kb-dir; see
// internal/kbdir's adapter, which maps both "content/..." and
// "etc/..." onto the same content-repo root so this is directly
// reachable in a real deployment). Load now re-asserts the
// "content/" prefix on the joined path rather than trusting a
// well-formed id to keep it.
func TestLoad_PageIDTraversalIsBlocked(t *testing.T) {
	withFS(t, fstest.MapFS{
		"content/index.md":         {Data: []byte("# Index\n\nhome\n")},
		"etc/prompts/general.md":   {Data: []byte("SECRET PROMPT BODY")},
		"etc/templates/default.md": {Data: []byte("SECRET TEMPLATE BODY")},
	})

	for _, id := range []string{"../etc/prompts/general", "../etc/templates/default"} {
		t.Run(id, func(t *testing.T) {
			page, err := Load(id)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Load(%q) err = %v, want ErrNotFound", id, err)
			}
			if strings.Contains(page.Body, "SECRET") {
				t.Fatalf("Load(%q) leaked body: %q", id, page.Body)
			}
		})
	}
}

// TestLoad_LegitimateIDsStillWorkAfterTraversalFix makes sure the
// content/-prefix re-assertion in Load isn't over-broad: nested paths
// and ids containing dots (both look superficially "suspicious" but
// are completely ordinary wiki ids) must keep working.
func TestLoad_LegitimateIDsStillWorkAfterTraversalFix(t *testing.T) {
	withFS(t, fstest.MapFS{
		"content/index.md":                 {Data: []byte("# Index\n\nhome\n")},
		"content/concepts/widgets.md":      {Data: []byte("# Widgets\n\nround\n")},
		"content/2024.10-release-notes.md": {Data: []byte("# Notes\n\nshipped\n")},
	})

	for _, id := range []string{"index", "concepts/widgets", "2024.10-release-notes"} {
		t.Run(id, func(t *testing.T) {
			if _, err := Load(id); err != nil {
				t.Fatalf("Load(%q): %v", id, err)
			}
		})
	}
}

// TestList_NonRegularFileDoesNotHang guards the fix that makes List
// skip non-regular directory entries. A --kb-dir walks a live
// filesystem, so it can contain FIFOs, sockets or device nodes.
// Opening a FIFO for reading blocks until a writer connects, which
// never happens during a directory walk — before the fix, one stray
// pipe under wiki/ hung `mk list` and stopped `http serve`/`mcp serve`
// from ever binding, with no diagnostic. The goroutine+timeout below
// turns a regression into a fast, clear test failure instead of an
// indefinite CI hang.
func TestList_NonRegularFileDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("syscall.Mkfifo is not available on windows")
	}

	dir := t.TempDir()
	write(t, filepath.Join(dir, "content", "real.md"), "---\nid: real\ntitle: Real\n---\n# Real\n\nbody\n")
	fifoPath := filepath.Join(dir, "content", "pipe.md")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	withFS(t, os.DirFS(dir))

	type result struct {
		pages []Page
		err   error
	}
	done := make(chan result, 1)
	go func() {
		pages, err := List()
		done <- result{pages, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("List: %v", res.err)
		}
		if len(res.pages) != 1 || res.pages[0].ID != "real" {
			t.Fatalf("List = %+v, want exactly the 'real' page", res.pages)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("List() hung reading a non-regular file (FIFO) — the non-regular-file skip in List has regressed")
	}
}

// TestList_UnreadableEntryDoesNotAbortWholeKB guards the fix that
// makes List skip (log-and-continue) an entry it can't read instead
// of aborting the walk. A broken symlink, a permission error or a
// symlink loop used to make fs.WalkDir return that error straight out
// of List, so a single bad file denied every other page and prevented
// the http/mcp servers from starting.
func TestList_UnreadableEntryDoesNotAbortWholeKB(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions are not enforced")
	}

	dir := t.TempDir()
	write(t, filepath.Join(dir, "content", "good.md"), "---\nid: good\ntitle: Good\n---\n# Good\n\nbody\n")
	badPath := filepath.Join(dir, "content", "bad.md")
	write(t, badPath, "nobody should be able to read this\n")
	if err := os.Chmod(badPath, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(badPath, 0o600) })

	withFS(t, os.DirFS(dir))

	pages, err := List()
	if err != nil {
		t.Fatalf("List: %v, want nil — one unreadable entry must not abort the whole KB", err)
	}
	if len(pages) != 1 || pages[0].ID != "good" {
		t.Fatalf("List = %+v, want exactly the 'good' page", pages)
	}
}
