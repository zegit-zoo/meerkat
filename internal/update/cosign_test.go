package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCosignAssetNames checks the suffix convention matches what
// goreleaser writes: <checksums>.sig, <checksums>.pem.
func TestCosignAssetNames(t *testing.T) {
	sig, cert := CosignAssetNames("meerkat_0.4.2_checksums.txt")
	if sig != "meerkat_0.4.2_checksums.txt.sig" {
		t.Errorf("sig name = %q", sig)
	}
	if cert != "meerkat_0.4.2_checksums.txt.pem" {
		t.Errorf("cert name = %q", cert)
	}
}

// TestVerifyChecksumSignature_NoCosign returns ErrCosignMissing when
// the cosign binary is absent from PATH. We force this by setting
// PATH to an empty directory.
func TestVerifyChecksumSignature_NoCosign(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	err := VerifyChecksumSignature(context.Background(), "x", "y", "z")
	if !errors.Is(err, ErrCosignMissing) {
		t.Errorf("err = %v, want ErrCosignMissing", err)
	}
}

// TestVerifyChecksumSignature_BadSig fails when cosign is present
// but the signature does not verify. We stub cosign with a tiny
// shell script that always exits non-zero.
func TestVerifyChecksumSignature_BadSig(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "cosign")
	script := "#!/bin/sh\necho 'cosign: signature does not match' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	err := VerifyChecksumSignature(context.Background(), "checksums", "sig", "cert")
	if err == nil {
		t.Fatal("expected verification failure")
	}
	if errors.Is(err, ErrCosignMissing) {
		t.Errorf("should not be ErrCosignMissing when binary exists")
	}
	if !strings.Contains(err.Error(), "cosign verify-blob failed") {
		t.Errorf("error should mention cosign verify-blob: %v", err)
	}
}

// TestVerifyChecksumSignature_OK succeeds when the cosign stub
// returns 0. Exercises the happy path without needing real Sigstore
// infrastructure.
func TestVerifyChecksumSignature_OK(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "cosign")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if err := VerifyChecksumSignature(context.Background(), "c", "s", "p"); err != nil {
		t.Errorf("expected success, got %v", err)
	}
}

// TestDownloadToTemp writes the response body to a tempfile.
func TestDownloadToTemp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	path, err := DownloadToTemp(context.Background(), srv.URL+"/x", "tok", "test-*.bin")
	if err != nil {
		t.Fatalf("DownloadToTemp: %v", err)
	}
	defer os.Remove(path)
	body, _ := os.ReadFile(path)
	if string(body) != "hello" {
		t.Errorf("body = %q", body)
	}
}

// TestReadChecksumFor parses lines from a local file (cosign-verified
// in production).
func TestReadChecksumFor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.txt")
	body := "abcdef  meerkat_0.4.2_darwin_arm64.tar.gz\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadChecksumFor(path, "meerkat_0.4.2_darwin_arm64.tar.gz")
	if err != nil {
		t.Fatalf("ReadChecksumFor: %v", err)
	}
	if got != "abcdef" {
		t.Errorf("got %q", got)
	}
}
