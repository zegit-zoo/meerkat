package contentsource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// completionMarker names the file written as the very last step of a
// successful extraction, before the atomic rename into the cache path
// (see FetchURL). Its presence — checked by isCacheComplete — is what
// lets a cache directory be trusted as "done" rather than merely
// "exists": os.Rename is atomic on every platform this ships for, so in
// the ordinary case a half-extracted directory never appears at the
// cache path at all, but the marker is checked anyway as a second,
// independent signal in case a cache directory ever got there by some
// other means (a differing/future code path, manual intervention, a
// prior crashed run's leftovers).
const completionMarker = ".meerkat-complete"

// urlCacheDir returns the cache directory a type: url source's extracted
// content lives in, keyed on its sha256 digest: content is immutable by
// digest, so a *complete* cache entry never needs re-fetching, and two
// different content.url values that happen to produce byte-identical
// archives correctly share one cache entry.
func urlCacheDir(sha256hex string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "meerkat", "content", "url", strings.ToLower(sha256hex)), nil
}

// isCacheComplete reports whether dir is a fully-extracted, cache-hit-
// eligible directory (see completionMarker).
func isCacheComplete(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, completionMarker))
	return err == nil && !fi.IsDir()
}

// maxDownloadBytes bounds the raw (compressed) download for a type: url
// content archive. This runs before sha256 verification, so — like
// internal/update's maxExtractedArchiveBytes — it's defense in depth:
// the hash check below will reject anything that doesn't match
// content.sha256 regardless, but without a cap, a malicious or merely
// misbehaving URL could still make us stream an unbounded response to
// disk before that check ever runs. No real KB archive approaches this:
// even a generously large wiki (thousands of pages, each already bounded
// to 8 MiB by internal/kb's own per-page cap) is a small fraction of it.
//
// A var, not a const, so tests can shrink it to exercise the cap through
// the real download path at a tiny, deterministic scale — mirrors
// internal/update/download.go's maxExtractedArchiveBytes and this
// package's own archive.go extraction limits.
var maxDownloadBytes int64 = 2 << 30 // 2 GiB

// urlHTTPClient is the client used to fetch type: url content archives.
// A package var (mirrors sync.go's resolveGitHubToken) so tests can
// point it at a local httptest server without weakening the redirect
// policy under test — the real CheckRedirect closure is reused as-is in
// those tests, just with the Transport swapped to trust the test
// server's certificate.
var urlHTTPClient = &http.Client{
	Timeout: 10 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// SECURITY: mirrors internal/update/client.go's redirect policy —
		// https required on every hop, not just the first request (Go's
		// client only strips Authorization on a cross-HOST redirect, not
		// on a same-host https->http downgrade — the scheme check is
		// unconditional here regardless of host for the same reason).
		// Unlike that client, there is deliberately no host allowlist: an
		// arbitrary host is the entire point of type: url (the operator
		// names content.url). This fetch carries no credentials to leak
		// on a downgrade, but an https->http redirect could still be used
		// to serve a tampered archive over an unauthenticated channel to
		// a caller that asked for https — the eventual sha256 check
		// catches a tampered *payload* regardless, but refusing the
		// downgrade in-transport is the same defense-in-depth posture as
		// the update client.
		if !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("refusing redirect to non-https URL %q", req.URL.Redacted())
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	},
}

