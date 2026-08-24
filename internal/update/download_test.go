package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAsset constructs a tarball containing just a 'meerkat' binary
// with the given content. Returns (bytes, hex sha256).
func fakeAsset(t *testing.T, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "meerkat", Mode: 0o755, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])
}

// TestDownloadAsset_HappyPath: server returns the tarball + correct
// auth header is required.
func TestDownloadAsset_HappyPath(t *testing.T) {
	body, wantSha := fakeAsset(t, "fake binary contents")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	path, gotSha, err := DownloadAsset(context.Background(), srv.URL+"/asset.tar.gz", "test-token")
	if err != nil {
		t.Fatalf("DownloadAsset: %v", err)
	}
	defer os.Remove(path)
	if gotSha != wantSha {
		t.Errorf("sha = %q, want %q", gotSha, wantSha)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, body) {
		t.Error("downloaded bytes don't match served bytes")
	}
}

// TestDownloadAsset_401 surfaces a friendly auth error.
func TestDownloadAsset_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, _, err := DownloadAsset(context.Background(), srv.URL+"/x", "tok")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "expired") && !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401 or expired token, got: %v", err)
	}
}

// TestFetchChecksumFor parses the standard "sha256  filename" lines.
func TestFetchChecksumFor(t *testing.T) {
	body := strings.Join([]string{
		"abcdef  meerkat_0.4.0_darwin_arm64.tar.gz",
		"123456  meerkat_0.4.0_linux_amd64.tar.gz",
		"deadbe  ./meerkat_0.4.0_darwin_amd64.tar.gz", // path-prefixed form
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	cases := map[string]string{
		"meerkat_0.4.0_darwin_arm64.tar.gz": "abcdef",
		"meerkat_0.4.0_linux_amd64.tar.gz":  "123456",
		"meerkat_0.4.0_darwin_amd64.tar.gz": "deadbe", // tolerate the ./ prefix
	}
	for asset, wantSha := range cases {
		got, err := FetchChecksumFor(context.Background(), srv.URL+"/c.txt", "tok", asset)
		if err != nil {
			t.Errorf("%s: %v", asset, err)
			continue
		}
		if got != wantSha {
			t.Errorf("%s: got %q, want %q", asset, got, wantSha)
		}
	}
}

// TestFetchChecksumFor_NotInList returns a clear error.
func TestFetchChecksumFor_NotInList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("abc  other.tar.gz\n"))
	}))
	defer srv.Close()

	_, err := FetchChecksumFor(context.Background(), srv.URL+"/c", "tok", "missing.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "missing.tar.gz") {
		t.Errorf("expected 'missing.tar.gz' in error, got: %v", err)
	}
}

// TestExtractMeerkat round-trips: build a tarball, extract it,
// confirm the bytes match.
func TestExtractMeerkat(t *testing.T) {
	body, _ := fakeAsset(t, "fake binary")
	tmp := filepath.Join(t.TempDir(), "asset.tar.gz")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	binPath, err := ExtractMeerkat(tmp)
	if err != nil {
		t.Fatalf("ExtractMeerkat: %v", err)
	}
	defer os.Remove(binPath)
	got, _ := os.ReadFile(binPath)
	if string(got) != "fake binary" {
		t.Errorf("got %q, want %q", got, "fake binary")
	}
	stat, _ := os.Stat(binPath)
	if stat.Mode().Perm()&0o100 == 0 {
		t.Errorf("extracted binary not executable: mode = %v", stat.Mode())
	}
}

// TestExtractMeerkat_OversizeRejected: an entry named "meerkat" claiming
// to be larger than maxExtractedBinarySize must be rejected rather than
// silently truncated or written unbounded to disk.
func TestExtractMeerkat_OversizeRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	size := int64(maxExtractedBinarySize + 1024)
	if err := tw.WriteHeader(&tar.Header{Name: "meerkat", Mode: 0o755, Size: size}); err != nil {
		t.Fatal(err)
	}
	// Write the declared size's worth of zero bytes without holding it
	// all in memory.
	if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	tmp := filepath.Join(t.TempDir(), "oversize.tar.gz")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractMeerkat(tmp)
	if err == nil {
		t.Fatal("expected an error for an oversize tar entry")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected a size-cap error, got: %v", err)
	}
}

