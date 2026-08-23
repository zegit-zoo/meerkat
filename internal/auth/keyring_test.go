package auth

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// fakeKeyring is an in-memory keyringBackend used so tests never touch
// the real OS keyring (macOS Keychain, Secret Service, Credential
// Manager) — none of which are guaranteed to exist, be unlocked, or be
// writable in a CI sandbox.
type fakeKeyring struct {
	get func(service, user string) (string, error)
}

func (f fakeKeyring) Get(service, user string) (string, error) {
	return f.get(service, user)
}

func withFakeKeyring(t *testing.T, get func(service, user string) (string, error)) {
	t.Helper()
	orig := keyringGet
	keyringGet = fakeKeyring{get: get}
	t.Cleanup(func() { keyringGet = orig })
}

func TestKeyringProvider_Success(t *testing.T) {
	withFakeKeyring(t, func(service, user string) (string, error) {
		if service != keyringService {
			t.Errorf("service = %q, want %q", service, keyringService)
		}
		if user != "github.com" {
			t.Errorf("user = %q, want github.com", user)
		}
		return "kr_testtoken456", nil
	})

	p := KeyringProvider{}
	tok, err := p.Token(HostGitHub, "")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "kr_testtoken456" {
		t.Errorf("token = %q", tok)
	}
}

func TestKeyringProvider_DomainPassthrough(t *testing.T) {
	withFakeKeyring(t, func(service, user string) (string, error) {
		if user != "github.example.com" {
			t.Errorf("user = %q, want github.example.com", user)
		}
		return "kr_enterprise", nil
	})

	p := KeyringProvider{}
	tok, err := p.Token(HostGitHub, "github.example.com")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "kr_enterprise" {
		t.Errorf("token = %q", tok)
	}
}

func TestKeyringProvider_NotFound(t *testing.T) {
	withFakeKeyring(t, func(service, user string) (string, error) {
		return "", keyring.ErrNotFound
	})

	p := KeyringProvider{}
	_, err := p.Token(HostGitHub, "")
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("expected ErrNoToken for missing entry, got %v", err)
	}
}

// TestKeyringProvider_BackendUnavailable is the core "never hard-fail"
// guarantee: an unsupported-platform / no-Secret-Service / locked-
// session error from the OS keyring must degrade to a wrapped
// ErrNoConfig, exactly like "not configured" — never a panic, never a
// distinguishable-as-fatal error type.
func TestKeyringProvider_BackendUnavailable(t *testing.T) {
	withFakeKeyring(t, func(service, user string) (string, error) {
		return "", keyring.ErrUnsupportedPlatform
	})

	p := KeyringProvider{}
	_, err := p.Token(HostGitHub, "")
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("expected ErrNoConfig for unavailable backend, got %v", err)
	}
}

func TestKeyringProvider_EmptyTokenTreatedAsNotFound(t *testing.T) {
	withFakeKeyring(t, func(service, user string) (string, error) {
		return "", nil
	})

	p := KeyringProvider{}
	_, err := p.Token(HostGitHub, "")
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("expected ErrNoToken for empty token, got %v", err)
	}
}

func TestKeyringProvider_WrongHost(t *testing.T) {
	p := KeyringProvider{}
	_, err := p.Token(Host("gitlab"), "")
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("expected ErrNoConfig for wrong host, got %v", err)
	}
}

func TestKeyringProvider_UserAlwaysUnknown(t *testing.T) {
	p := KeyringProvider{}
	user, err := p.User(HostGitHub, "")
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if user != "" {
		t.Errorf("user = %q, want empty (keyring has no username convention)", user)
	}
}
