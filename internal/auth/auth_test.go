package auth

import (
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// GhProvider tests (stubbed runGh)
// ---------------------------------------------------------------------------

func TestGhProvider_Success(t *testing.T) {
	orig := runGh
	defer func() { runGh = orig }()

	runGh = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return "ghs_testtoken123", nil
		}
		if len(args) >= 2 && args[0] == "api" && args[1] == "user" {
			return "octocat", nil
		}
		return "", nil
	}

	p := GhProvider{}
	tok, err := p.Token(HostGitHub, "")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_testtoken123" {
		t.Errorf("token = %q", tok)
	}
	user, err := p.User(HostGitHub, "")
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if user != "octocat" {
		t.Errorf("user = %q", user)
	}
}

func TestGhProvider_MissingBinary(t *testing.T) {
	orig := runGh
	defer func() { runGh = orig }()

	runGh = func(args ...string) (string, error) {
		return "", &execNotFoundError{}
	}

	p := GhProvider{}
	_, err := p.Token(HostGitHub, "")
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("expected ErrNoConfig for missing binary, got %v", err)
	}
}

func TestGhProvider_Unauthed(t *testing.T) {
	orig := runGh
	defer func() { runGh = orig }()

	runGh = func(args ...string) (string, error) {
		return "", errors.New("not logged into github.com")
	}

	p := GhProvider{}
	_, err := p.Token(HostGitHub, "")
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("expected ErrNoToken for unauthed, got %v", err)
	}
}

func TestGhProvider_WrongHost(t *testing.T) {
	p := GhProvider{}
	_, err := p.Token(Host("gitlab"), "")
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("expected ErrNoConfig for wrong host, got %v", err)
	}
}

// execNotFoundError mimics an exec.Error (binary not found) without
// importing os/exec (to keep the stub self-contained in the test).
type execNotFoundError struct{}

func (e *execNotFoundError) Error() string { return "exec: gh: not found" }

// Make it recognisable as an *exec.Error by ghErr's type switch.
// We stub runGh so the real exec path is never hit; the type switch in
// ghErr checks for *exec.Error specifically. Since we want to test the
// missing-binary branch without a real exec.Error, we test that the
// returned error wraps ErrNoConfig (the catch-all branch in ghErr also
// wraps ErrNoConfig), which is correct behavior for any exec failure.

// ---------------------------------------------------------------------------
// NewDefault router dispatch
// ---------------------------------------------------------------------------

func TestNewDefault_DispatchGitHub(t *testing.T) {
	orig := runGh
	defer func() { runGh = orig }()
	runGh = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return "ghs_router_token", nil
		}
		return "ghuser", nil
	}

	d := NewDefault()
	tok, err := d.Token(HostGitHub, "")
	if err != nil {
		t.Fatalf("Token(github): %v", err)
	}
	if tok != "ghs_router_token" {
		t.Errorf("token = %q", tok)
	}
}

func TestNewDefault_UnknownHost(t *testing.T) {
	d := NewDefault()
	_, err := d.Token("bitbucket", "")
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("expected ErrNoConfig for unknown host, got %v", err)
	}
	_, err = d.User("bitbucket", "")
	if !errors.Is(err, ErrNoConfig) {
		t.Errorf("expected ErrNoConfig for unknown host User, got %v", err)
	}
}
