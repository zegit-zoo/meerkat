//go:build unix

package contentsource

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// leaseOpenFlags refuses a symlink at the lease path at open time.
const leaseOpenFlags = unix.O_NOFOLLOW | unix.O_CLOEXEC

// pruneHoldsLockAcrossRename: on Unix a directory can be renamed while
// a file inside it is open, so a prune keeps the exclusive lock until
// the version has been moved aside and no reader can lock it in the
// meantime.
const pruneHoldsLockAcrossRename = true

// lockLease takes a non-blocking flock on f, exclusive or shared. It
// reports false, with no error, when another holder conflicts.
func lockLease(f *os.File, exclusive bool) (bool, error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

func unlockLease(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
