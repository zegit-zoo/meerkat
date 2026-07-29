package contentsource

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Extraction hardening limits for a type: url content archive. These are
// `var`, not `const`, so tests can shrink them to exercise the cap logic
// through the real gzip/tar path at a tiny, deterministic scale instead
// of pushing large payloads through the race detector on every run —
// mirrors internal/update/download.go's maxExtractedArchiveBytes.
var (
	// maxExtractEntries caps the number of tar entries an archive may
	// contain, so a huge number of tiny (or zero-byte) entries can't
	// exhaust inodes/file handles or just waste CPU walking the archive.
	maxExtractEntries = 100_000

	// maxExtractedFileBytes caps the uncompressed size of any single
	// entry. Generously above internal/kb's own 8 MiB per-page cap (this
	// runs before that check ever sees the content, and also covers
	// sources.yaml/prompts/templates) while still bounding a single
	// hostile entry.
	maxExtractedFileBytes int64 = 64 << 20 // 64 MiB

	// maxExtractedTotalBytes caps the *cumulative* uncompressed bytes
	// read from the archive across every entry. Without this, a single
	// entry that declares a huge size (fed through gzip, which can
	// inflate at very high ratios) forces us to decompress an
	// attacker-chosen number of bytes before we ever finish — the
	// classic decompression-bomb shape. See cumulativeLimitReader.
	maxExtractedTotalBytes int64 = 4 << 30 // 4 GiB
)

// errExtractLimitExceeded is cumulativeLimitReader's sentinel Read error
// once its budget is exhausted.
var errExtractLimitExceeded = errors.New("cumulative uncompressed archive size limit exceeded")

// cumulativeLimitReader wraps r and enforces a total byte budget across
// all reads, no matter how many logical tar entries (matched, skipped,
// or partially read) those bytes are attributed to. Unlike
// io.LimitReader — which turns "budget exhausted" into a quiet io.EOF,
// indistinguishable from a legitimately short archive — it also records
// that the budget was the reason reading stopped, via exceeded, so the
// caller can fail loudly and specifically regardless of how
// archive/tar's internals wrap or rewrap the Read error on its way back
// out of tr.Next() / io.Copy.
//
// This is the same approach as internal/update/download.go's
// cumulativeLimitReader of the same name, reimplemented here rather than
// imported: internal/update is a separate, self-update-specific package
// outside this change's file-ownership boundary, and the type is small
// enough that duplicating the (already-proven) approach is clearer than
// contorting either package's API to share it.
type cumulativeLimitReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (c *cumulativeLimitReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		c.exceeded = true
		return 0, errExtractLimitExceeded
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// extractTarGz extracts the tar.gz archive at tarballPath into destDir,
// which must already exist (an empty, freshly-created temp directory —
// see FetchURL, which renames it into the content cache only after this
// returns successfully).
//
// This is the highest-risk code in the content-acquisition path: the
// archive's bytes are attacker-influenceable up to matching a pinned
// sha256 (an operator names content.url; whoever controls that host or
// an on-path network position controls the archive up to the digest
// collision bound), so every entry is treated as hostile.
//
// SECURITY:
//   - Every write goes through an os.Root rooted at destDir (opened once,
//     reused for every entry) rather than a string-joined path. os.Root
//     re-resolves each path component through the real filesystem and
//     refuses to leave the root, so containment holds even against a
//     name engineered to defeat a filepath.Clean-only check — matching
//     internal/kbdir's own os.Root usage for the analogous problem on
//     the *serving* side, and internal/contentsource's own
//     copyMarkdownDir/cleanGlob (build time).
//   - Symlink and hardlink entries are skipped outright: never created,
//     never followed. This is the primary escape vector — a tar entry
//     `wiki/leak.md -> /etc/passwd`, extracted with a naive
//     os.Symlink(hdr.Linkname, target), would recreate exactly the
//     symlink vulnerability kbdir's adapter was hardened against, except
//     on the write side instead of the read side. os.Root.Symlink
//     documents that it does NOT validate the link target, so it must
//     never be called with a tar-supplied Linkname.
//   - An entry name that's absolute, contains a literal backslash or
//     colon (Windows drive-absolute paths, e.g. `C:\evil`, and
//     separator confusion), or that (after cleaning) still carries a
//     leading ".." element, is rejected outright — loudly and
//     specifically — rather than relying on os.Root alone to contain
//     it. os.Root is the backstop, not the only check.
//   - Only regular files and directories are ever created. Every other
//     type (symlink, hardlink, device node, FIFO, etc.) is skipped.
//   - A cumulative uncompressed-byte budget (maxExtractedTotalBytes)
//     across the whole archive guards against a decompression bomb; a
//     per-file budget (maxExtractedFileBytes) guards against a single
//     huge entry; an entry-count cap (maxExtractEntries) guards against
//     a huge number of tiny entries.
//   - Permissions are normalised to 0644 (files) / 0755 (dirs) rather
//     than trusting the archive's mode bits.
func extractTarGz(tarballPath, destDir string) error {
	f, err := os.Open(tarballPath) //nolint:gosec // G304: tarballPath is our own just-downloaded, sha256-verified tempfile (see FetchURL), not attacker-chosen.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("open extraction destination: %w", err)
	}
	defer func() { _ = root.Close() }()

	// SECURITY (gosec G110 / decompression bomb): tr reads from limited,
	// not directly from gz, so every byte archive/tar pulls out of the
	// gzip stream — for a matched entry, a skipped one, or padding
	// between entries — counts against maxExtractedTotalBytes.
	limited := &cumulativeLimitReader{r: gz, remaining: maxExtractedTotalBytes}
	tr := tar.NewReader(limited)

	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if limited.exceeded {
				return fmt.Errorf("archive exceeds the %d-byte cumulative uncompressed size cap; refusing (corrupt or unexpectedly large archive)", maxExtractedTotalBytes)
			}
			return fmt.Errorf("tar: %w", err)
		}

		entries++
		if entries > maxExtractEntries {
			return fmt.Errorf("archive exceeds the %d-entry cap; refusing", maxExtractEntries)
		}

		name, ok := safeEntryName(hdr.Name)
		if !ok {
			return fmt.Errorf("refusing to extract entry with unsafe name %q", hdr.Name)
		}
		if name == "." {
			continue // the archive's own root directory entry, if present.
		}
		nativeName := filepath.FromSlash(name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(nativeName, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", name, err)
			}
		case tar.TypeReg:
			if err := extractRegularFile(root, name, nativeName, tr, limited); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// SECURITY: the primary escape vector — see the doc comment
			// above. Never create, never follow: os.Root.Symlink does
			// not validate a symlink's target, so creating one from a
			// tar-supplied Linkname would let the entry point anywhere
			// on the host filesystem regardless of os.Root's own
			// containment.
			continue
		default:
			// Device nodes, FIFOs, sockets, etc. — not meaningful for a
			// content archive; skip rather than extract.
			continue
		}
	}
	return nil
}

