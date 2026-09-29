package kb

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// contentRoot is the directory every page walk starts from: the wiki
// root inside the filesystem UseFS or a collection mounted.
const contentRoot = "content"

// walkPages calls fn for every file under the content root that could be
// a page: a regular file, ending in .md, not on the excluded list. It is
// the ONE enumeration ListFS and FingerprintFS share, so "the pages this
// index was built from" and "the files the version token covers" can
// never be two different sets.
//
// # Entries that vanish mid-walk
//
// A live directory moves while it is walked: a `git pull` or a branch
// switch in a knowledge-base checkout deletes directories. A
// subdirectory that disappears between being listed and being read
// reports fs.ErrNotExist, and that is skipped here like any other
// vanished entry. Before timer-driven reloads existed that error
// escaped the walk, and ListFS turned it into "no content root at all",
// an EMPTY page list. A rebuild racing a pull would then have served an
// empty index for one interval. Only the content root itself being
// absent still means "no pages".
func walkPages(fsys fs.FS, fn func(p string, d fs.DirEntry) error) error {
	return fs.WalkDir(fsys, contentRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != contentRoot && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip anything that isn't a regular file. With a runtime
		// --kb-dir this walks a live filesystem, so the tree can contain
		// FIFOs, sockets and device nodes. Opening a FIFO blocks until a
		// writer appears, which never happens — one stray pipe under
		// wiki/ would otherwise hang `mk list` and stop `http serve` and
		// `mcp serve` from ever binding, with no diagnostic.
		if !d.Type().IsRegular() {
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		if isExcluded(p) {
			return nil
		}
		return fn(p, d)
	})
}

// Fingerprint is the version token of a page set, plus the newest page
// modification time it saw.
type Fingerprint struct {
	// Token is a SHA-256 over every candidate page's (path, size,
	// modification time), in walk order, which is lexical and therefore
	// stable.
	Token string
	// Newest is the most recent modification time among those pages, or
	// zero when there are none. A caller uses it to tell whether the
	// token may be unsettled (see FingerprintFS).
	Newest time.Time
}

// FingerprintFS fingerprints the pages ListFS would consider in fsys.
//
// It is the cheap probe for a `type: local` collection with a
// `refresh:` block (meerkat-mob#25, MK-FRESH-01/02). It costs one stat per
// page and reads no page content. It is the same shape as the object
// stores' listing fingerprint (internal/contentsource). Adding, deleting,
// renaming or resizing a page changes it, and so does rewriting one once
// the clock has moved on. It reads no git metadata and runs nothing, so it
// works the same for a directory that is not a git repository
// (MK-SEC-12).
//
// It covers every candidate file walkPages yields, including the few
// ListFS then skips after reading them (an OKF navigation artifact, an
// unparseable page). A change to one of those costs a spare rebuild,
// never a missed one.
//
// # What it cannot see
//
// A file's modification time comes from the kernel's coarse clock, which
// ticks every few milliseconds, and some filesystems (FAT, several network
// filesystems) store it to the second or coarser. Two same-size writes
// inside one tick therefore leave the token unchanged. Newest lets the
// caller close most of that gap: a token taken while a page is younger
// than the timestamp granularity is unsettled, and the next probe should
// rebuild regardless (git's "racy clean" rule; see
// internal/collections' settleWindow). An mtime-preserving copy onto a
// same-length file stays invisible. A restart rebuilds regardless.
//
// A missing content root fingerprints as the empty set, matching ListFS,
// which serves it as zero pages.
func FingerprintFS(fsys fs.FS) (Fingerprint, error) {
	h := sha256.New()
	var newest time.Time
	err := walkPages(fsys, func(p string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // deleted between the listing and the stat
			}
			return err
		}
		mod := info.ModTime()
		if mod.After(newest) {
			newest = mod
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", p, info.Size(), mod.UnixNano())
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Fingerprint{}, fmt.Errorf("fingerprint content: %w", err)
	}
	return Fingerprint{Token: hex.EncodeToString(h.Sum(nil))[:32], Newest: newest}, nil
}

// FingerprintGlobal is FingerprintFS over the process-wide filesystem.
func FingerprintGlobal() (Fingerprint, error) { return FingerprintFS(loadFS()) }
