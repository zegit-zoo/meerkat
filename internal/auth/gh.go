package auth

import (
	"fmt"
	"os/exec"
	"strings"
)

// runGh is the low-level executor for gh CLI commands. It is a package-
// level variable so tests can replace it without invoking the real binary.
var runGh = func(args ...string) (string, error) {
	out, err := exec.Command("gh", args...).Output() //nolint:gosec // args are caller-controlled
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// GhProvider implements TokenProvider for HostGitHub by shelling out to
// the gh CLI (https://cli.github.com). Prerequisite: gh is installed and
// the user has run `gh auth login` at least once.
type GhProvider struct{}

// Token returns the cached access token for the given domain. Pass domain
// "" to default to "github.com". Tokens are obtained via `gh auth token`
// (or `gh auth token --hostname <domain>` for enterprise instances).
func (GhProvider) Token(host Host, domain string) (string, error) {
	if host != HostGitHub {
		return "", fmt.Errorf("%w: GhProvider does not support host %q", ErrNoConfig, host)
	}
	args := []string{"auth", "token"}
	if domain != "" {
		args = append(args, "--hostname", domain)
	}
	tok, err := runGh(args...)
	if err != nil {
		return "", ghErr(err)
	}
	if tok == "" {
		return "", ErrNoToken
	}
	return tok, nil
}

// User returns the authenticated username for the given domain, or "" if
// unknown. Uses `gh api user --jq .login`.
func (GhProvider) User(host Host, domain string) (string, error) {
	if host != HostGitHub {
		return "", fmt.Errorf("%w: GhProvider does not support host %q", ErrNoConfig, host)
	}
	args := []string{"api", "user", "--jq", ".login"}
	if domain != "" {
		args = append(args, "--hostname", domain)
	}
	user, err := runGh(args...)
	if err != nil {
		// Non-fatal — caller treats "" as unknown.
		return "", nil //nolint:nilerr
	}
	return user, nil
}

// ghErr maps exec errors from the gh binary to the canonical auth errors.
func ghErr(err error) error {
	if err == nil {
		return nil
	}
	// exec.Error means the binary wasn't found on PATH.
	var execErr *exec.Error
	if isExecError(err, &execErr) {
		return fmt.Errorf("%w: gh binary not found on PATH (install from https://cli.github.com)", ErrNoConfig)
	}
	// ExitError from gh usually means not authenticated.
	msg := err.Error()
	if strings.Contains(msg, "not logged into") ||
		strings.Contains(msg, "authentication") ||
		strings.Contains(msg, "no credentials") {
		return fmt.Errorf("%w: %s", ErrNoToken, msg)
	}
	return fmt.Errorf("%w: gh: %s", ErrNoConfig, msg)
}

func isExecError(err error, target **exec.Error) bool {
	e, ok := err.(*exec.Error) //nolint:errorlint // direct type assertion is fine here
	if ok {
		*target = e
	}
	return ok
}
