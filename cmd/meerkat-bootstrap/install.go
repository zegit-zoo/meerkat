package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/auth"
	"github.com/zegit-zoo/meerkat/internal/update"
)

// installTimeout bounds the entire install flow: release lookup,
// download, verification, and install. Generous -- release assets are
// tens of MB -- but still finite, so a hung connection or an
// unresponsive destination binary's smoke check can't wedge the
// process forever.
const installTimeout = 5 * time.Minute

func newInstallCmd() *cobra.Command {
	var (
		release     string
		destination string
		force       bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download, verify, and install a zegit-zoo/meerkat release at --destination",
		Long: `install downloads a zegit-zoo/meerkat release and verifies it exactly the
way "mk update" does -- a cosign signature over the checksums file,
checked against the exact GitHub Actions workflow/tag identity that
signed it, then the per-asset SHA-256 -- and only then atomically swaps
it into --destination.

Unlike "mk update", install does not assume the binary already at
--destination understands zegit-zoo/meerkat's release feed, or trusts
its signing identity, at all. That gap -- not SemVer ordering -- is
exactly what this command exists to cross, once, so every subsequent
"mk update" can take over from there.

Assets are only ever fetched from github.com/zegit-zoo/meerkat. The
repository is public, so this works anonymously; a cached "gh auth
login" token is used automatically if present, purely for the higher
API rate limit -- there is no flag to pass a token directly.

Examples:
  meerkat-bootstrap install --destination "$(command -v meerkat)"
  meerkat-bootstrap install --release v0.10.0 --destination /usr/local/bin/meerkat
  meerkat-bootstrap install --destination /usr/local/bin/meerkat --force`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), installTimeout)
			defer cancel()
			return runInstall(ctx, installOptions{
				release:     release,
				destination: destination,
				force:       force,
			}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&release, "release", "",
		"Release tag to install, e.g. v0.10.0 (default: the newest stable release)")
	cmd.Flags().StringVar(&destination, "destination", "",
		"Path to the binary to replace (default: the first of `meerkat`/`mk` found on $PATH)")
	cmd.Flags().BoolVar(&force, "force", false,
		"Install even if --release is older than the detected installed version")
	return cmd
}

// installOptions is install's parsed flag set, kept as a plain struct
// so runInstall is callable directly from tests without going through
// cobra.
type installOptions struct {
	release     string
	destination string
	force       bool
}

