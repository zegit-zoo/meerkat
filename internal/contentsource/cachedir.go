package contentsource

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cachedir.go keeps the on-disk content cache bounded and safe to share
// between processes: a complete entry is never deleted under a reader,
// interrupted staging directories are cleaned up, and old versions of
// one object-store location are pruned after a newer one is installed.

const (
	// stagingPrefix names the sibling directory a cache entry is filled
	// in before it is renamed into place.
	stagingPrefix = ".fetch-"
	// asidePrefix names an entry moved out of the way before deletion,
	// so a half-deleted tree is never visible at a cache path.
	asidePrefix = ".stale-"
)

// staleStagingAge is how old a staging or aside directory must be before
// cleanStaleStaging removes it. Generous, so a slow fill by another live
// process is never mistaken for one a killed process left behind. A var
// so tests can shrink it.
var staleStagingAge = 24 * time.Hour

// DefaultKeepVersions is how many versions of one object-store location
// the cache keeps (the newest first) when cache.keep_versions is unset.
// Two keeps the version being served plus the one it replaced, so a
// process still draining requests on the previous snapshot keeps its
// files.
const DefaultKeepVersions = 2

// cacheKeepVersions is the effective keep count; set from cache.keep_versions
// by ResolveRuntimeCollections.
var cacheKeepVersions = DefaultKeepVersions

// installCacheDir renames the complete staging directory tmpDir to
// cacheDir and reports whether it did. When cacheDir is already a
// complete entry (another process got there first) it leaves it alone,
// returns false, and the caller discards tmpDir. An incomplete leftover
// at cacheDir — never a cache hit — is moved aside and removed first.
func installCacheDir(tmpDir, cacheDir string) (bool, error) {
	if isCacheComplete(cacheDir) {
		return false, nil
	}
	err := os.Rename(tmpDir, cacheDir)
	if err == nil {
		return true, nil
	}
	if isCacheComplete(cacheDir) {
		return false, nil
	}
	if _, serr := os.Lstat(cacheDir); serr != nil {
		return false, fmt.Errorf("finalize cache dir: %w", err)
	}
	aside, aerr := moveAside(cacheDir)
	if aerr != nil {
		return false, fmt.Errorf("finalize cache dir: %w", aerr)
	}
	if isCacheComplete(aside) {
		// A winner landed between the check and the move: put it back
		// and discard ours.
		if rerr := os.Rename(aside, cacheDir); rerr != nil && !isCacheComplete(cacheDir) {
			return false, fmt.Errorf("finalize cache dir: %w", rerr)
		}
		return false, nil
	}
	_ = os.RemoveAll(aside)
	if err := os.Rename(tmpDir, cacheDir); err != nil {
		if isCacheComplete(cacheDir) {
			return false, nil
		}
		return false, fmt.Errorf("finalize cache dir: %w", err)
	}
	return true, nil
}

// moveAside renames dir to a fresh hidden sibling and returns its path.
func moveAside(dir string) (string, error) {
	placeholder, err := os.MkdirTemp(filepath.Dir(dir), asidePrefix+"*")
	if err != nil {
		return "", err
	}
	if err := os.Remove(placeholder); err != nil {
		return "", err
	}
	if err := os.Rename(dir, placeholder); err != nil {
		return "", err
	}
	return placeholder, nil
}

// cleanStaleStaging removes staging and aside directories under parent
// older than staleStagingAge: what a process killed mid-fill leaves.
// Best effort; errors are ignored.
func cleanStaleStaging(parent string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleStagingAge)
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || (!strings.HasPrefix(name, stagingPrefix) && !strings.HasPrefix(name, asidePrefix)) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, name))
	}
}

// pruneCacheVersions keeps the keep newest complete versions in the
// directory holding current (one object-store location's versions) and
// removes the rest, never current itself. Incomplete entries are left
// to installCacheDir. Each pruned entry is moved aside before deletion
// so a half-deleted tree is never visible at a version path.
func pruneCacheVersions(current string, keep int) {
	if keep < 1 {
		keep = 1
	}
	parent := filepath.Dir(current)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	type version struct {
		path string
		mod  time.Time
	}
	var others []version
	for _, e := range entries {
		p := filepath.Join(parent, e.Name())
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || p == current || !isCacheComplete(p) {
			continue
		}
		info, err := os.Stat(filepath.Join(p, completionMarker))
		if err != nil {
			continue
		}
		others = append(others, version{p, info.ModTime()})
	}
	sort.Slice(others, func(i, j int) bool { return others[i].mod.After(others[j].mod) })
	// current counts as one of the kept versions.
	for i, v := range others {
		if i < keep-1 {
			continue
		}
		if aside, err := moveAside(v.path); err == nil {
			_ = os.RemoveAll(aside)
		}
	}
}
