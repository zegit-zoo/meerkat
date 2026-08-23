package kb

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
//
// index.md here carries frontmatter (id:/title:) rather than being a
// bare heading: since the OKF reserved-filename skip (see
// isReservedArtifact), a frontmatter-less index.md is deliberately no
// longer an ordinary page. This test's index.md is the OTHER case —
// meerkat's own frontmatter-bearing landing page — which is exactly
// what should still load; the frontmatter-less case is covered by
// okf_test.go instead.
func TestLoad_LegitimateIDsStillWorkAfterTraversalFix(t *testing.T) {
	withFS(t, fstest.MapFS{
		"content/index.md":                 {Data: []byte("---\nid: index\ntitle: Index\n---\n# Index\n\nhome\n")},
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

// TestLoad_OversizedPageIsRejected guards the fix that caps how large a
// single page's file may be before loadByPath reads it whole into
// memory. Without a cap, a --kb-dir (a live, operator-writable
// filesystem) containing one very large file was measured driving `mk
// list` to ~2 GB RSS and the search index build (which both `http
// serve` and `mcp serve` run at startup) to ~5 GB RSS / 66s, against a
// single real 1 GiB page.
func TestLoad_OversizedPageIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "content", "huge.md"), strings.Repeat("a", maxPageSize+1))
	withFS(t, os.DirFS(dir))

	_, err := Load("huge")
	if !errors.Is(err, ErrPageTooLarge) {
		t.Fatalf("Load(huge) err = %v, want ErrPageTooLarge", err)
	}
}

// TestLoad_PageAtExactCapStillLoads proves the cap is exclusive: a page
// of exactly maxPageSize bytes must still load; only strictly-larger
// pages are rejected. Guards against an off-by-one that would reject
// legitimate pages sitting right at the boundary.
func TestLoad_PageAtExactCapStillLoads(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "content", "exact.md"), strings.Repeat("a", maxPageSize))
	withFS(t, os.DirFS(dir))

	page, err := Load("exact")
	if err != nil {
		t.Fatalf("Load(exact): %v, want a page of exactly maxPageSize bytes to load", err)
	}
	if len(page.Body) != maxPageSize {
		t.Errorf("Body length = %d, want %d", len(page.Body), maxPageSize)
	}
}

// TestLoad_OversizedPageRejectedEvenIfStatUnderreports proves the
// io.LimitReader in readCapped is the authoritative enforcement, not
// merely a fast path alongside the Stat-based check: even an fs.FS
// whose Stat lies about a small size must still be rejected once actual
// bytes read cross maxPageSize.
func TestLoad_OversizedPageRejectedEvenIfStatUnderreports(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "content", "huge.md"), strings.Repeat("a", maxPageSize+1))
	withFS(t, lyingStatFS{FS: os.DirFS(dir), reportedSize: 1})

	_, err := Load("huge")
	if !errors.Is(err, ErrPageTooLarge) {
		t.Fatalf("Load(huge) err = %v, want ErrPageTooLarge even though Stat under-reports size", err)
	}
}

// lyingStatFS wraps an fs.FS so every file it opens reports
// reportedSize from Stat regardless of its actual content length —
// used to prove readCapped doesn't trust Stat blindly.
type lyingStatFS struct {
	fs.FS
	reportedSize int64
}

func (f lyingStatFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return lyingStatFile{File: file, reportedSize: f.reportedSize}, nil
}

type lyingStatFile struct {
	fs.File
	reportedSize int64
}

func (f lyingStatFile) Stat() (fs.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return lyingFileInfo{FileInfo: info, size: f.reportedSize}, nil
}

type lyingFileInfo struct {
	fs.FileInfo
	size int64
}

func (i lyingFileInfo) Size() int64 { return i.size }

// TestList_OversizedPageIsSkippedWithWarning mirrors
// TestList_UnreadableEntryDoesNotAbortWholeKB: List must skip (not
// abort on) an oversized page — the same "log to stderr and continue"
// path any other loadByPath error already takes — and keep serving
// every other page.
func TestList_OversizedPageIsSkippedWithWarning(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "content", "good.md"), "---\nid: good\ntitle: Good\n---\n# Good\n\nbody\n")
	write(t, filepath.Join(dir, "content", "huge.md"), strings.Repeat("a", maxPageSize+1))
	withFS(t, os.DirFS(dir))

	pages, err := List()
	if err != nil {
		t.Fatalf("List: %v, want nil — an oversized entry must not abort the whole KB", err)
	}
	if len(pages) != 1 || pages[0].ID != "good" {
		t.Fatalf("List = %+v, want exactly the 'good' page", pages)
	}
}

// TestUseFS_ConcurrentReadersAndWriter guards the fix that moved
// currentFS behind atomic.Pointer[fs.FS]. Before the fix, UseFS wrote a
// plain `var currentFS fs.FS` package var — an interface value, i.e. a
// type+data word pair — while List/Load/FS read it with no
// synchronization: a data race on those words that the race detector
// reproduces even though today's only real caller (cobra's
// PersistentPreRunE) writes once before any reader starts. Run with
// `go test -race` to prove it no longer does; there's deliberately no
// assertion on *which* FS a given read lands on (UseFS gives no
// ordering guarantee against concurrent readers), only that no read
// ever observes a torn/invalid fs.FS value. A concurrent Load can of
// course miss a page the writer just flipped away from (it returns
// ErrNotFound, which the readers below ignore); that race is inherent
// to calling UseFS while readers are live and is not what this test is
// guarding against. List itself resolves the active FS exactly once per
// call (see ListFS) and so always walks and reads one consistent
// snapshot.
func TestUseFS_ConcurrentReadersAndWriter(t *testing.T) {
	fsA := fstest.MapFS{"content/a.md": {Data: []byte("---\nid: a\ntitle: A\n---\nbody a\n")}}
	fsB := fstest.MapFS{"content/b.md": {Data: []byte("---\nid: b\ntitle: B\n---\nbody b\n")}}
	t.Cleanup(func() { UseFS(nil) })

	const iterations = 500
	var wg sync.WaitGroup

	// Writer: flips the active FS back and forth while readers are live.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				UseFS(fsA)
			} else {
				UseFS(fsB)
			}
		}
	}()

	// Readers: hammer every currentFS-reading entry point concurrently
	// with the writer above.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = List()
				_, _ = Load("a")
				_, _ = Load("b")
				_ = FS()
			}
		}()
	}

	wg.Wait()
}