// zeroReader is an io.Reader that yields an endless stream of zero
// bytes, used to synthesize a large tar entry without allocating it.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestExtractMeerkat_CumulativeOversizeRejected is the regression test
// for the decompression-bomb finding: a *decoy* entry (not named
// "meerkat", so it never goes through the per-file
// maxExtractedBinarySize check at all) that declares a size larger
// than the cumulative archive budget must still be rejected.
// archive/tar's Reader.Next() silently reads and discards whatever is
// left of an entry we don't fully consume when advancing past it, so
// without a cumulative cap this decoy alone would force us to
// decompress and discard an unbounded, attacker-chosen number of
// bytes — the exact "3 GiB decoy in a ~3 MB tarball" reproduction from
// the audit, scaled down here to keep the test fast.
func TestExtractMeerkat_CumulativeOversizeRejected(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	size := maxExtractedArchiveBytes + 4096
	if err := tw.WriteHeader(&tar.Header{Name: "decoy-not-meerkat", Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	// Write the declared size's worth of zero bytes without holding
	// it all in memory; gzip compresses this to a tiny stream.
	if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	tmp := filepath.Join(t.TempDir(), "bomb.tar.gz")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := ExtractMeerkat(tmp)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error for a cumulative-oversize archive")
	}
	if !strings.Contains(err.Error(), "cumulative") {
		t.Errorf("expected a cumulative-cap error, got: %v", err)
	}
	// Guard against the fix regressing to "decompress everything, then
	// check": this should fail fast once the budget is exhausted, not
	// after reading the full (fake, zero-filled) decoy payload.
	if elapsed > 5*time.Second {
		t.Errorf("ExtractMeerkat took %s — the cumulative cap should short-circuit quickly", elapsed)
	}
}

// TestExtractMeerkat_CumulativeCapCatchesMultipleSmallEntries confirms
// the cap is genuinely cumulative — several entries, each individually
// small, that together exceed the budget must still be rejected. This
// rules out an implementation that (incorrectly) only re-checks the
// budget once per entry using that entry's own declared size rather
// than tracking bytes actually consumed across the whole archive.
//
// Runs against a temporarily shrunk maxExtractedArchiveBytes so this
// stays fast and deterministic — TestExtractMeerkat_CumulativeOversizeRejected
// already covers the real production-scale cap end to end.
func TestExtractMeerkat_CumulativeCapCatchesMultipleSmallEntries(t *testing.T) {
	origCap := maxExtractedArchiveBytes
	maxExtractedArchiveBytes = 10 * 1024 // 10 KiB
	t.Cleanup(func() { maxExtractedArchiveBytes = origCap })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	const perEntry = 1024 // 1 KiB — individually well under the 10 KiB cap
	entries := int(maxExtractedArchiveBytes/perEntry) + 2
	for i := 0; i < entries; i++ {
		// Duplicate names across entries are fine here: none of them
		// is "meerkat", and tar doesn't require unique names.
		if err := tw.WriteHeader(&tar.Header{Name: "decoy", Mode: 0o644, Size: perEntry}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(tw, zeroReader{}, perEntry); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()

	tmp := filepath.Join(t.TempDir(), "many-small.tar.gz")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractMeerkat(tmp)
	if err == nil {
		t.Fatal("expected an error once cumulative entries exceed the cap")
	}
	if !strings.Contains(err.Error(), "cumulative") {
		t.Errorf("expected a cumulative-cap error, got: %v", err)
	}
}

// TestCumulativeLimitReader is a fast, direct unit test of the reader
// itself (no tar/gzip involved): it must allow exactly the configured
// budget, then fail with errCumulativeLimitExceeded and flip
// exceeded=true on the very next read attempt.
func TestCumulativeLimitReader(t *testing.T) {
	data := strings.Repeat("a", 100)
	cr := &cumulativeLimitReader{r: strings.NewReader(data), remaining: 40}

	buf := make([]byte, 4096)
	n, err := cr.Read(buf)
	if err != nil {
		t.Fatalf("first read: unexpected error %v", err)
	}
	if n != 40 {
		t.Fatalf("first read returned %d bytes, want 40 (the whole budget in one call)", n)
	}
	if cr.exceeded {
		t.Fatal("exceeded should still be false after consuming exactly the budget")
	}

	n, err = cr.Read(buf)
	if n != 0 {
		t.Errorf("read past budget returned %d bytes, want 0", n)
	}
	if !errors.Is(err, errCumulativeLimitExceeded) {
		t.Fatalf("err = %v, want errCumulativeLimitExceeded", err)
	}
	if !cr.exceeded {
		t.Fatal("expected exceeded=true once the budget is exhausted")
	}
}

// TestExtractMeerkat_NoBinary errors when the tarball doesn't
// contain a 'meerkat' file.
func TestExtractMeerkat_NoBinary(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte("hi\n")
	_ = tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: int64(len(content))})
	_, _ = tw.Write(content)
	tw.Close()
	gz.Close()

	tmp := filepath.Join(t.TempDir(), "nope.tar.gz")
	_ = os.WriteFile(tmp, buf.Bytes(), 0o644)
	_, err := ExtractMeerkat(tmp)
	if err == nil || !strings.Contains(err.Error(), "no `meerkat` binary") {
		t.Errorf("expected 'no meerkat binary' error, got: %v", err)
	}
}

// fakeZipAsset builds a Windows-shaped release zip containing
// meerkat.exe with the given content, mirroring fakeAsset's tar.gz
// helper above. Returns (bytes, hex sha256).
func fakeZipAsset(t *testing.T, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("meerkat.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])
}

// TestExtractMeerkatArchive_Zip round-trips a Windows-shaped zip
// through ExtractMeerkatArchive, dispatched purely by the requested
// asset name's ".zip" suffix (as meerkat-bootstrap does) rather than
// by the downloaded tempfile's own name.
func TestExtractMeerkatArchive_Zip(t *testing.T) {
	body, _ := fakeZipAsset(t, "fake windows binary")
	// DownloadAsset always names its tempfile "*.tar.gz" regardless of
	// actual content (see download.go) -- use that same misleading
	// name here to prove dispatch really is keyed on assetName, not
	// archivePath.
	tmp := filepath.Join(t.TempDir(), "download.tar.gz")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}

	binPath, err := ExtractMeerkatArchive(tmp, "meerkat_0.10.0_windows_amd64.zip")
	if err != nil {
		t.Fatalf("ExtractMeerkatArchive: %v", err)
	}
	defer os.Remove(binPath)

	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if string(got) != "fake windows binary" {
		t.Errorf("got %q, want %q", got, "fake windows binary")
	}
	if !strings.HasSuffix(binPath, ".exe") {
		t.Errorf("extracted path %q does not end in .exe", binPath)
	}
}

// TestExtractMeerkatArchive_TarGz confirms the non-".zip" dispatch
// path still goes through the existing tar.gz extraction unchanged.
func TestExtractMeerkatArchive_TarGz(t *testing.T) {
	body, _ := fakeAsset(t, "fake unix binary")
	tmp := filepath.Join(t.TempDir(), "asset.tar.gz")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	binPath, err := ExtractMeerkatArchive(tmp, "meerkat_0.10.0_linux_amd64.tar.gz")
	if err != nil {
		t.Fatalf("ExtractMeerkatArchive: %v", err)
	}
	defer os.Remove(binPath)
	got, _ := os.ReadFile(binPath)
	if string(got) != "fake unix binary" {
		t.Errorf("got %q", got)
	}
}

// TestExtractMeerkatZip_NoBinary errors when the zip has no
// meerkat.exe entry -- the tampered/mismatched-asset case.
func TestExtractMeerkatZip_NoBinary(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("hi\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "nope.zip")
	_ = os.WriteFile(tmp, buf.Bytes(), 0o644)

	_, err = ExtractMeerkatArchive(tmp, "meerkat_0.10.0_windows_amd64.zip")
	if err == nil || !strings.Contains(err.Error(), "no `meerkat.exe` binary") {
		t.Errorf("expected 'no meerkat.exe binary' error, got: %v", err)
	}
}

// TestExtractMeerkatZip_OversizeRejected: a meerkat.exe entry whose
// actual uncompressed content exceeds the per-entry cap must be
// rejected. zip.Writer computes UncompressedSize64 from what's
// actually written (recorded in the central directory zw.Close()
// writes out), so -- like TestExtractMeerkat_OversizeRejected's
// tar.gz equivalent above -- this writes the declared size's worth of
// real (compressible) zero bytes via zeroReader rather than faking
// the header field directly.
func TestExtractMeerkatZip_OversizeRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("meerkat.exe")
	if err != nil {
		t.Fatal(err)
	}
	size := int64(maxExtractedBinarySize + 1024)
	if _, err := io.CopyN(w, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "oversize.zip")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = ExtractMeerkatArchive(tmp, "meerkat_0.10.0_windows_amd64.zip")
	if err == nil {
		t.Fatal("expected an error for an oversize zip entry")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected a size-cap error, got: %v", err)
	}
}
