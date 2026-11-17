//go:build !unix && !windows

package contentsource

import "os"

// On a platform with neither flock nor LockFileEx (none that meerkat is
// released for) a lease is taken but locks nothing: pruning behaves as
// it did before meerkat#240, keeping only the newest keep_versions.

const leaseOpenFlags = 0

const pruneHoldsLockAcrossRename = true

func lockLease(*os.File, bool) (bool, error) { return true, nil }

func unlockLease(*os.File) {}
