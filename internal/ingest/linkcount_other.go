//go:build !unix

package ingest

import "io/fs"

// linkCount reports 1 where the platform exposes no hard-link count
// through fs.FileInfo; the symlink check in readPage still applies.
func linkCount(fs.FileInfo) uint64 { return 1 }
