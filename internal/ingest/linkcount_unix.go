//go:build unix

package ingest

import (
	"io/fs"
	"syscall"
)

// linkCount is the number of names fi's file has (its hard-link count),
// or 1 when the platform does not report one.
func linkCount(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // Nlink is uint16 on darwin, uint64 on linux.
	}
	return 1
}
