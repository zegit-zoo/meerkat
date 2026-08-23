package auth

import (
	"errors"
	"testing"
)

// stubProvider is a minimal TokenProvider for exercising
// defaultProvider's fallback ordering without shelling out to gh or
// touching the real OS keyring.
type stubProvider struct {
	token    string
	tokenErr error
	user     string
	userErr  error
}

func (s stubProvider) Token(Host, string) (string, error) { return s.token, s.tokenErr }
func (s stubProvider) User(Host, string) (string, error)  { return s.user, s.userErr }

// TestDefaultProvider_PrefersGhWhenAvailable: gh's token wins even if
// the keyring also has one — gh is the primary mechanism, the keyring
// is strictly a fallback.
func TestDefaultProvider_PrefersGhWhenAvailable(t *testing.T) {
	d := &defaultProvider{
		gh:      stubProvider{token: "gh-token"},
		keyring: stubProvider{token: "keyring-token"},
	}
	tok, err := d.Token(HostGitHub, "")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "gh-token" {
		t.Errorf("token = %q, want gh-token (gh should win over keyring)", tok)
	}
}

// TestDefaultProvider_FallsBackToKeyringOnGhError covers the primary
// scenario from issue #13: gh isn't installed/authenticated (e.g. a
// minimal container or CI runner), but a token was provisioned into
// the OS keyring instead.
func TestDefaultProvider_FallsBackToKeyringOnGhError(t *testing.T) {
	d := &defaultProvider{
		gh:      stubProvider{tokenErr: ErrNoConfig},
		keyring: stubProvider{token: "keyring-token"},
	}
	tok, err := d.Token(HostGitHub, "")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "keyring-token" {
		t.Errorf("token = %q, want keyring-token", tok)
	}
}

// TestDefaultProvider_FallsBackToKeyringOnGhEmptyToken: gh succeeds
// (no error) but has nothing cached — still falls through to the
// keyring, since an empty token is as useless as an error.
func TestDefaultProvider_FallsBackToKeyringOnGhEmptyToken(t *testing.T) {
	d := &defaultProvider{
		gh:      stubProvider{token: ""},
		keyring: stubProvider{token: "keyring-token"},
	}
	tok, err := d.Token(HostGitHub, "")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "keyring-token" {
		t.Errorf("token = %q, want keyring-token", tok)
	}
}

// TestDefaultProvider_DegradesGracefullyWhenBothFail is the "never
// hard-fail" guarantee end to end: neither gh nor the keyring have
// anything to offer (or the keyring itself errored), so the caller
// gets an error it can treat as "use anonymous access" — never a
// panic, never something that blocks the update.
func TestDefaultProvider_DegradesGracefullyWhenBothFail(t *testing.T) {
	d := &defaultProvider{
		gh:      stubProvider{tokenErr: ErrNoConfig},
		keyring: stubProvider{tokenErr: errors.New("keyring: no Secret Service running")},
	}
	tok, err := d.Token(HostGitHub, "")
	if tok != "" {
		t.Errorf("token = %q, want empty on double failure", tok)
	}
	if err == nil {
		t.Fatal("expected a non-nil error when both mechanisms fail")
	}
}

// TestDefaultProvider_UnknownHost confirms the host-support check
// still gates dispatch: an unsupported host returns ErrNoConfig
// regardless of what gh/keyring would have answered.
func TestDefaultProvider_UnknownHost(t *testing.T) {
	d := &defaultProvider{
		gh:      stubProvider{token: "gh-token"},
		keyring: stubProvider{token: "keyring-token"},
	}
	_, err := d.Token(Host("bitbucket"), "")
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("expected ErrNoConfig for unknown host, got %v", err)
	}
}

// TestDefaultProvider_UserFallsBackToKeyring: keyring's User is
// always ("", nil), so this mostly documents that gh's answer wins and
// an unknown gh user still resolves through to keyring's "unknown".
func TestDefaultProvider_UserFallsBackToKeyring(t *testing.T) {
	d := &defaultProvider{
		gh:      stubProvider{userErr: ErrNoConfig},
		keyring: KeyringProvider{},
	}
	user, err := d.User(HostGitHub, "")
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if user != "" {
		t.Errorf("user = %q, want empty", user)
	}
}

func TestNewDefault_UsesRealProviders(t *testing.T) {
	d := NewDefault()
	dp, ok := d.(*defaultProvider)
	if !ok {
		t.Fatalf("NewDefault() = %T, want *defaultProvider", d)
	}
	if _, ok := dp.gh.(GhProvider); !ok {
		t.Errorf("gh provider = %T, want GhProvider", dp.gh)
	}
	if _, ok := dp.keyring.(KeyringProvider); !ok {
		t.Errorf("keyring provider = %T, want KeyringProvider", dp.keyring)
	}
}
