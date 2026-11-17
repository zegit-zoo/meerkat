package contentsource

import "errors"

// lease.go: a reading process leases the cache version it serves, so
// pruneCacheVersions never removes a tree from under it (meerkat#240).
//
// Stub: the API the tests need, with no lock behind it yet.

// leaseFile is the per-version lease file, inside the version directory.
const leaseFile = ".meerkat-lease"

// errLeaseLost reports that the version was pruned or replaced while
// the lease was being taken.
var errLeaseLost = errors.New("cache version was removed while it was being leased")

// CacheLease is a reader's hold on one cache version.
type CacheLease struct{}

// LeaseCacheDir leases the complete cache entry dir.
func LeaseCacheDir(dir string) (*CacheLease, error) { return &CacheLease{}, nil }

func acquireCacheLease(dir string) (*CacheLease, error) { return LeaseCacheDir(dir) }

// Retain adds a reference and returns l.
func (l *CacheLease) Retain() *CacheLease { return l }

// Release drops a reference.
func (l *CacheLease) Release() {}

// Active reports whether the lease is still held.
func (l *CacheLease) Active() bool { return l != nil }
