package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCosignAssetName checks the suffix convention matches what
// goreleaser writes: <checksums>.sigstore.json.
func TestCosignAssetName(t *testing.T) {
	got := CosignAssetName("meerkat_0.4.2_checksums.txt")
	if got != "meerkat_0.4.2_checksums.txt.sigstore.json" {
		t.Errorf("asset name = %q", got)
	}
}

// TestCertIdentityRegexp_AnchoredAtEnd is the regression test for the
// unanchored-regexp finding: CertIdentityRegexp used to have no `$`
// (or other end-of-string anchor), so anything appended after a
// legitimate "refs/tags/vX.Y.Z" identity would still match. Confirm
// real release identities still match, and that appending anything —
// a forged suffix, a path traversal, garbage after the version, or a
// non-numeric "version" — is now rejected.
func TestCertIdentityRegexp_AnchoredAtEnd(t *testing.T) {
	re := regexp.MustCompile(CertIdentityRegexp)

	valid := []string{
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v1.2.3",
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v0.0.1",
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v10.20.30",
	}
	for _, id := range valid {
		if !re.MatchString(id) {
			t.Errorf("expected legitimate release identity to match: %q", id)
		}
	}

	invalid := []string{
		// The core regression: anything trailing a valid version must
		// NOT match now that the pattern is anchored at the end.
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v1.2.3-evil",
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v1.2.3zzz",
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v1.2.3/../../etc",
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v1.2.3\nextra",
		// Non-numeric / malformed version component.
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/vNOTAVERSION",
		"https://github.com/zegit-zoo/meerkat/.github/workflows/release.yml@refs/tags/v1.2",
		// Wrong repo path — should already fail on the (unchanged)
		// anchored prefix, kept here as a belt-and-suspenders check.
		"https://github.com/some-other-org/meerkat/.github/workflows/release.yml@refs/tags/v1.2.3",
	}
	for _, id := range invalid {
		if re.MatchString(id) {
			t.Errorf("expected forged/malformed identity to be rejected: %q", id)
		}
	}
}

// TestVerifyChecksumSignature_NoCosign returns ErrCosignMissing when
// the cosign binary is absent from PATH. We force this by setting
// PATH to an empty directory.
func TestVerifyChecksumSignature_NoCosign(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	err := VerifyChecksumSignature(context.Background(), "x", "y")
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

	err := VerifyChecksumSignature(context.Background(), "checksums", "bundle")
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

	if err := VerifyChecksumSignature(context.Background(), "c", "b"); err != nil {
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
