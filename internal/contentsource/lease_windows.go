//go:build windows

package contentsource

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// leaseOpenFlags: Windows has no O_NOFOLLOW. openLeaseFile's Lstat
// before the open and sameLeaseFile after the lock keep a link from
// being used as the lease.
const leaseOpenFlags = 0

// pruneHoldsLockAcrossRename: Windows refuses to rename a directory
// while a file inside it is open, so a prune drops its exclusive lock
// before moving the version aside. A reader that opens the lease file
// in that window keeps the rename from happening (its open handle is
// inside the directory); one that locks after the rename finds the path
// gone and fetches again (see acquireCacheLease).
const pruneHoldsLockAcrossRename = false

// lockLease takes a non-blocking LockFileEx byte-range lock on the
// first byte of f, exclusive or shared. It reports false, with no
// error, when another holder conflicts.
func lockLease(f *os.File, exclusive bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return false, nil
	default:
		return false, err
	}
}

func unlockLease(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
