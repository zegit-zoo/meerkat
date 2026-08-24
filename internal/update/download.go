package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DownloadAsset fetches one release asset to a tempfile and returns
// its path + sha256 hex. token is optional: pass "" for anonymous
// download, which works fine now that the repo is public. Pass a cached
// gh token to get the higher authenticated GitHub API rate limit (or to
// reach a private repo, should this one ever go back to being private).
//
// GitHub asset API URLs respond with a 302 redirect to a signed CDN
// URL when Accept: application/octet-stream is set. Go's default
// client follows the redirect and correctly drops the Authorization
// header on the cross-host hop.
func DownloadAsset(ctx context.Context, url, token string) (path string, sha256hex string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// GitHub asset API requires octet-stream to serve the binary
	// directly (otherwise it returns JSON metadata).
	req.Header.Set("Accept", "application/octet-stream")
	setUA(req)

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if token != "" {
			return "", "", fmt.Errorf("GitHub returned %s — run `gh auth login` (or `gh auth status`) to refresh your token", resp.Status)
		}
		return "", "", fmt.Errorf("GitHub returned %s — this is likely the anonymous API rate limit; run `gh auth login` for a higher limit", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub returned %s for %s", resp.Status, url)
	}

	tmp, err := os.CreateTemp("", "meerkat-download-*.tar.gz")
	if err != nil {
		return "", "", fmt.Errorf("create tempfile: %w", err)
	}
	defer tmp.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("download: %w", err)
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// FetchChecksumFor returns the expected sha256 hex for assetName,
// reading the published checksums.txt for the release.
//
// Deprecated: prefer DownloadToTemp + ReadChecksumFor so cosign can
// verify the file before its contents are trusted.
func FetchChecksumFor(ctx context.Context, checksumURL, token, assetName string) (string, error) {
	body, err := fetchBytes(ctx, checksumURL, token, 64*1024)
	if err != nil {
		return "", fmt.Errorf("fetch checksums: %w", err)
	}
	return parseChecksum(body, assetName)
}

// ReadChecksumFor returns the expected sha256 hex for assetName,
// reading the checksums file from a local path. Use after cosign
// has verified the file's authenticity.
func ReadChecksumFor(path, assetName string) (string, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- path is a tmpfile we created
	if err != nil {
		return "", fmt.Errorf("read checksums: %w", err)
	}
	return parseChecksum(body, assetName)
}

func parseChecksum(body []byte, assetName string) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		// Format: "<sha256>  <filename>" (two spaces).
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == assetName || strings.HasSuffix(fields[1], "/"+assetName) {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum for %q not in checksums.txt", assetName)
}

// DownloadToTemp fetches a small auxiliary file (sig, cert, the
// checksums file itself) into a tempfile and returns the path. Use
// for files we want to hand to an external tool by path. token is
// optional — see DownloadAsset.
func DownloadToTemp(ctx context.Context, url, token, namePattern string) (string, error) {
	body, err := fetchBytes(ctx, url, token, 1*1024*1024)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", namePattern)
	if err != nil {
		return "", fmt.Errorf("create tempfile: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// fetchBytes performs a single GET with optional bearer auth (token may
// be "" for anonymous access) and Accept: application/octet-stream
// (required for GitHub asset URLs), returning the response body bounded
// by maxBytes.
func fetchBytes(ctx context.Context, url, token string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/octet-stream")
	setUA(req)
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		if token != "" {
			return nil, fmt.Errorf("GitHub returned %s — run `gh auth login` (or `gh auth status`) to refresh your token", resp.Status)
		}
		return nil, fmt.Errorf("GitHub returned %s — this is likely the anonymous API rate limit; run `gh auth login` for a higher limit", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s for %s", resp.Status, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

// maxExtractedBinarySize caps how many bytes we'll write out when
// extracting the `meerkat` binary from the downloaded tarball. This runs
// after sha256 + cosign verification, so it's defense in depth rather
// than a load-bearing control — but a tar entry lying about its size (or
// a gzip bomb) would otherwise let io.Copy write an unbounded amount to
// disk. 512 MiB is comfortably above any real build of this binary.
const maxExtractedBinarySize = 512 << 20 // 512 MiB

// maxExtractedArchiveBytes caps the *cumulative* uncompressed bytes we
// will read from the tarball across every entry — not just the one
// entry (named "meerkat") that maxExtractedBinarySize bounds.
// archive/tar's Reader.Next() transparently reads and discards
// whatever is left of the current entry when advancing to the next
// one, so without a cumulative cap, a single decoy entry that lies
// about its size (fed through gzip, which can inflate at very high
// ratios) forces us to decompress and discard an attacker-chosen
// number of bytes before we ever reach — or fail to find — the real
// `meerkat` entry. The headroom above maxExtractedBinarySize covers
// the small ancillary files goreleaser bundles alongside the binary
// (LICENSE, NOTICE, THIRD-PARTY-LICENSES.txt, README.md), none of
// which are more than a few hundred KiB in practice.
//
// A var, not a const: tests shrink it to exercise the cumulative-cap
// logic through ExtractMeerkat's real gzip/tar path at a tiny,
// deterministic scale instead of pushing hundreds of MiB through the
// race detector on every run.
var maxExtractedArchiveBytes int64 = maxExtractedBinarySize + 16<<20 // +16 MiB headroom

// errCumulativeLimitExceeded is cumulativeLimitReader's sentinel Read
// error once its budget is exhausted.
var errCumulativeLimitExceeded = errors.New("cumulative archive size limit exceeded")

// cumulativeLimitReader wraps r and enforces a total byte budget
// across all reads, no matter how many logical tar entries (matched,
// skipped, or partially read) those bytes are attributed to. Unlike
// io.LimitReader — which turns "budget exhausted" into a quiet
// io.EOF, indistinguishable from a legitimately short archive — it
// also records that the budget was the reason reading stopped, via
// exceeded, so the caller can fail loudly and specifically for that
// case regardless of how archive/tar's internals wrap or rewrap the
// Read error on its way back out of tr.Next() / io.Copy.
type cumulativeLimitReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (c *cumulativeLimitReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		c.exceeded = true
		return 0, errCumulativeLimitExceeded
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// ExtractMeerkat extracts the `meerkat` binary from a tar.gz to a
// tempfile and returns its path. The caller is responsible for
// removing the file.
func ExtractMeerkat(tarballPath string) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()

	limited := &cumulativeLimitReader{r: gz, remaining: maxExtractedArchiveBytes}
	tr := tar.NewReader(limited)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if limited.exceeded {
				return "", fmt.Errorf("extract: tarball exceeds the %d-byte cumulative uncompressed size cap; refusing (corrupt or unexpectedly large tarball)", maxExtractedArchiveBytes)
			}
			return "", fmt.Errorf("tar: %w", err)
		}
		// Goreleaser puts the binary at the root of the tarball.
		if filepath.Base(hdr.Name) != "meerkat" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.CreateTemp("", "meerkat-extracted-*")
		if err != nil {
			return "", fmt.Errorf("tempfile: %w", err)
		}
		n, err := io.Copy(out, io.LimitReader(tr, maxExtractedBinarySize+1))
		if err != nil {
			out.Close()
			os.Remove(out.Name())
			if limited.exceeded {
				return "", fmt.Errorf("extract: tarball exceeds the %d-byte cumulative uncompressed size cap; refusing (corrupt or unexpectedly large tarball)", maxExtractedArchiveBytes)
			}
			return "", fmt.Errorf("extract: %w", err)
		}
		if n > maxExtractedBinarySize {
			out.Close()
			os.Remove(out.Name())
			return "", fmt.Errorf("extract: %q exceeds the %d-byte cap; refusing (corrupt or unexpectedly large tarball)", hdr.Name, maxExtractedBinarySize)
		}
		if err := out.Chmod(0o755); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", fmt.Errorf("chmod: %w", err)
		}
		out.Close()
		return out.Name(), nil
	}
	return "", fmt.Errorf("no `meerkat` binary in tarball")
}

// ExtractMeerkatArchive extracts the `meerkat` binary from a
// downloaded release archive, dispatching purely on assetName's
// extension: a ".zip" (Windows releases, per .goreleaser.yaml's
// archives.format_overrides) goes through extractMeerkatZip, every
// other extension goes through the existing ExtractMeerkat tar.gz
// path. Dispatch is keyed on the *requested asset name* — the actual
// content we asked GitHub for — rather than archivePath's own
// filename, since DownloadAsset always names its tempfile
// "*.tar.gz" regardless of what bytes actually ended up in it.
//
// mk update itself never needs this: it only ever runs on (and so
// only ever needs to extract for) the single platform it already is,
// via ExtractMeerkat directly. This exists for meerkat-bootstrap,
// which must be able to select and extract the correct archive for
// whichever platform it's running on, Windows included.
func ExtractMeerkatArchive(archivePath, assetName string) (string, error) {
	if strings.HasSuffix(assetName, ".zip") {
		return extractMeerkatZip(archivePath)
	}
	return ExtractMeerkat(archivePath)
}

// extractMeerkatZip extracts `meerkat.exe` from a Windows release zip
// to a tempfile and returns its path. Mirrors ExtractMeerkat's
// per-entry size cap (maxExtractedBinarySize) and its "only a regular
// file literally named meerkat.exe" allowlist. Zip's central
// directory records each entry's uncompressed size up front, so —
// unlike tar.gz's sequential stream — skipping entries that aren't
// "meerkat.exe" never requires decompressing them; the cumulative-cap
// concern ExtractMeerkat has to defend against (a decoy entry forcing
// unbounded decompression while searching for the real one) does not
// apply here, so this only needs the per-entry cap plus a bounded
// copy of the one entry we do read.
func extractMeerkatZip(archivePath string) (string, error) {
	zr, err := zip.OpenReader(archivePath) // #nosec G304 -- archivePath is a tempfile we downloaded and are about to sha256/cosign-verify by the time this runs
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if filepath.Base(f.Name) != "meerkat.exe" {
			continue
		}
		if !f.Mode().IsRegular() {
			continue
		}
		if f.UncompressedSize64 > maxExtractedBinarySize {
			return "", fmt.Errorf("extract: %q exceeds the %d-byte cap; refusing (corrupt or unexpectedly large archive)", f.Name, maxExtractedBinarySize)
		}

		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("open zip entry: %w", err)
		}
		out, err := os.CreateTemp("", "meerkat-extracted-*.exe")
		if err != nil {
			_ = rc.Close()
			return "", fmt.Errorf("tempfile: %w", err)
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxExtractedBinarySize+1))
		_ = rc.Close()
		if err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", fmt.Errorf("extract: %w", err)
		}
		if n > maxExtractedBinarySize {
			out.Close()
			os.Remove(out.Name())
			return "", fmt.Errorf("extract: %q exceeds the %d-byte cap; refusing (corrupt or unexpectedly large archive)", f.Name, maxExtractedBinarySize)
		}
		if err := out.Chmod(0o755); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", fmt.Errorf("chmod: %w", err)
		}
		out.Close()
		return out.Name(), nil
	}
	return "", fmt.Errorf("no `meerkat.exe` binary in zip")
}
