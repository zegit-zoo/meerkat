package authn_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/authn"
	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
)

const testAudience = "api://meerkat"

func TestVerify_AcceptsAWellFormedToken(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{Issuer: iss.URL, Audience: testAudience})

	raw := iss.Token(t, authntest.Claims{
		Subject:  "user-1",
		Audience: testAudience,
		Email:    "alice@example.com",
		Groups:   []string{"sre", "leads"},
		Tenant:   "acme",
	})
	id, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Subject != "user-1" {
		t.Errorf("Subject = %q", id.Subject)
	}
	if id.Issuer != iss.URL {
		t.Errorf("Issuer = %q, want %q", id.Issuer, iss.URL)
	}
	if id.Email != "alice@example.com" {
		t.Errorf("Email = %q", id.Email)
	}
	if id.Tenant != "acme" {
		t.Errorf("Tenant = %q", id.Tenant)
	}
	if len(id.Groups) != 2 || id.Groups[0] != "sre" {
		t.Errorf("Groups = %v", id.Groups)
	}
}

func TestVerify_Rejections(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{Issuer: iss.URL, Audience: testAudience})

	t.Run("empty token", func(t *testing.T) {
		if _, err := v.Verify(context.Background(), ""); !errors.Is(err, authn.ErrNoToken) {
			t.Fatalf("err = %v, want ErrNoToken", err)
		}
	})

	for _, tc := range []struct {
		name  string
		token func() string
	}{
		{"wrong audience", func() string {
			return iss.Token(t, authntest.Claims{Subject: "u", Audience: "api://someone-else"})
		}},
		{"expired", func() string {
			return iss.Token(t, authntest.Claims{
				Subject: "u", Audience: testAudience,
				IssuedAt: time.Now().Add(-2 * time.Hour),
				Expiry:   time.Now().Add(-time.Hour),
			})
		}},
		{"wrong issuer", func() string {
			return iss.Token(t, authntest.Claims{
				Subject: "u", Audience: testAudience, Issuer: "https://evil.example.com",
			})
		}},
		{"forged signature", func() string {
			return iss.TokenSignedByOther(t, authntest.Claims{Subject: "u", Audience: testAudience})
		}},
		{"not a jwt", func() string { return "not-a-token" }},
		{"no subject", func() string {
			return iss.Token(t, authntest.Claims{Audience: testAudience})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tc.token())
			if !errors.Is(err, authn.ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerify_RequireTenant(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{
		Issuer: iss.URL, Audience: testAudience, RequireTenant: "acme",
	})

	ok := iss.Token(t, authntest.Claims{Subject: "u", Audience: testAudience, Tenant: "acme"})
	if _, err := v.Verify(context.Background(), ok); err != nil {
		t.Fatalf("matching tenant should verify: %v", err)
	}
	bad := iss.Token(t, authntest.Claims{Subject: "u", Audience: testAudience, Tenant: "other"})
	if _, err := v.Verify(context.Background(), bad); !errors.Is(err, authn.ErrInvalidToken) {
		t.Fatal("a token from another tenant must be refused")
	}
	missing := iss.Token(t, authntest.Claims{Subject: "u", Audience: testAudience})
	if _, err := v.Verify(context.Background(), missing); !errors.Is(err, authn.ErrInvalidToken) {
		t.Fatal("a token with no tenant claim must be refused when a tenant is required")
	}
}

func TestVerify_CustomClaimMapping(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{
		Issuer:   iss.URL,
		Audience: testAudience,
		Claims: authz.ClaimMapping{
			Groups: "roles",
			Email:  "preferred_username",
			Tenant: "org_id",
		},
	})

	raw := iss.Token(t, authntest.Claims{
		Subject:  "u",
		Audience: testAudience,
		// The default-named claims are present too, and must be IGNORED:
		// a mapping says which claim is authoritative.
		Email:  "ignored@example.com",
		Groups: []string{"ignored-group"},
		Tenant: "ignored-tenant",
		Extra: map[string]any{
			"roles":              []string{"kb-readers"},
			"preferred_username": "alice@example.com",
			"org_id":             "acme",
		},
	})
	id, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(id.Groups) != 1 || id.Groups[0] != "kb-readers" {
		t.Errorf("Groups = %v, want the mapped 'roles' claim", id.Groups)
	}
	if id.Email != "alice@example.com" {
		t.Errorf("Email = %q, want the mapped 'preferred_username' claim", id.Email)
	}
	if id.Tenant != "acme" {
		t.Errorf("Tenant = %q, want the mapped 'org_id' claim", id.Tenant)
	}
}

func TestVerify_ScalarGroupClaim(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{Issuer: iss.URL, Audience: testAudience})

	raw := iss.Token(t, authntest.Claims{
		Subject: "u", Audience: testAudience,
		Extra: map[string]any{"groups": "just-one-group"},
	})
	id, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(id.Groups) != 1 || id.Groups[0] != "just-one-group" {
		t.Errorf("Groups = %v — a bare string group claim should be accepted", id.Groups)
	}
}

