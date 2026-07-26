package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zegit-zoo/meerkat/internal/auth"
	"github.com/zegit-zoo/meerkat/internal/update"
)

func newUpdateCmd() *cobra.Command {
	var (
		checkOnly  bool
		pinTag     string
		force      bool
		yes        bool
		skipCosign bool
	)
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for or install meerkat updates",
		Long: `Check the GitHub Releases page for newer meerkat versions and,
unless --check is given, download + atomically swap the binary
+ re-exec.

The repository is public, so this works with no authentication at
all. If you've run 'gh auth login', meerkat borrows the cached
token from gh's OAuth credential cache and sends it for the higher
authenticated GitHub API rate limit — there are no PATs to paste
either way.

Examples:
  mk update --check                  # just print latest version
  mk update                          # interactive: prompt before swap
  mk update --yes                    # download + swap without prompt
  mk update --version v0.4.0         # pin to a specific tag
  mk update --force                  # downgrade or re-install same`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()

			cur := currentVersion()
			rel, err := update.FetchByTag(ctx, pinTag)
			if err != nil {
				return fmt.Errorf("check release: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"current: %s  (built %s on %s/%s)\n",
				cur.Version, cur.Date, cur.OS, cur.Arch)
			fmt.Fprintf(cmd.OutOrStdout(),
				"target:  %s  (released %s)\n",
				rel.TagName, rel.PublishedAt)

			if checkOnly {
				return nil
			}

			// Same version + no --force? bail out.
			if !force && versionEq(cur.Version, rel.TagName) {
				fmt.Fprintln(cmd.OutOrStdout(), "already on the target version (use --force to re-install)")
				return nil
			}

			// Confirm.
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(), "\nDownload + install %s? [y/N] ", rel.TagName)
				var ans string
				_, _ = fmt.Fscanln(os.Stdin, &ans)
				if strings.ToLower(strings.TrimSpace(ans)) != "y" {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}

			// Get a GitHub token via the gh CLI cache, if one is available.
			// This is optional now that the repo is public: anonymous
			// download works fine, just at the lower unauthenticated rate
			// limit. When a token is cached we still use it — higher rate
			// limit, and it's what's needed if the repo is ever private
			// again.
			token, tokErr := auth.NewDefault().Token(auth.HostGitHub, "github.com")
			if tokErr != nil {
				token = ""
			}
			if token != "" {
				if user, _ := auth.NewDefault().User(auth.HostGitHub, "github.com"); user != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "auth:    gh token for %s\n", user)
				}
			}

			// Locate the right asset.
			assetName := update.PickAssetName(rel.TagName)
			tarballURL, ok := rel.FindAsset(assetName)
			if !ok {
				return fmt.Errorf("release %s has no asset matching %s — your platform may not be published", rel.TagName, assetName)
			}
			checksumName := update.ChecksumAssetName(rel.TagName)
			checksumURL, ok := rel.FindAsset(checksumName)
			if !ok {
				return fmt.Errorf("release %s has no checksums file %s", rel.TagName, checksumName)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "asset:   %s\n", assetName)
			fmt.Fprintln(cmd.OutOrStdout(), "downloading…")

			tarPath, gotSha, err := update.DownloadAsset(ctx, tarballURL, token)
			if err != nil {
				return fmt.Errorf("download asset: %w", err)
			}
			defer os.Remove(tarPath)

			// Cosign verification anchors trust in the checksums
			// file; the per-asset sha256 then transitively verifies
			// the tarball. We must verify cosign BEFORE consulting
			// the checksums file's contents — otherwise an attacker
			// who could swap the checksums file alone could feed us
			// a matching-but-malicious sha256.
			checksumsLocal, err := update.DownloadToTemp(ctx, checksumURL, token, "meerkat-checksums-*.txt")
			if err != nil {
				return fmt.Errorf("fetch checksums: %w", err)
			}
			defer os.Remove(checksumsLocal)

			if skipCosign {
				fmt.Fprintln(cmd.OutOrStdout(), "cosign:  SKIPPED (--skip-cosign) — falling back to sha256-only")
			} else {
				sigName, certName := update.CosignAssetNames(checksumName)
				sigURL, _ := rel.FindAsset(sigName)
				certURL, _ := rel.FindAsset(certName)
				sigPath, err := update.DownloadToTemp(ctx, sigURL, token, "meerkat-checksums-*.sig")
				if err != nil {
					return fmt.Errorf("fetch signature: %w", err)
				}
				defer os.Remove(sigPath)
				certPath, err := update.DownloadToTemp(ctx, certURL, token, "meerkat-checksums-*.pem")
				if err != nil {
					return fmt.Errorf("fetch certificate: %w", err)
				}
				defer os.Remove(certPath)

				fmt.Fprintln(cmd.OutOrStdout(), "cosign:  verifying signature on checksums…")
				if err := update.VerifyChecksumSignature(ctx, checksumsLocal, sigPath, certPath); err != nil {
					if errors.Is(err, update.ErrCosignMissing) {
						return fmt.Errorf(
							"cosign binary not found on PATH.\n\n"+
								"Install via `brew install cosign` (or see https://docs.sigstore.dev/system_config/installation/),\n"+
								"or re-run with --skip-cosign to fall back to SHA256-only verification.\n\n"+
								"Original error: %w", err)
					}
					return fmt.Errorf("signature verification FAILED — refusing to install: %w", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "cosign:  ✓ signed by github.com/zegit-zoo/meerkat release workflow")
			}

			expectedSha, err := update.ReadChecksumFor(checksumsLocal, assetName)
			if err != nil {
				return fmt.Errorf("read checksum: %w", err)
			}
			if !strings.EqualFold(gotSha, expectedSha) {
				return fmt.Errorf("checksum mismatch — got %s, expected %s", gotSha, expectedSha)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sha256:  %s ✓\n", gotSha[:16])

			binPath, err := update.ExtractMeerkat(tarPath)
			if err != nil {
				return fmt.Errorf("extract: %w", err)
			}
			defer os.Remove(binPath)

			fmt.Fprintln(cmd.OutOrStdout(), "swapping + re-exec…")
			return update.SwapAndReExec(binPath)
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false,
		"Just report the latest version; don't download or install.")
	cmd.Flags().StringVar(&pinTag, "version", "",
		"Install a specific tag (e.g. v0.4.0) instead of latest.")
	cmd.Flags().BoolVar(&force, "force", false,
		"Re-install even if already on the target version (downgrade-friendly).")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false,
		"Skip the confirmation prompt.")
	cmd.Flags().BoolVar(&skipCosign, "skip-cosign", false,
		"Skip cosign signature verification (NOT recommended — sha256-only).")
	return cmd
}

// versionEq normalises 'v' prefix and snapshot suffixes when
// comparing versions for "are we already there?".
func versionEq(a, b string) bool {
	a = strings.TrimSuffix(strings.TrimPrefix(a, "v"), "-dirty")
	b = strings.TrimSuffix(strings.TrimPrefix(b, "v"), "-dirty")
	return a == b
}