// runInstall is the whole install flow. It mirrors internal/cli's
// `mk update` RunE step for step -- fetch release metadata, locate
// assets, verify, extract, install -- reusing internal/update for
// every security-sensitive piece, with two differences that follow
// directly from this being a bootstrap tool rather than a running
// binary updating itself: it resolves --destination as an arbitrary
// path (not os.Executable()) and it runs a `version` smoke check
// after the swap, restoring the backup automatically if that check
// (or anything upstream of it) fails.
func runInstall(ctx context.Context, opts installOptions, out io.Writer) error {
	destination, err := resolveDestinationFlag(opts.destination)
	if err != nil {
		return err
	}

	var rel *update.Release
	if opts.release != "" {
		rel, err = update.FetchByTag(ctx, opts.release)
	} else {
		rel, err = update.FetchLatest(ctx)
	}
	if err != nil {
		return fmt.Errorf("check release: %w", err)
	}
	fmt.Fprintf(out, "target:      %s (released %s)\n", rel.TagName, rel.PublishedAt)
	fmt.Fprintf(out, "destination: %s\n", destination)

	installedVersion, verErr := update.DetectInstalledVersion(ctx, destination)
	switch {
	case verErr != nil:
		// Can't tell what's installed -- same "unknown, never block"
		// stance IsDowngrade itself takes for an unparseable version.
		// Most commonly this just means "there's nothing runnable at
		// destination yet" or a binary predating a `version`
		// subcommand at all.
		fmt.Fprintf(out, "installed:   could not determine (%v) — downgrade guard skipped\n", verErr)
	default:
		fmt.Fprintf(out, "installed:   %s\n", installedVersion)
		if err := decideProceed(rel.TagName, installedVersion, opts.force); err != nil {
			return err
		}
	}

	// A cached gh CLI token is optional -- the repository is public,
	// so anonymous access works fine -- but sent when available for
	// the higher authenticated GitHub API rate limit. There is
	// deliberately no flag to pass a token directly: see
	// docs/design/upstream-migration.md.
	token, tokErr := auth.NewDefault().Token(auth.HostGitHub, "github.com")
	if tokErr != nil {
		token = ""
	}
	if token != "" {
		if user, _ := auth.NewDefault().User(auth.HostGitHub, "github.com"); user != "" {
			fmt.Fprintf(out, "auth:        gh token for %s\n", user)
		}
	}

	assetName := update.PickAssetNameFor(rel.TagName, runtime.GOOS, runtime.GOARCH)
	assetURL, ok := rel.FindAsset(assetName)
	if !ok {
		return fmt.Errorf("release %s has no asset matching %s — your platform may not be published", rel.TagName, assetName)
	}
	checksumName := update.ChecksumAssetName(rel.TagName)
	checksumURL, ok := rel.FindAsset(checksumName)
	if !ok {
		return fmt.Errorf("release %s has no checksums file %s", rel.TagName, checksumName)
	}
	bundleName := update.CosignAssetName(checksumName)
	bundleURL, ok := rel.FindAsset(bundleName)
	if !ok {
		return fmt.Errorf("release %s has no cosign bundle %s", rel.TagName, bundleName)
	}

	fmt.Fprintf(out, "asset:       %s\n", assetName)
	fmt.Fprintln(out, "downloading…")

	archivePath, gotSha, err := update.DownloadAsset(ctx, assetURL, token)
	if err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	defer os.Remove(archivePath)

	// Cosign verification anchors trust in the checksums file; the
	// per-asset sha256 then transitively verifies the archive. Verify
	// cosign BEFORE trusting the checksums file's contents -- an
	// attacker who could swap the checksums file alone could
	// otherwise feed a matching-but-malicious sha256.
	checksumsLocal, err := update.DownloadToTemp(ctx, checksumURL, token, "meerkat-bootstrap-checksums-*.txt")
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	defer os.Remove(checksumsLocal)

	bundlePath, err := update.DownloadToTemp(ctx, bundleURL, token, "meerkat-bootstrap-checksums-*.sigstore.json")
	if err != nil {
		return fmt.Errorf("fetch cosign bundle: %w", err)
	}
	defer os.Remove(bundlePath)

	fmt.Fprintln(out, "cosign:      verifying signature on checksums…")
	if err := update.VerifyChecksumSignature(ctx, checksumsLocal, bundlePath); err != nil {
		if errors.Is(err, update.ErrCosignMissing) {
			return fmt.Errorf(
				"cosign binary not found on PATH.\n\n"+
					"meerkat-bootstrap requires cosign to verify the release signature "+
					"-- unlike `mk update`, there is no --skip-cosign fallback here, "+
					"since this is the one step that establishes trust in the upstream "+
					"signing identity in the first place. Install cosign via "+
					"`brew install cosign` (or see "+
					"https://docs.sigstore.dev/system_config/installation/) and re-run.\n\n"+
					"Original error: %w", err)
		}
		return fmt.Errorf("signature verification FAILED — refusing to install: %w", err)
	}
	fmt.Fprintf(out, "cosign:      ✓ signed by the %s release workflow\n", update.Project)

	expectedSha, err := update.ReadChecksumFor(checksumsLocal, assetName)
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	if !strings.EqualFold(gotSha, expectedSha) {
		return fmt.Errorf("checksum mismatch for %s — got %s, expected %s; refusing to install", assetName, gotSha, expectedSha)
	}
	fmt.Fprintf(out, "sha256:      %s ✓\n", gotSha[:16])

	binPath, err := update.ExtractMeerkatArchive(archivePath, assetName)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	defer os.Remove(binPath)

	fmt.Fprintln(out, "installing…")
	resolvedDestination, err := update.InstallAtomic(binPath, destination)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}

	fmt.Fprintln(out, "verifying the installed binary runs (`version` smoke check)…")
	if smokeErr := update.RunVersionSmoke(ctx, resolvedDestination); smokeErr != nil {
		fmt.Fprintf(out, "smoke check FAILED — restoring the previous binary: %v\n", smokeErr)
		if restoreErr := update.RestoreBackup(resolvedDestination); restoreErr != nil {
			return fmt.Errorf(
				"smoke check failed AND restoring the backup also failed — manual "+
					"recovery needed: move %q back to %q yourself.\nsmoke check error: %v\nrestore error: %w",
				resolvedDestination+".old", resolvedDestination, smokeErr, restoreErr)
		}
		return fmt.Errorf("newly installed binary failed its `version` smoke check; previous binary restored: %w", smokeErr)
	}
	fmt.Fprintln(out, "smoke check: ✓ new binary runs")
	if err := update.RemoveBackup(resolvedDestination); err != nil {
		fmt.Fprintf(out, "warning: could not remove backup %q: %v\n", resolvedDestination+".old", err)
	}

	fmt.Fprintf(out, "\ninstalled %s at %s\n", rel.TagName, resolvedDestination)
	fmt.Fprintln(out, "run `mk update --check` (or the equivalent at your --destination) to confirm it now queries "+update.Project)
	return nil
}

// decideProceed applies the downgrade guard: it refuses a target that
// update.IsDowngrade confirms is strictly older than installedVersion,
// unless force is set. Pure and side-effect-free, so it's directly
// unit-testable without any network or filesystem fake -- exactly the
// property update.IsDowngrade itself already has, which this simply
// delegates to (see docs/design/upstream-migration.md for why that
// comparator, not this command, is the single source of truth for
// "newer").
func decideProceed(targetTag, installedVersion string, force bool) error {
	if force {
		return nil
	}
	if update.IsDowngrade(targetTag, installedVersion) {
		return fmt.Errorf("target %s is older than the installed %s — pass --force to downgrade", targetTag, installedVersion)
	}
	return nil
}

// resolveDestinationFlag returns the effective --destination: the
// flag value if given, otherwise the first of `meerkat`/`mk` found on
// $PATH (mirroring the operator flow
// `meerkat-bootstrap install --destination "$(command -v meerkat)"`
// from the command's own help text, so leaving --destination off
// does the same thing that example spells out explicitly).
func resolveDestinationFlag(destination string) (string, error) {
	if destination != "" {
		return destination, nil
	}
	for _, name := range []string{"meerkat", "mk"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no --destination given, and neither `meerkat` nor `mk` was found on $PATH — pass --destination explicitly")
}
