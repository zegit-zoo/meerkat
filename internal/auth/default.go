package auth

import "fmt"

// defaultProvider is the router returned by NewDefault. It dispatches
// Token and User calls to GhProvider based on the Host argument.
type defaultProvider struct {
	gh GhProvider
}

// NewDefault returns a TokenProvider that dispatches to GhProvider for
// HostGitHub. An unknown Host returns a wrapped ErrNoConfig error.
func NewDefault() TokenProvider {
	return &defaultProvider{}
}

// Token dispatches to the appropriate provider based on host.
func (d *defaultProvider) Token(host Host, domain string) (string, error) {
	switch host {
	case HostGitHub:
		return d.gh.Token(host, domain)
	default:
		return "", fmt.Errorf("%w: unsupported host %q", ErrNoConfig, host)
	}
}

// User dispatches to the appropriate provider based on host.
func (d *defaultProvider) User(host Host, domain string) (string, error) {
	switch host {
	case HostGitHub:
		return d.gh.User(host, domain)
	default:
		return "", fmt.Errorf("%w: unsupported host %q", ErrNoConfig, host)
	}
}
