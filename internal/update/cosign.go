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
//
// Anchored at both ends: release tags in this repo are strictly
// `vX.Y.Z` (see docs/RELEASE.md and the release.yml tag trigger;
// there is no prerelease-suffix convention in use), so the ref
// component after "v" is exactly three dot-separated numbers with
// nothing following. Not anchoring the end is not exploitable today
// — release.yml only triggers on an actual `push: tags:`, so no
// other ref can mint a matching OIDC identity — but leaving it open
// means any suffix after a valid version would also match, which is
// needless slack for a value whose entire job is exact-matching a
// trust boundary. Update this (and its mirror in .goreleaser.yaml's
// verification comment, and docs/RELEASE.md) together if the project
// ever adopts prerelease tags.
const CertIdentityRegexp = `^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`

// CertOIDCIssuer is the OIDC issuer for GitHub Actions workload
// identity tokens. Both this and CertIdentityRegexp must match for
// cosign to accept the signature.
const CertOIDCIssuer = "https://token.actions.githubusercontent.com"

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
// bundlePath is the Sigstore bundle goreleaser publishes alongside
// the checksums file (see CosignAssetName). The bundle carries the
// signature, the Fulcio certificate, and the Rekor inclusion proof
// (a Merkle proof against a signed checkpoint) in one file, so
// --insecure-ignore-tlog=false is enforced by verifying that
// embedded proof rather than by cosign making a live call to a
// Rekor URL — the same cryptographic guarantee (the signature is
// durably logged in the public transparency log), but checked
// offline and without trusting a specific endpoint at verify time.
// Cosign v3 dropped --signature/--certificate/--rekor-url from
// sign-blob/verify-blob in favor of this bundle; see docs/RELEASE.md.
//
// We deliberately invoke the external binary rather than vendor
// cosign as a library: the dependency footprint of sigstore-go is
// enormous (TUF, in-toto, OCI) and most update flows already
// require cosign to be installed for first-time install.
func VerifyChecksumSignature(ctx context.Context, checksumsPath, bundlePath string) error {
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
		"--insecure-ignore-tlog=false",
		"--bundle", bundlePath,
		checksumsPath,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cosign verify-blob failed: %w", err)
	}
	return nil
}

// CosignAssetName returns the Sigstore bundle file name that
// goreleaser publishes alongside the checksums file.
func CosignAssetName(checksumName string) string {
	return checksumName + ".sigstore.json"
}
