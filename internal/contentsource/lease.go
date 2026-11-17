package contentsource

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// lease.go: a reading process leases the cache version it serves, so
// pruneCacheVersions never removes a tree from under it (meerkat#240).
//
// The lease is an OS advisory lock on a lease file inside the version
// directory: flock(2) on Unix, LockFileEx on Windows. A reader holds a
// shared lock for as long as it serves the version; a prune takes a
// non-blocking exclusive lock and skips the version when that fails.
// The kernel drops a lock with the process that held it, so a reader
// that crashed or was killed stops protecting its version at once and
// no stale-lease timeout is needed.
//
// The #237/#238 rules hold for the lease file too: it is looked at with
// Lstat, opened without following a symlink where the platform allows
// it, checked to be a regular file inside the version directory, and
// checked again after locking to be the file at that path.

// leaseFile is the per-version lease file, inside the version
// directory. A dot file, so content enumeration skips it as it skips
// the completion marker, and it is removed with the version.
const leaseFile = ".meerkat-lease"

// errLeaseLost reports that the version was pruned or replaced while
// the lease was being taken: the path no longer holds the tree that was
// asked for, so the caller fetches again instead of serving it.
var errLeaseLost = errors.New("cache version was removed while it was being leased")

// CacheLease is a reader's hold on one cache version: while it is
// active, no meerkat process prunes that version. It is reference
// counted so that every holder of one served tree (a snapshot and the
// snapshots derived from it) can release it independently; the lock is
// dropped with the last reference. A nil *CacheLease is valid and holds
// nothing: it is what a source with no prunable cache (a local
// directory, a url: digest) or a read-only cache gets.
type CacheLease struct {
	mu   sync.Mutex
	refs int
	f    *os.File
}

// LeaseCacheDir leases the complete cache entry dir for the life of the
// returned lease. It returns errLeaseLost (wrapped) when dir is no
// longer a complete entry by the time the lock is held, and a nil lease
// with no error when the cache cannot be written (a read-only volume),
// where no prune can remove the version either.
func LeaseCacheDir(dir string) (*CacheLease, error) {
	return acquireCacheLease(dir)
}

func acquireCacheLease(dir string) (*CacheLease, error) {
	if !isCacheComplete(dir) {
		return nil, fmt.Errorf("lease %s: %w", filepath.Base(dir), errLeaseLost)
	}
	path := filepath.Join(dir, leaseFile)
	f, err := openLeaseFile(path)
	if err != nil {
		if isReadOnly(err) {
			return nil, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			// The directory went away between the check and the open.
			return nil, fmt.Errorf("lease %s: %w", filepath.Base(dir), errLeaseLost)
		}
		return nil, fmt.Errorf("lease %s: %w", filepath.Base(dir), err)
	}
	if ok, err := lockLease(f, false); err != nil || !ok {
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("lease %s: %w", filepath.Base(dir), err)
		}
		// A prune holds the exclusive lock: the version is going away.
		return nil, fmt.Errorf("lease %s: %w", filepath.Base(dir), errLeaseLost)
	}
	// Locked. The version is ours only if the path still holds the
	// complete tree and the file we locked is the one at that path: a
	// prune may have moved the directory aside (and a fresh fetch put
	// the same version back) between the open and the lock.
	if !isCacheComplete(dir) || !sameLeaseFile(f, path) {
		unlockLease(f)
		_ = f.Close()
		return nil, fmt.Errorf("lease %s: %w", filepath.Base(dir), errLeaseLost)
	}
	return &CacheLease{refs: 1, f: f}, nil
}

// openLeaseFile opens (creating it if needed) the lease file at path
// without following a symlink, and refuses anything but a regular file.
// Read-only is enough to lock it on every supported platform.
func openLeaseFile(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("lease file %s is not a regular file", filepath.Base(path))
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|leaseOpenFlags, 0o600) //nolint:gosec // path is inside the cache root, built from a sanitized version segment; never a symlink (Lstat above, O_NOFOLLOW where available, sameLeaseFile after locking)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("lease file %s is not a regular file", filepath.Base(path))
	}
	return f, nil
}

// sameLeaseFile reports whether the open file f is still the regular
// file at path, not a replacement or a link put there since.
func sameLeaseFile(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	now, err := os.Lstat(path)
	return err == nil && now.Mode().IsRegular() && os.SameFile(held, now)
}

// isReadOnly reports whether err means the cache cannot be written.
func isReadOnly(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// Retain adds a reference and returns l, for a holder that serves the
// same tree as an existing one. Nil-safe.
func (l *CacheLease) Retain() *CacheLease {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refs > 0 {
		l.refs++
	}
	return l
}

// Release drops a reference; the last one unlocks and closes the lease
// file, after which the version may be pruned. Extra calls are no-ops.
// Nil-safe.
func (l *CacheLease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refs == 0 {
		return
	}
	l.refs--
	if l.refs > 0 {
		return
	}
	unlockLease(l.f)
	_ = l.f.Close()
	l.f = nil
}

// Active reports whether the lease still holds its version.
func (l *CacheLease) Active() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refs > 0
}

// pruneLock is what pruneCacheVersions holds on a version it is about
// to remove: the exclusive lock, or nothing when the version has no
// usable lease file (then no reader can hold it either).
type pruneLock struct{ f *os.File }

// tryPruneLock reports whether the version at dir may be pruned: no
// live reader leases it. On success the caller must call release, after
// the version has been moved aside where the platform allows it
// (pruneHoldsLockAcrossRename) and before it otherwise.
func tryPruneLock(dir string) (pruneLock, bool) {
	f, err := openLeaseFile(filepath.Join(dir, leaseFile))
	if err != nil {
		// No usable lease file: a symlink or other non-regular entry
		// (readers refuse it, so nobody leases through it), or a cache we
		// cannot write, where the removal fails anyway. Nothing to hold.
		return pruneLock{}, true
	}
	ok, err := lockLease(f, true)
	if err != nil || !ok {
		_ = f.Close()
		return pruneLock{}, false
	}
	return pruneLock{f: f}, true
}

func (p pruneLock) release() {
	if p.f == nil {
		return
	}
	unlockLease(p.f)
	_ = p.f.Close()
}