// FetchURL resolves a type: url content source: skip-if-cached, fetch,
// verify, extract, cache. Returns the extracted content-repo-layout
// directory, ready for kbdir.ConfigureLayout/FSLayout.
//
// src.Type/URL/SHA256 are expected to already have passed Source.Validate
// (ResolveRuntime's LoadFile call does this); FetchURL re-checks the
// scheme and digest shape itself anyway as defense in depth against
// being called directly with an unvalidated Source.
func FetchURL(src Source) (dir string, err error) {
	if src.Type != TypeURL {
		return "", fmt.Errorf("FetchURL: type %q is not %q", src.Type, TypeURL)
	}
	u, perr := url.Parse(src.URL)
	if perr != nil || !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("content.url must be an https:// URL, got %q", src.URL)
	}
	if !isHex64(src.SHA256) {
		return "", fmt.Errorf("content.sha256 must be 64 hex characters, got %q", src.SHA256)
	}
	digest := strings.ToLower(src.SHA256)

	cacheDir, err := urlCacheDir(digest)
	if err != nil {
		return "", err
	}
	if isCacheComplete(cacheDir) {
		return cacheDir, nil // immutable by digest: restarts are free.
	}

	tmpFile, gotDigest, err := downloadToTemp(src.URL)
	if err != nil {
		return "", fmt.Errorf("content.url %s: %w", src.URL, err)
	}
	defer func() { _ = os.Remove(tmpFile) }()

	// SECURITY: verify BEFORE extracting anything. A mismatch must leave
	// nothing extracted and nothing cached — the function returns here,
	// before either happens.
	if gotDigest != digest {
		return "", fmt.Errorf("content.sha256 mismatch for %s: got %s, want %s — refusing to extract or cache", src.URL, gotDigest, digest)
	}

	cacheParent := filepath.Dir(cacheDir)
	if err := os.MkdirAll(cacheParent, 0o750); err != nil {
		return "", fmt.Errorf("create cache parent dir: %w", err)
	}
	// The extraction tempdir is a *sibling* of cacheDir (same parent), not
	// under os.TempDir(): the final step below is an os.Rename, which
	// requires both paths to be on the same filesystem to be atomic (a
	// cross-device rename fails outright). Extracting anywhere else would
	// risk either an error or a silent non-atomic copy-then-delete
	// fallback, defeating the "half-extracted directory never mistaken
	// for a good one" property this whole dance is for.
	tmpDir, err := os.MkdirTemp(cacheParent, ".extract-*")
	if err != nil {
		return "", fmt.Errorf("create extraction tempdir: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := extractTarGz(tmpFile, tmpDir); err != nil {
		return "", fmt.Errorf("extract %s: %w", src.URL, err)
	}
	// Written last, inside the not-yet-visible tmpDir, so the eventual
	// rename below moves a tree that already contains it — there is no
	// window where cacheDir could exist without the marker.
	if err := os.WriteFile(filepath.Join(tmpDir, completionMarker), []byte(digest+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write completion marker: %w", err)
	}

	// Best-effort: clear out any previous incomplete attempt at this
	// exact path (e.g. left by a process that crashed between MkdirTemp
	// and Rename in an earlier run — isCacheComplete's doc comment on
	// completionMarker covers why such a directory, if present, is never
	// treated as a cache hit above) so Rename can't fail on an existing
	// non-empty directory.
	_ = os.RemoveAll(cacheDir)
	if err := os.Rename(tmpDir, cacheDir); err != nil {
		return "", fmt.Errorf("finalize cache dir: %w", err)
	}
	succeeded = true
	return cacheDir, nil
}

// downloadToTemp streams rawURL to a new temp file, enforcing
// maxDownloadBytes, and returns the file's path plus the hex-encoded
// sha256 of exactly the bytes written. The caller is responsible for
// removing the file on every path (success or failure) once it's done
// with it.
func downloadToTemp(rawURL string) (path string, sha256hex string, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := urlHTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("server returned %s", resp.Status)
	}

	tmp, err := os.CreateTemp("", "meerkat-content-*.tar.gz")
	if err != nil {
		return "", "", fmt.Errorf("create tempfile: %w", err)
	}
	path = tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(path)
		}
	}()
	defer func() { _ = tmp.Close() }()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("download: %w", err)
	}
	if n > maxDownloadBytes {
		return "", "", fmt.Errorf("exceeds the %d-byte download cap; refusing", maxDownloadBytes)
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	success = true
	return path, hex.EncodeToString(h.Sum(nil)), nil
}
