package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// ErrCosignMissing is returned when the cosign binary is not on PATH.
var ErrCosignMissing = errors.New("cosign binary not found on PATH")

// CertIdentityRegexp constrains acceptable Fulcio certificate
// identities to those issued for the meerkat release workflow on
// GitHub Actions. The regexp anchors on the canonical workflow path
// so that a compromise of a different repository (sharing the same
// GitHub Actions OIDC issuer) cannot forge a signature that we'd accept.
const CertIdentityRegexp = `^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v`

// CertOIDCIssuer is the OIDC issuer for GitHub Actions workload
// identity tokens. Both this and CertIdentityRegexp must match for
// cosign to accept the signature.
const CertOIDCIssuer = "https://token.actions.githubusercontent.com"

// RekorURL is the transparency log endpoint cosign must use when
// verifying signatures.
const RekorURL = "https://rekor.sigstore.dev"

// VerifyChecksumSignature runs `cosign verify-blob` to assert that
// checksumsPath was signed by the meerkat release workflow. It returns
// nil on success, ErrCosignMissing if cosign isn't installed, or
// the underlying error from cosign on verification failure.
//
// Verification is keyless (Fulcio + Rekor): no public key is
// embedded; trust is rooted in (a) the GitHub Actions OIDC issuer
// (token.actions.githubusercontent.com) and (b) the certificate
// identity regexp pinning the release.yml workflow path.
//
// We deliberately invoke the external binary rather than vendor
// cosign as a library: the dependency footprint of sigstore-go is
// enormous (TUF, in-toto, OCI) and most update flows already
// require cosign to be installed for first-time install.
func VerifyChecksumSignature(ctx context.Context, checksumsPath, sigPath, certPath string) error {
	cosign, err := exec.LookPath("cosign")
	if err != nil {
		return ErrCosignMissing
	}
	vCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// #nosec G204 -- arguments are constants or paths we just
	// downloaded into a controlled tmp dir; user input does not
	// influence argv beyond the temp file names.
	cmd := exec.CommandContext(vCtx, cosign,
		"verify-blob",
		"--certificate-identity-regexp", CertIdentityRegexp,
		"--certificate-oidc-issuer", CertOIDCIssuer,
		"--rekor-url", RekorURL,
		"--insecure-ignore-tlog=false",
		"--signature", sigPath,
		"--certificate", certPath,
		checksumsPath,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cosign verify-blob failed: %w", err)
	}
	return nil
}

// CosignAssetNames returns the signature and certificate file names
// that goreleaser publishes alongside the checksums file.
func CosignAssetNames(checksumName string) (sig, cert string) {
	return checksumName + ".sig", checksumName + ".pem"
}
