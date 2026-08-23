package auth

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// keyringService is the "service" name update credentials are filed
// under in the OS-native credential store. Kept distinct from the
// "meerkat" name used elsewhere so a user (or a provisioning script)
// can tell at a glance, in Keychain Access / seahorse / Credential
// Manager, that this specific entry is what `mk update` reads.
const keyringService = "mk-update"

// keyringBackend abstracts the OS keyring lookup so tests can inject a
// fake and never touch the real macOS Keychain, Linux Secret Service
// (D-Bus), or Windows Credential Manager.
type keyringBackend interface {
	Get(service, user string) (string, error)
}

// systemKeyring adapts github.com/zalando/go-keyring's package-level
// functions — which talk to whichever OS-native store is available —
// to the keyringBackend interface.
type systemKeyring struct{}

func (systemKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

// keyringGet is a package-level var (mirroring runGh in gh.go) purely
// so tests can swap in a fake backend without linking against the
// real OS keyring.
var keyringGet keyringBackend = systemKeyring{}

// KeyringProvider implements TokenProvider by reading a token from the
// OS-native credential store via go-keyring. It exists as a fallback
// for environments where the gh CLI isn't installed or authenticated —
// e.g. minimal containers, CI runners, or downstream fork deployments
// that provision an update token straight into the platform keyring
// instead of expecting `gh auth login` to have been run.
//
// Every failure mode — no keyring backend on this platform/session
// (e.g. no D-Bus Secret Service, no Windows Credential Manager),
// access denied, or simply no entry stored — is mapped to one of the
// package's own sentinel errors and never panics or otherwise hard-
// fails. Callers (see defaultProvider in default.go) treat any error
// from this provider as "no credential here" and fall through to
// unauthenticated access; a broken or absent keyring must never break
// `mk update`.
type KeyringProvider struct{}

// Token returns the token stored in the OS keyring under this
// provider's fixed service name, keyed by domain (or "github.com" for
// the default/empty domain — mirroring GhProvider.Token).
func (KeyringProvider) Token(host Host, domain string) (string, error) {
	if host != HostGitHub {
		return "", fmt.Errorf("%w: KeyringProvider does not support host %q", ErrNoConfig, host)
	}
	tok, err := keyringGet.Get(keyringService, keyringAccount(domain))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrNoToken
		}
		// Anything else (unsupported platform, locked/denied session,
		// no Secret Service running, ...) is not fatal — it just means
		// this fallback has nothing to offer, same as "not configured".
		return "", fmt.Errorf("%w: keyring: %s", ErrNoConfig, err.Error())
	}
	if tok == "" {
		return "", ErrNoToken
	}
	return tok, nil
}

// User always returns "", nil: the keyring fallback only stores a
// bearer token, with no companion-username convention. Non-fatal —
// callers already treat "" as "unknown", same as GhProvider.User's own
// failure path.
func (KeyringProvider) User(Host, string) (string, error) {
	return "", nil
}

// keyringAccount normalizes domain into the keyring "user"/account
// key, defaulting empty (github.com's shorthand, mirroring GhProvider)
// to the explicit hostname so entries are addressed consistently.
func keyringAccount(domain string) string {
	if domain == "" {
		return "github.com"
	}
	return domain
}
