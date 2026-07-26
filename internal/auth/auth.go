// Package auth defines the shared credential contract for reaching git
// hosts by borrowing the OAuth token cached by the user's host CLI
// (gh). It never mints or stores its own tokens.
//
// This package is intentionally self-contained and dependency-light so
// it can later be lifted into a shared module.
package auth

import "errors"

// Host identifies which git hosting provider a credential belongs to.
type Host string

const (
	// HostGitHub identifies github.com (and GitHub Enterprise instances).
	HostGitHub Host = "github"
)

var (
	// ErrNoConfig is returned when the host CLI has no configuration
	// file — the user needs to run its login command (e.g.
	// `gh auth login`) first.
	ErrNoConfig = errors.New("auth: no CLI configuration found")
	// ErrNoToken is returned when the configuration exists but contains
	// no cached token for the requested host/domain.
	ErrNoToken = errors.New("auth: no cached token found")
)

// TokenProvider yields credentials for reaching a git host by borrowing
// the OAuth token cached by the user's host CLI (gh). It never mints or
// stores its own tokens.
type TokenProvider interface {
	// Token returns a bearer token for the host at the given domain.
	// domain may be "" for the provider's default (github.com).
	Token(host Host, domain string) (string, error)
	// User returns the authenticated username, or "" if unknown.
	User(host Host, domain string) (string, error)
}
