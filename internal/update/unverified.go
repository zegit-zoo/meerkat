package update

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// AllowUnverifiedEnv is the environment variable that must be set to
// "1" in addition to --skip-cosign before `mk update` will install a
// release without verifying its signature. A flag alone is not enough:
// a flag is something a tool's error text or a copy-pasted command line
// can talk a user into, an environment variable is a deliberate act.
const AllowUnverifiedEnv = "MEERKAT_UPDATE_ALLOW_UNVERIFIED"

// CosignInstallHint is the remedy shown when cosign is missing. It
// deliberately offers no way around verification.
const CosignInstallHint = "Install cosign (`brew install cosign`, or see " +
	"https://docs.sigstore.dev/cosign/system_config/installation/) and re-run `mk update`."

// ErrUnverifiedNotAllowed is returned when --skip-cosign is given
// without the explicit environment opt-in.
var ErrUnverifiedNotAllowed = errors.New(
	"--skip-cosign also requires " + AllowUnverifiedEnv + "=1 in the environment: " +
		"without a verified signature the downloaded binary is only as trustworthy as the release page itself")

// ErrUnverifiedDeclined is returned when the interactive confirmation
// is answered with anything other than "yes".
var ErrUnverifiedDeclined = errors.New("install without signature verification declined")

// UnverifiedNotice is printed whenever a skip is in effect.
const UnverifiedNotice = "cosign:  SKIPPED — signature NOT verified; the checksums file comes from the same " +
	"release as the binary, so sha256 only detects corruption, not tampering"

// ConfirmUnverified gates the skip of signature verification. The
// environment variable named by AllowUnverifiedEnv must be "1"; when
// interactive is true (stdin is a terminal) the user must additionally
// type "yes". Non-interactive callers (CI) pass on the env var alone.
func ConfirmUnverified(getenv func(string) string, interactive bool, in io.Reader, out io.Writer) error {
	if getenv(AllowUnverifiedEnv) != "1" {
		return ErrUnverifiedNotAllowed
	}
	if !interactive {
		return nil
	}
	fmt.Fprintf(out, "\nWARNING: signature verification is being skipped. Nothing proves this binary\n"+
		"came from the meerkat release workflow. Type 'yes' to install anyway: ")
	ans, _ := bufio.NewReader(in).ReadString('\n')
	if strings.TrimSpace(ans) != "yes" {
		return ErrUnverifiedDeclined
	}
	return nil
}

// StdinIsTerminal reports whether stdin is an interactive terminal.
func StdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// MissingCosignError is the error `mk update` returns when cosign is not
// installed and no (gated) skip is in effect. It names the remedy and
// never the bypass.
func MissingCosignError(cause error) error {
	return fmt.Errorf("cosign binary not found on PATH; refusing to install an unverified binary.\n\n%s\n\nOriginal error: %w",
		CosignInstallHint, cause)
}
