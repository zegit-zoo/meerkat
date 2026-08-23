package auth

import "fmt"

// defaultProvider is the router returned by NewDefault. It dispatches
// to GhProvider based on the Host argument, falling back to
// KeyringProvider when gh has nothing to offer.
type defaultProvider struct {
	gh      TokenProvider
	keyring TokenProvider
}

// NewDefault returns a TokenProvider that, for HostGitHub, tries the
// cached gh CLI token first and falls back to the OS keyring
// (KeyringProvider) if gh isn't installed, isn't authenticated, or
// simply has no token cached. An unknown Host returns a wrapped
// ErrNoConfig error before either mechanism is tried.
//
// The fallback exists for environments without a working gh CLI —
// minimal containers, CI runners, or downstream fork deployments that
// provision an update token directly into the platform keyring. Either
// mechanism failing (including the keyring being unavailable or
// erroring) degrades to "no token" rather than a hard failure: update
// flows always have unauthenticated GitHub API access as a last
// resort, since the release repository is public.
func NewDefault() TokenProvider {
	return &defaultProvider{gh: GhProvider{}, keyring: KeyringProvider{}}
}

// Token dispatches to the appropriate provider based on host, trying
// gh before the keyring fallback for HostGitHub.
func (d *defaultProvider) Token(host Host, domain string) (string, error) {
	switch host {
	case HostGitHub:
		return firstToken(host, domain, d.gh.Token, d.keyring.Token)
	default:
		return "", fmt.Errorf("%w: unsupported host %q", ErrNoConfig, host)
	}
}

// User dispatches to the appropriate provider based on host, trying gh
// before the keyring fallback for HostGitHub.
//
// Unlike Token, an empty User result is a normal, non-error outcome
// (TokenProvider.User's contract: "" means unknown, not "failed") — so
// this doesn't use firstToken's error-forcing fallback logic. It
// simply prefers gh's answer when gh has one, and otherwise returns
// whatever the keyring provider says (which, for KeyringProvider, is
// always "", nil: the keyring only stores a token, with no companion
// username convention).
func (d *defaultProvider) User(host Host, domain string) (string, error) {
	switch host {
	case HostGitHub:
		if user, err := d.gh.User(host, domain); err == nil && user != "" {
			return user, nil
		}
		return d.keyring.User(host, domain)
	default:
		return "", fmt.Errorf("%w: unsupported host %q", ErrNoConfig, host)
	}
}

// firstToken tries each provider function in order and returns the
// first non-empty, error-free result. If every provider errors or
// comes back empty, it returns the last error seen (or ErrNoToken if
// none of them even returned an error, just an empty string) — never
// panics, never blocks the caller from falling through to
// unauthenticated access.
func firstToken(host Host, domain string, providers ...func(Host, string) (string, error)) (string, error) {
	var lastErr error
	for _, p := range providers {
		val, err := p(host, domain)
		if err == nil && val != "" {
			return val, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoToken
	}
	return "", lastErr
}