func TestVerify_MultipleIssuers(t *testing.T) {
	a := authntest.NewIssuer(t)
	b := authntest.NewIssuer(t)

	// One HTTP client that can reach both httptest servers. Both use the
	// default transport, so either client works.
	v, err := authn.NewVerifier(context.Background(), authn.Options{
		Config: &authz.Config{
			Resource: "https://mcp.example.com/mcp",
			Providers: []authz.Provider{
				{Issuer: a.URL, Audience: testAudience},
				{Issuer: b.URL, Audience: testAudience},
			},
		},
		HTTPClient: a.Client(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	for _, iss := range []*authntest.Issuer{a, b} {
		raw := iss.Token(t, authntest.Claims{Subject: "u", Audience: testAudience})
		id, verr := v.Verify(context.Background(), raw)
		if verr != nil {
			t.Fatalf("token from %s: %v", iss.URL, verr)
		}
		if id.Issuer != iss.URL {
			t.Errorf("Issuer = %q, want %q", id.Issuer, iss.URL)
		}
	}
}

func TestNewVerifier_UnreachableIssuerFailsAtStartup(t *testing.T) {
	// A server that 404s discovery stands in for a misconfigured or
	// down IdP. Startup must fail, not degrade into intermittent 401s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, err := authn.NewVerifier(context.Background(), authn.Options{
		Config: &authz.Config{
			Resource:  "https://mcp.example.com/mcp",
			Providers: []authz.Provider{{Issuer: srv.URL, Audience: testAudience}},
		},
		HTTPClient: srv.Client(),
	})
	if err == nil {
		t.Fatal("NewVerifier should fail when discovery fails")
	}
	if !strings.Contains(err.Error(), "oidc discovery") {
		t.Errorf("error should name discovery: %v", err)
	}
}

func TestNewVerifier_Disabled(t *testing.T) {
	v, err := authn.NewVerifier(context.Background(), authn.Options{Config: nil})
	if err != nil {
		t.Fatalf("NewVerifier(nil): %v", err)
	}
	if v.Enabled() {
		t.Fatal("a verifier with no providers is not enabled")
	}
	if v.Policy() != nil {
		t.Fatal("no policy without configuration")
	}
}

func TestNewVerifier_AudienceDefaultsToResource(t *testing.T) {
	iss := authntest.NewIssuer(t)
	resource := "https://mcp.example.com/mcp"
	v, err := authn.NewVerifier(context.Background(), authn.Options{
		Config: &authz.Config{
			Resource:  resource,
			Providers: []authz.Provider{{Issuer: iss.URL}}, // no explicit audience
		},
		HTTPClient: iss.Client(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	raw := iss.Token(t, authntest.Claims{Subject: "u", Audience: resource})
	if _, verr := v.Verify(context.Background(), raw); verr != nil {
		t.Fatalf("a token audienced to auth.resource should verify: %v", verr)
	}
	wrong := iss.Token(t, authntest.Claims{Subject: "u", Audience: "something-else"})
	if _, verr := v.Verify(context.Background(), wrong); verr == nil {
		t.Fatal("audience must still be enforced when it defaults to auth.resource")
	}
}

func TestBearerToken(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER  abc  ", "abc"},
		{"Basic abc", ""},
		{"abc", ""},
		{"", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		if got := authn.BearerToken(r); got != tc.want {
			t.Errorf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func newVerifier(t *testing.T, iss *authntest.Issuer, p authz.Provider) *authn.Verifier {
	t.Helper()
	v, err := authn.NewVerifier(context.Background(), authn.Options{
		Config: &authz.Config{
			Resource:  "https://mcp.example.com/mcp",
			Providers: []authz.Provider{p},
		},
		HTTPClient: iss.Client(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}