// extractRegularFile writes one tar.TypeReg entry through root,
// enforcing the per-file and cumulative size caps. name is the entry's
// cleaned, slash-separated path (used only in error messages); nativeName
// is the same path converted to the host's separator convention (what
// every os.Root call actually uses).
func extractRegularFile(root *os.Root, name, nativeName string, tr *tar.Reader, cum *cumulativeLimitReader) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", dir, err)
		}
	}
	out, err := root.OpenFile(nativeName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %q: %w", name, err)
	}
	defer func() { _ = out.Close() }()

	n, err := io.Copy(out, io.LimitReader(tr, maxExtractedFileBytes+1))
	if err != nil {
		if cum.exceeded {
			return fmt.Errorf("archive exceeds the %d-byte cumulative uncompressed size cap; refusing (corrupt or unexpectedly large archive)", maxExtractedTotalBytes)
		}
		return fmt.Errorf("write %q: %w", name, err)
	}
	if n > maxExtractedFileBytes {
		return fmt.Errorf("entry %q exceeds the %d-byte per-file cap; refusing", name, maxExtractedFileBytes)
	}
	return out.Close()
}

// safeEntryName validates a tar entry's name and returns its cleaned,
// slash-separated form (or "", false if the name is rejected). tar entry
// names are always "/"-separated regardless of host OS (GNU/POSIX/ustar),
// so cleaning uses the "path" package, not "path/filepath" — callers
// convert to the host separator (filepath.FromSlash) only at the point
// of an actual os.Root call.
//
// This check must not be the only thing standing between a hostile name
// and the filesystem (os.Root is the backstop — see extractTarGz's
// SECURITY comment), but nor should a determined attacker's ".." or
// absolute path only fail deep inside a MkdirAll call with a confusing
// error: rejecting it here up front is loud and specific.
func safeEntryName(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	// Reject a literal backslash (separator confusion — some tools may
	// misinterpret it as a path separator on Windows) and a colon
	// (Windows drive-absolute paths, e.g. "C:\evil" or "C:/evil"; the
	// "path" package's IsAbs only recognises a leading "/", not a drive
	// letter). No legitimate KB content path needs either character.
	for _, c := range name {
		if c == '\\' || c == ':' {
			return "", false
		}
	}
	if path.IsAbs(name) {
		return "", false
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}
