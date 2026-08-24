// Command meerkat-bootstrap installs a github.com/zegit-zoo/meerkat
// release at an arbitrary destination path, independent of whatever
// update mechanism (if any) the binary already there understands.
//
// It exists for exactly one situation: a meerkat binary built by a
// downstream fork -- whose own updater queries a different release
// feed and trusts a different signing identity -- cannot discover
// zegit-zoo/meerkat releases on its own, no matter how new they are.
// meerkat-bootstrap crosses that gap once; every subsequent
// `mk update` then talks to zegit-zoo/meerkat directly. See
// docs/design/upstream-migration.md#converging-from-a-downstream-fork.
//
// meerkat-bootstrap deliberately does not reimplement any of the
// security-sensitive parts of that: OS/arch asset selection, the
// redirect allowlist, checksum + Sigstore verification, and the
// atomic backup/rollback install dance all come from internal/update
// verbatim -- the exact same code `mk update` itself runs.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Set via -X ldflags at build time (see .goreleaser.yaml and the
// Makefile's `build` target's mirror-build note); "dev" for
// go build/go test with no ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "meerkat-bootstrap:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "meerkat-bootstrap",
		Short:         "Verified, one-time bootstrap installer for zegit-zoo/meerkat releases",
		Version:       fmt.Sprintf("%s (%s) built %s", version, commit, date),
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `meerkat-bootstrap installs a zegit-zoo/meerkat release at an arbitrary
destination path, independent of whatever update mechanism (if any) the
binary already there understands.

It exists for exactly one situation: a meerkat binary built by a
downstream fork -- whose own updater queries a different release feed and
trusts a different signing identity -- cannot discover zegit-zoo/meerkat
releases on its own, regardless of how new they are. meerkat-bootstrap
crosses that gap once; every subsequent "mk update" then talks to
zegit-zoo/meerkat directly.

It reuses mk update's own verification and install code unchanged:
OS/arch asset selection, the redirect allowlist, cosign signature
verification (pinned to the release.yml GitHub Actions workflow
identity), per-asset SHA-256 verification, and an atomic write-to-temp
plus rename install with automatic backup/rollback.

See docs/design/upstream-migration.md#converging-from-a-downstream-fork.`,
	}
	cmd.AddCommand(newInstallCmd())
	return cmd
}
