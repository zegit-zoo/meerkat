package contentsource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// lease_test.go pins the reader lease (meerkat#240): a version a
// process still serves is never pruned, however old it is, and a lease
// left by a process that died stops protecting the version.

// leaseVersions writes n complete versions of one location, oldest
// first, with distinct completion-marker mtimes so "newest" is well
// defined on coarse filesystem clocks.
func leaseVersions(t *testing.T, n int) []string {
	t.Helper()
	parent := t.TempDir()
	base := time.Now().Add(-time.Hour)
	dirs := make([]string, 0, n)
	for i := range n {
		d := filepath.Join(parent, fmt.Sprintf("v%d", i))
		completeEntry(t, d, "# v\n")
		when := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(d, completionMarker), when, when); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	return dirs
}

// A reader serving the oldest version keeps it on disk past
// keep_versions; once it lets go, the next prune removes it.
func TestPruneCacheVersions_SkipsALeasedVersion(t *testing.T) {
	dirs := leaseVersions(t, 4)
	lease, err := acquireCacheLease(dirs[0])
	if err != nil || lease == nil {
		t.Fatalf("acquireCacheLease = %v, %v", lease, err)
	}
	pruneCacheVersions(dirs[3], 2)
	if !exists(dirs[0]) {
		t.Fatal("a leased version was pruned while its reader still serves it")
	}
	if exists(dirs[1]) {
		t.Error("an unleased version beyond keep_versions was kept")
	}
	if !exists(dirs[2]) || !exists(dirs[3]) {
		t.Error("the newest two versions must be kept")
	}
	lease.Release()
	if lease.Active() {
		t.Error("lease still active after its last Release")
	}
	pruneCacheVersions(dirs[3], 2)
	if exists(dirs[0]) {
		t.Error("a released version was kept by the next prune")
	}
	if h := listHidden(t, filepath.Dir(dirs[0])); len(h) != 0 {
		t.Errorf("aside left behind: %v", h)
	}
}

// Two readers in one process (two collections on the same source) each
// hold the version; it stays until the last one lets go, and Retain
// counts as a reader.
func TestCacheLease_LastReleaseEndsTheLease(t *testing.T) {
	dirs := leaseVersions(t, 3)
	a, err := acquireCacheLease(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := acquireCacheLease(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	c := a.Retain()
	a.Release()
	b.Release()
	pruneCacheVersions(dirs[2], 1)
	if !exists(dirs[0]) {
		t.Fatal("version pruned while a retained reference was still held")
	}
	c.Release()
	pruneCacheVersions(dirs[2], 1)
	if exists(dirs[0]) {
		t.Error("version kept after every reference was released")
	}
}

// A version that is pruned (or replaced) before the lease is taken is
// not leased: the caller must fetch again rather than serve a path that
// no longer holds that tree.
func TestAcquireCacheLease_RemovedVersionIsLost(t *testing.T) {
	dirs := leaseVersions(t, 1)
	if err := os.RemoveAll(dirs[0]); err != nil {
		t.Fatal(err)
	}
	if l, err := acquireCacheLease(dirs[0]); !errors.Is(err, errLeaseLost) {
		t.Fatalf("acquireCacheLease on a removed version = %v, %v; want errLeaseLost", l, err)
	}
}

// A lease file that is a symlink is never followed: the lease is
// refused, nothing outside the cache root is created or locked, and
// the version stays prunable (no reader can hold it through a link).
func TestAcquireCacheLease_RefusesASymlinkedLeaseFile(t *testing.T) {
	dirs := leaseVersions(t, 3)
	target := filepath.Join(t.TempDir(), "outside-lease")
	symlink(t, target, filepath.Join(dirs[0], leaseFile))
	if l, err := acquireCacheLease(dirs[0]); err == nil {
		l.Release()
		t.Fatal("acquireCacheLease followed a symlinked lease file")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("the symlink's target outside the cache root was created")
	}
	pruneCacheVersions(dirs[2], 1)
	if exists(dirs[0]) {
		t.Error("a version with a symlinked lease file was kept: a planted link must not block pruning")
	}
}
