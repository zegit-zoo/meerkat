package contentsource

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// completeEntry writes a complete cache entry at dir holding one page.
func completeEntry(t *testing.T, dir, page string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wiki", "a.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, completionMarker), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listHidden(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestPopulateCacheDir_NeverReplacesACompleteEntry is the concurrent
// cold start: another process completes the entry while this one is
// filling its staging copy. The winner's directory — which that process
// may already be serving through an os.Root — must survive untouched,
// and the staging copy must be discarded.
func TestPopulateCacheDir_NeverReplacesACompleteEntry(t *testing.T) {
	parent := t.TempDir()
	cacheDir := filepath.Join(parent, "v1")
	var root *os.Root
	err := populateCacheDir(cacheDir, "loser", func(tmpDir string) error {
		completeEntry(t, cacheDir, "# winner\n")
		var err error
		root, err = os.OpenRoot(cacheDir)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(tmpDir, "loser.md"), []byte("x"), 0o644)
	})
	if err != nil {
		t.Fatalf("populateCacheDir: %v", err)
	}
	defer func() { _ = root.Close() }()
	got, err := root.ReadFile("wiki/a.md")
	if err != nil || string(got) != "# winner\n" {
		t.Fatalf("winner's open root reads %q, %v — its directory was replaced", got, err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "loser.md")); err == nil {
		t.Error("the staging copy was installed over a complete entry")
	}
	if h := listHidden(t, parent); len(h) != 0 {
		t.Errorf("staging left behind: %v", h)
	}
}

// An incomplete leftover at the cache path (a crashed run, a manual
// copy) is not a cache hit and is replaced.
func TestPopulateCacheDir_ReplacesAnIncompleteLeftover(t *testing.T) {
	parent := t.TempDir()
	cacheDir := filepath.Join(parent, "v1")
	if err := os.MkdirAll(filepath.Join(cacheDir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "wiki", "old.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := populateCacheDir(cacheDir, "new", func(tmpDir string) error {
		return os.WriteFile(filepath.Join(tmpDir, "new.md"), []byte("new"), 0o644)
	})
	if err != nil {
		t.Fatalf("populateCacheDir: %v", err)
	}
	if !isCacheComplete(cacheDir) || !exists(filepath.Join(cacheDir, "new.md")) || exists(filepath.Join(cacheDir, "wiki", "old.md")) {
		t.Error("the incomplete leftover was not replaced by the new entry")
	}
	if h := listHidden(t, parent); len(h) != 0 {
		t.Errorf("aside/staging left behind: %v", h)
	}
}

// Staging and aside directories a killed process left behind are removed
// once they are old; a fresh one (another live fill) is kept.
func TestCleanStaleStaging(t *testing.T) {
	parent := t.TempDir()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(parent, name)
		if err := os.MkdirAll(filepath.Join(p, "wiki"), 0o755); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldFetch := mk(stagingPrefix+"old", 48*time.Hour)
	oldAside := mk(asidePrefix+"old", 48*time.Hour)
	fresh := mk(stagingPrefix+"fresh", time.Minute)
	version := mk("v1", 48*time.Hour)

	cleanStaleStaging(parent)
	for _, p := range []string{oldFetch, oldAside} {
		if exists(p) {
			t.Errorf("%s survived", filepath.Base(p))
		}
	}
	for _, p := range []string{fresh, version} {
		if !exists(p) {
			t.Errorf("%s was removed", filepath.Base(p))
		}
	}
}

// pruneCacheVersions keeps the newest keep complete versions, counting
// the current one, and never touches the current entry or an incomplete
// one.
func TestPruneCacheVersions(t *testing.T) {
	parent := t.TempDir()
	base := time.Now().Add(-time.Hour)
	var dirs []string
	for i := range 4 {
		d := filepath.Join(parent, fmt.Sprintf("v%d", i))
		completeEntry(t, d, "# v\n")
		when := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(d, completionMarker), when, when); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	incomplete := filepath.Join(parent, "partial")
	if err := os.MkdirAll(incomplete, 0o755); err != nil {
		t.Fatal(err)
	}
	// The current entry is the OLDEST by mtime (a re-fetch of a version
	// that came back): it must still be kept.
	pruneCacheVersions(dirs[0], 2)
	want := map[string]bool{dirs[0]: true, dirs[3]: true, incomplete: true}
	for _, d := range append(dirs, incomplete) {
		if exists(d) != want[d] {
			t.Errorf("%s exists = %v, want %v", filepath.Base(d), exists(d), want[d])
		}
	}
	if h := listHidden(t, parent); len(h) != 0 {
		t.Errorf("aside left behind: %v", h)
	}
}

