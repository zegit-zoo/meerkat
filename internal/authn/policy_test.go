package authn_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authn"
	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
)

// emailPolicyVerifier is a verifier whose only rule selects on `emails:`.
func emailPolicyVerifier(t *testing.T, iss *authntest.Issuer, p authz.Provider, logger *slog.Logger) *authn.Verifier {
	t.Helper()
	p.Issuer, p.Audience = iss.URL, testAudience
	v, err := authn.NewVerifier(context.Background(), authn.Options{
		Config: &authz.Config{
			Resource:  testResource,
			Providers: []authz.Provider{p},
			Rules:     []authz.Rule{{Name: "by-email", Emails: []string{"alice@example.com"}, Collections: []string{"runbooks"}}},
		},
		HTTPClient: iss.Client(),
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func TestVerify_UnverifiedEmailNeverGrants(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := emailPolicyVerifier(t, iss, authz.Provider{}, nil)
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  bool
	}{
		{"email_verified true", map[string]any{"email_verified": true}, true},
		{"email_verified absent", nil, true},
		{"email_verified false", map[string]any{"email_verified": false}, false},
		{"email_verified false as a string", map[string]any{"email_verified": "false"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := iss.Token(t, authntest.Claims{Subject: "u", Audience: testAudience, Email: "alice@example.com", Extra: tc.extra})
			id, err := v.Verify(context.Background(), raw)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got := v.Policy().Evaluate(id).CanRead("runbooks"); got != tc.want {
				t.Errorf("CanRead = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerify_TokenTypeAndAZP(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{
		Issuer: iss.URL, Audience: testAudience,
		TokenType: authz.TokenTypeAccess, AllowedAZP: []string{"client-1"},
	})
	for _, tc := range []struct {
		name string
		c    authntest.Claims
		ok   bool
	}{
		{"at+jwt typ", authntest.Claims{Type: "at+jwt", Extra: map[string]any{"azp": "client-1"}}, true},
		{"application/at+jwt typ", authntest.Claims{Type: "application/at+jwt", Extra: map[string]any{"azp": "client-1"}}, true},
		{"scp claim, no typ", authntest.Claims{Extra: map[string]any{"scp": "read", "azp": "client-1"}}, true},
		{"roles claim, appid", authntest.Claims{Extra: map[string]any{"roles": []string{"r"}, "appid": "client-1"}}, true},
		{"id token shape", authntest.Claims{Extra: map[string]any{"azp": "client-1"}}, false},
		{"wrong client", authntest.Claims{Type: "at+jwt", Extra: map[string]any{"azp": "other"}}, false},
		{"no azp", authntest.Claims{Type: "at+jwt"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.Subject, tc.c.Audience = "u", testAudience
			_, err := v.Verify(context.Background(), iss.Token(t, tc.c))
			if (err == nil) != tc.ok {
				t.Errorf("Verify err = %v, want ok=%v", err, tc.ok)
			}
		})
	}

	// Without the settings an ID-token-shaped token is accepted, as before.
	plain := newVerifier(t, iss, authz.Provider{Issuer: iss.URL, Audience: testAudience})
	if _, err := plain.Verify(context.Background(), iss.Token(t, authntest.Claims{Subject: "u", Audience: testAudience})); err != nil {
		t.Errorf("default provider rejected a plain token: %v", err)
	}
}

func TestNewVerifier_WarnsAboutRiskyProviderSettings(t *testing.T) {
	iss := authntest.NewIssuer(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	emailPolicyVerifier(t, iss, authz.Provider{
		SkipIssuerCheck: true, RequireTenant: "acme",
		Claims: authz.ClaimMapping{Email: "preferred_username"},
	}, logger)
	out := buf.String()
	for _, want := range []string{"skip_issuer_check", "preferred_username"} {
		if !strings.Contains(out, want) {
			t.Errorf("no startup warning mentioning %q; log:\n%s", want, out)
		}
	}

	buf.Reset()
	emailPolicyVerifier(t, iss, authz.Provider{}, logger)
	if buf.Len() != 0 {
		t.Errorf("a default provider must warn about nothing, got:\n%s", buf.String())
	}
}

// TestGate_NilGrantsDenyWhenEnabled: a verifier that has providers but
// lost its policy must deny, not fall through to "no policy, no
// restriction".
func TestGate_NilGrantsDenyWhenEnabled(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{Issuer: iss.URL, Audience: testAudience})
	authn.DropPolicyForTest(v)
	gate := authn.NewGate(v, authn.MetadataURL(testResource))

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+iss.Token(t, authntest.Claims{Subject: "u", Audience: testAudience}))
	reached := false
	rec := httptest.NewRecorder()
	gate.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	if reached || rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, handler reached = %v; want 403 and not reached", rec.Code, reached)
	}
}
