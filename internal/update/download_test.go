package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
