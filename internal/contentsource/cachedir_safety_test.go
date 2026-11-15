package contentsource

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cachedir_safety_test.go pins the boundaries of the on-disk cache
// lifecycle (meerkat#237): nothing outside one location's directory is
// renamed or removed, a symlink is never followed out of the cache, and
// the tree being served is never removed by an install or a prune.

// symlink makes link point at target, skipping the test where the
// platform or user cannot create symlinks (Windows without the
// privilege).
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// outsideEntry is a complete cache entry outside the cache root: what a
// planted symlink would point at. The returned check fails the test when
// anything in it was touched.
func outsideEntry(t *testing.T) (string, func()) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outside")
	completeEntry(t, dir, "# outside\n")
	return dir, func() {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(dir, "wiki", "a.md"))
		if err != nil || string(got) != "# outside\n" {
			t.Errorf("tree outside the cache root was modified: %q, %v", got, err)
		}
		if !exists(filepath.Join(dir, completionMarker)) {
			t.Error("completion marker outside the cache root was removed")
		}
	}
}

func TestIsCacheComplete_RefusesSymlinks(t *testing.T) {
	outside, _ := outsideEntry(t)
	parent := t.TempDir()

	real := filepath.Join(parent, "real")
	completeEntry(t, real, "# real\n")
	if !isCacheComplete(real) {
		t.Fatal("a real complete entry is not complete")
	}

	linkedDir := filepath.Join(parent, "linked-dir")
	symlink(t, outside, linkedDir)
	if isCacheComplete(linkedDir) {
		t.Error("a symlinked entry pointing at a complete tree outside the cache counts as a cache hit")
	}

	linkedMarker := filepath.Join(parent, "linked-marker")
	if err := os.MkdirAll(linkedMarker, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(outside, completionMarker), filepath.Join(linkedMarker, completionMarker))
	if isCacheComplete(linkedMarker) {
		t.Error("a symlinked completion marker counts as complete")
	}

	dirMarker := filepath.Join(parent, "dir-marker")
	if err := os.MkdirAll(filepath.Join(dirMarker, completionMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if isCacheComplete(dirMarker) {
		t.Error("a directory named like the marker counts as complete")
	}
}

// A symlink planted at a cache path is not served: the fresh fill
// replaces the link itself, and the tree it pointed at is left alone.
func TestPopulateCacheDir_SymlinkAtCachePathIsReplacedNotFollowed(t *testing.T) {
	outside, untouched := outsideEntry(t)
	parent := t.TempDir()
	cacheDir := filepath.Join(parent, "v1")
	symlink(t, outside, cacheDir)

	err := populateCacheDir(cacheDir, "fresh", func(tmpDir string) error {
		return os.WriteFile(filepath.Join(tmpDir, "fresh.md"), []byte("fresh"), 0o644)
	})
	if err != nil {
		t.Fatalf("populateCacheDir: %v", err)
	}
	if isLink(cacheDir) || !isCacheComplete(cacheDir) || !exists(filepath.Join(cacheDir, "fresh.md")) {
		t.Error("the symlink at the cache path was not replaced by a real, complete entry")
	}
	untouched()
	if h := listHidden(t, parent); len(h) != 0 {
		t.Errorf("aside/staging left behind: %v", h)
	}
}

// Pruning considers only real directories beside current: a symlinked
// version (pointing outside) and another location's directory survive,
// and the link itself is not removed or renamed.
func TestPruneCacheVersions_StaysInsideTheLocation(t *testing.T) {
	outside, untouched := outsideEntry(t)
	root := t.TempDir()
	locA := filepath.Join(root, "locA")
	locB := filepath.Join(root, "locB")
	old := time.Now().Add(-48 * time.Hour)

	mkVersion := func(dir string, when time.Time) {
		completeEntry(t, dir, "# v\n")
		if err := os.Chtimes(filepath.Join(dir, completionMarker), when, when); err != nil {
			t.Fatal(err)
		}
	}
	var versions []string
	for i := range 3 {
		d := filepath.Join(locA, fmt.Sprintf("v%d", i))
		mkVersion(d, old.Add(time.Duration(i)*time.Hour))
		versions = append(versions, d)
	}
	otherLocation := filepath.Join(locB, "v0")
	mkVersion(otherLocation, old.Add(-time.Hour))
	link := filepath.Join(locA, "vlink")
	symlink(t, outside, link)
	// A plain file beside the versions is not a version either.
	stray := filepath.Join(locA, "stray")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	current := versions[2]
	pruneCacheVersions(current, 1)

	if !exists(current) {
		t.Error("the current version was pruned")
	}
	for _, d := range versions[:2] {
		if exists(d) {
			t.Errorf("%s survived a keep=1 prune", filepath.Base(d))
		}
	}
	if !isLink(link) {
		t.Error("the symlinked version was removed or renamed")
	}
	if !exists(otherLocation) {
		t.Error("another location's version was pruned")
	}
	if !exists(stray) {
		t.Error("a plain file beside the versions was removed")
	}
	untouched()
	if h := listHidden(t, locA); len(h) != 0 {
		t.Errorf("aside left behind: %v", h)
	}
}

// cleanStaleStaging never follows a symlink named like a staging or
// aside directory, and removing a real stale staging tree removes links
// inside it without following them.
func TestCleanStaleStaging_NeverFollowsSymlinks(t *testing.T) {
	outside, untouched := outsideEntry(t)
	root := t.TempDir()
	parent := filepath.Join(root, "loc")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	// Everything counts as stale, so only the type checks decide.
	prev := staleStagingAge
	staleStagingAge = -time.Hour
	t.Cleanup(func() { staleStagingAge = prev })

	linkFetch := filepath.Join(parent, stagingPrefix+"link")
	symlink(t, outside, linkFetch)
	linkAside := filepath.Join(parent, asidePrefix+"link")
	symlink(t, outside, linkAside)

	stale := filepath.Join(parent, stagingPrefix+"real")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, filepath.Join(stale, "into-outside"))
	symlink(t, filepath.Join(outside, "wiki", "a.md"), filepath.Join(stale, "page.md"))

	sibling := filepath.Join(root, stagingPrefix+"not-in-parent")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	cleanStaleStaging(parent)

	if exists(stale) {
		t.Error("a real stale staging directory survived")
	}
	for _, l := range []string{linkFetch, linkAside} {
		if !isLink(l) {
			t.Errorf("%s: a symlink named like staging was removed", filepath.Base(l))
		}
	}
	if !exists(sibling) {
		t.Error("a staging-named directory outside parent was removed")
	}
	untouched()
}

// The tree a process is serving survives the install of the next
// version under the default keep count, and a prune never removes the
// version being installed even when it is older than its siblings.
func TestInstall_ServedTreeSurvivesTheNextVersion(t *testing.T) {
	loc := t.TempDir()
	served := filepath.Join(loc, "v1")
	if err := populateCacheDir(served, "v1", func(tmpDir string) error {
		return os.WriteFile(filepath.Join(tmpDir, "page.md"), []byte("v1"), 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(served)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	next := filepath.Join(loc, "v2")
	if err := populateCacheDir(next, "v2", func(tmpDir string) error {
		return os.WriteFile(filepath.Join(tmpDir, "page.md"), []byte("v2"), 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	pruneCacheVersions(next, DefaultKeepVersions)

	got, err := r.ReadFile("page.md")
	if err != nil || string(got) != "v1" {
		t.Fatalf("served tree unreadable after the next install: %q, %v", got, err)
	}
	if !isCacheComplete(served) || !isCacheComplete(next) {
		t.Error("an install or prune removed a kept version")
	}
}