// End to end: a prefix whose objects keep changing leaves at most
// cacheKeepVersions versions on disk.
func TestFetchS3_PrefixCacheIsPruned(t *testing.T) {
	f := newFakeS3()
	useFakeS3(t, f)
	src := s3Prefix()
	var dirs []string
	for i := range 4 {
		f.put("kb/live/wiki/a.md", []byte(fmt.Sprintf("# A %d\n", i)))
		dir, _, err := FetchS3(context.Background(), src)
		if err != nil {
			t.Fatalf("FetchS3 #%d: %v", i, err)
		}
		// Distinct marker mtimes so "newest" is well defined on coarse
		// filesystem clocks.
		when := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(filepath.Join(dir, completionMarker), when, when); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	entries, err := os.ReadDir(filepath.Dir(dirs[3]))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != DefaultKeepVersions {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("%d entries under the location (%v), want %d", len(entries), names, DefaultKeepVersions)
	}
	if !exists(dirs[3]) || !exists(dirs[2]) {
		t.Error("the newest two versions must be kept")
	}
}

// A configured sha256 is enforced even when an entry for the same
// object version was cached before the pin existed.
func TestFetchS3_PinIsEnforcedOnACachedVersion(t *testing.T) {
	f := newFakeS3()
	useFakeS3(t, f)
	f.put("kb.tar.gz", kbTarGz(t, map[string]string{"wiki/a.md": "# A\n"}))
	src := s3Bundle()
	if _, _, err := FetchS3(context.Background(), src); err != nil {
		t.Fatalf("unpinned fetch: %v", err)
	}
	src.SHA256 = strings.Repeat("0", 64)
	if _, _, err := FetchS3(context.Background(), src); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("pinned fetch over a cached version: err = %v, want sha256 mismatch", err)
	}
}

func TestParseConfig_CacheKeepVersions(t *testing.T) {
	cfg, err := parseConfig([]byte("content: {type: none}\ncache: {}\n"), "t.yaml")
	if err != nil || cfg.Cache.KeepVersions != DefaultKeepVersions {
		t.Fatalf("default keep_versions = %+v, %v", cfg.Cache, err)
	}
	if _, err := parseConfig([]byte("content: {type: none}\ncache: {keep_versions: -1}\n"), "t.yaml"); err == nil || !strings.Contains(err.Error(), "keep_versions") {
		t.Fatalf("negative keep_versions: %v", err)
	}
}

// A type: git build with no user cache directory fails rather than
// cloning into a shared temp directory.
func TestResolveGit_NoUserCacheDirFails(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", "")
	if _, err := os.UserCacheDir(); err == nil {
		t.Skip("this platform resolves a user cache dir without HOME")
	}
	_, _, err := resolveGit(Source{Type: TypeGit, Repo: "file:///nonexistent", Ref: "v1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "user cache directory") {
		t.Fatalf("err = %v, want a refusal naming the user cache directory", err)
	}
}

func TestSanitize_NeverNamesTheCacheOrItsParent(t *testing.T) {
	for in, want := range map[string]string{
		"owner/repo": "owner_repo",
		"..":         "_..",
		".":          "_.",
		"":           "_",
		`a\b`:        "a_b",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// The clone URL comes from content-source.yaml; with "--" before it, a
// value shaped like an option is a repository argument, not a flag.
func TestSync_Git_OptionShapedRepoIsInert(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	marker := filepath.Join(t.TempDir(), "ran")
	var stderr bytes.Buffer
	_, _, err := resolveGit(Source{Type: TypeGit, Repo: "--upload-pack=touch " + marker + " #://x", Ref: "v1"}, &stderr)
	if err == nil {
		t.Fatal("an option-shaped repo cloned successfully")
	}
	if exists(marker) {
		t.Fatal("the repo value ran a command")
	}
	// git must have treated the value as the repository it was asked to
	// clone; parsed as an option, git instead complains about the
	// destination directory.
	if !strings.Contains(stderr.String(), "'--upload-pack=") {
		t.Fatalf("git did not treat the value as the repository: %s", stderr.String())
	}
}
