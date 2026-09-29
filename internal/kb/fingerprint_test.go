package kb

import (
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

// fingerprint_test.go pins the local version token (meerkat-mob#25): it
// moves exactly when the pages ListFS would read could have changed, and
// it covers the same files ListFS reads.

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func page(body string, mod time.Time) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\ntitle: T\n---\n# T\n\n" + body + "\n"), ModTime: mod}
}

func baseFS() fstest.MapFS {
	return fstest.MapFS{
		"content/concepts/alpha.md": page("alpha", t0),
		"content/concepts/beta.md":  page("beta", t0),
	}
}

func mustFingerprint(t *testing.T, fsys fs.FS) string {
	t.Helper()
	fp, err := FingerprintFS(fsys)
	if err != nil {
		t.Fatalf("FingerprintFS: %v", err)
	}
	return fp.Token
}

func TestFingerprintFS_MovesWithEveryPageChange(t *testing.T) {
	base := mustFingerprint(t, baseFS())
	if again := mustFingerprint(t, baseFS()); again != base {
		t.Fatalf("the same tree fingerprints twice as %s and %s", base, again)
	}

	cases := map[string]func(fstest.MapFS){
		"page added":   func(m fstest.MapFS) { m["content/concepts/gamma.md"] = page("gamma", t0) },
		"page deleted": func(m fstest.MapFS) { delete(m, "content/concepts/beta.md") },
		"page renamed": func(m fstest.MapFS) {
			m["content/concepts/beta2.md"] = m["content/concepts/beta.md"]
			delete(m, "content/concepts/beta.md")
		},
		"body resized": func(m fstest.MapFS) { m["content/concepts/beta.md"] = page("beta, longer now", t0) },
		"same size, new mtime": func(m fstest.MapFS) {
			m["content/concepts/beta.md"] = page("BETA", t0.Add(time.Nanosecond))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := baseFS()
			mutate(m)
			if got := mustFingerprint(t, m); got == base {
				t.Errorf("fingerprint did not move (%s)", got)
			}
		})
	}
}

// Files walkPages never yields (not markdown, outside the content root,
// not regular) do not move the token: a change to one cannot change the
// index, so it must not cost a rebuild.
func TestFingerprintFS_IgnoresNonPageFiles(t *testing.T) {
	base := mustFingerprint(t, baseFS())
	m := baseFS()
	m["content/concepts/notes.txt"] = &fstest.MapFile{Data: []byte("not a page"), ModTime: t0}
	m["content/concepts/sub"] = &fstest.MapFile{Mode: fs.ModeDir, ModTime: t0}
	m["outside/elsewhere.md"] = page("outside the content root", t0)
	m["content/concepts/pipe.md"] = &fstest.MapFile{Mode: fs.ModeNamedPipe}
	if got := mustFingerprint(t, m); got != base {
		t.Errorf("a non-page file moved the fingerprint: %s != %s", got, base)
	}
}

// Newest is the most recent page mtime, the input to the settle rule.
func TestFingerprintFS_ReportsTheNewestPage(t *testing.T) {
	m := baseFS()
	m["content/concepts/later.md"] = page("later", t0.Add(time.Hour))
	m["content/concepts/notes.txt"] = &fstest.MapFile{Data: []byte("x"), ModTime: t0.Add(2 * time.Hour)}
	fp, err := FingerprintFS(m)
	if err != nil {
		t.Fatal(err)
	}
	if !fp.Newest.Equal(t0.Add(time.Hour)) {
		t.Errorf("Newest = %s, want the newest PAGE (%s)", fp.Newest, t0.Add(time.Hour))
	}
	if empty, _ := FingerprintFS(fstest.MapFS{}); !empty.Newest.IsZero() {
		t.Errorf("an empty tree has Newest = %s, want zero", empty.Newest)
	}
}

// A missing content root is an empty knowledge base, not an error, for
// the fingerprint exactly as for ListFS.
func TestFingerprintFS_MissingRootIsTheEmptySet(t *testing.T) {
	empty := mustFingerprint(t, fstest.MapFS{})
	if empty == "" {
		t.Fatal("an empty tree has an empty token; a probe could not tell it from a failure")
	}
	if again := mustFingerprint(t, fstest.MapFS{"other/x.md": page("x", t0)}); again != empty {
		t.Errorf("two trees with no content root fingerprint differently: %s, %s", empty, again)
	}
	if pages, err := ListFS(fstest.MapFS{}); err != nil || len(pages) != 0 {
		t.Errorf("ListFS on a missing root = %v, %v; want no pages, no error", pages, err)
	}
}

// vanishingFS reports a subdirectory in its parent's listing and then
// answers "does not exist" when it is read, which is what a live
// directory does when a `git pull` deletes it mid-walk.
type vanishingFS struct {
	fstest.MapFS
	gone string
}

func (v vanishingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == v.gone {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return v.MapFS.ReadDir(name)
}

func (v vanishingFS) Open(name string) (fs.File, error) {
	if name == v.gone {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return v.MapFS.Open(name)
}

// A subdirectory that vanishes mid-walk is skipped. Before, ListFS turned
// its error into "no content root" and returned NO pages, so a rebuild
// that raced a pull would have swapped in an empty index.
func TestListFS_ASubdirectoryVanishingMidWalkIsSkipped(t *testing.T) {
	m := baseFS()
	m["content/doomed/page.md"] = page("doomed", t0)
	v := vanishingFS{MapFS: m, gone: "content/doomed"}

	pages, err := ListFS(v)
	if err != nil {
		t.Fatalf("ListFS: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("ListFS = %d pages, want the 2 that still exist", len(pages))
	}
	if _, err := FingerprintFS(v); err != nil {
		t.Fatalf("FingerprintFS: %v", err)
	}
}
