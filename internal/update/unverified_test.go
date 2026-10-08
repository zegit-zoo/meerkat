package update

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfirmUnverified(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		interactive bool
		input       string
		wantErr     error
		wantPrompt  bool
	}{
		{"no env, non-interactive", nil, false, "", ErrUnverifiedNotAllowed, false},
		{"no env, interactive never prompts", nil, true, "yes\n", ErrUnverifiedNotAllowed, false},
		{"env not exactly 1", map[string]string{AllowUnverifiedEnv: "true"}, false, "", ErrUnverifiedNotAllowed, false},
		{"env set, non-interactive passes", map[string]string{AllowUnverifiedEnv: "1"}, false, "", nil, false},
		{"env set, interactive yes", map[string]string{AllowUnverifiedEnv: "1"}, true, "yes\n", nil, true},
		{"env set, interactive y is not enough", map[string]string{AllowUnverifiedEnv: "1"}, true, "y\n", ErrUnverifiedDeclined, true},
		{"env set, interactive empty (EOF)", map[string]string{AllowUnverifiedEnv: "1"}, true, "", ErrUnverifiedDeclined, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := ConfirmUnverified(env(tc.env), tc.interactive, strings.NewReader(tc.input), &out)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := out.Len() > 0; got != tc.wantPrompt {
				t.Fatalf("prompted = %v, want %v", got, tc.wantPrompt)
			}
		})
	}
}

func TestMissingCosignErrorDoesNotSuggestSkipping(t *testing.T) {
	err := MissingCosignError(ErrCosignMissing)
	if !errors.Is(err, ErrCosignMissing) {
		t.Fatalf("cause not wrapped: %v", err)
	}
	msg := err.Error()
	for _, bad := range []string{"--skip-cosign", "skip", "SHA256-only", "sha256-only", AllowUnverifiedEnv} {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(bad)) {
			t.Errorf("error text mentions %q:\n%s", bad, msg)
		}
	}
	if !strings.Contains(msg, "brew install cosign") {
		t.Errorf("error text lacks install hint:\n%s", msg)
	}
}

func TestSkipNoticeAndInstallMessagesDoNotClaimVerification(t *testing.T) {
	if !strings.Contains(UnverifiedNotice, "NOT verified") {
		t.Errorf("notice must state the signature is not verified: %s", UnverifiedNotice)
	}
	for _, msg := range []string{
		wrapSudoInstallError("/x/mk", errors.New("e")).Error(),
		permissionDeniedError("/x/mk", "stage", errors.New("e")).Error(),
	} {
		if strings.Contains(msg, "cosign-signature-verified") {
			t.Errorf("message claims cosign verification unconditionally:\n%s", msg)
		}
	}
}
