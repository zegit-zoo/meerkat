package authn_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authn"
	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
)

const testResource = "https://mcp.example.com/mcp"

// wantMetadataURL is what MetadataURL derives for testResource: RFC 9728
// §3.1 puts a path-qualified resource's metadata under the well-known
// prefix.
const wantMetadataURL = "https://mcp.example.com/.well-known/oauth-protected-resource/mcp"

func TestMiddleware_MissingTokenIs401WithChallenge(t *testing.T) {
	gate, _ := newGate(t, nil)
	rec := serve(gate, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", challenge)
	}
	if !strings.Contains(challenge, `resource_metadata="`+wantMetadataURL+`"`) {
		t.Fatalf("WWW-Authenticate = %q, want it to point at the RFC 9728 metadata", challenge)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["error"] != "invalid_request" {
		t.Errorf("error = %q", body["error"])
	}
}

func TestMiddleware_InvalidTokenIs401(t *testing.T) {
	gate, iss := newGate(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+iss.TokenSignedByOther(t, authntest.Claims{
		Subject: "u", Audience: testAudience,
	}))
	rec := serve(gate, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want error=invalid_token", rec.Header().Get("WWW-Authenticate"))
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Error("even an invalid_token 401 must point at the metadata so a client can re-authorize")
	}
}

func TestMiddleware_ValidTokenWithNoGrantsIs403(t *testing.T) {
	// A policy that grants only to a group this caller isn't in.
	gate, iss := newGate(t, []authz.Rule{{
		Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"},
	}})
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+iss.Token(t, authntest.Claims{
		Subject: "outsider", Audience: testAudience, Groups: []string{"interns"},
	}))
	rec := serve(gate, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	// The refusal must say nothing about what exists — it is a statement
	// about the caller. Assert no collection name leaked into it.
	if strings.Contains(rec.Body.String(), "runbooks") {
		t.Errorf("a 403 must not name a collection: %s", rec.Body.String())
	}
}

func TestMiddleware_ValidTokenInstallsGrants(t *testing.T) {
	gate, iss := newGate(t, []authz.Rule{
		{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}, Capabilities: []string{"read", "team-write"}},
		{Name: "all", Collections: []string{"public"}},
	})

	var got *authz.Grants
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = authz.FromContext(r.Context())
	})
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+iss.Token(t, authntest.Claims{
		Subject: "u1", Audience: testAudience, Email: "u1@example.com",
		Groups: []string{"sre"}, Tenant: "acme",
	}))
	rec := httptest.NewRecorder()
	gate.Middleware(next).ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got == nil {
		t.Fatal("grants should be installed in the request context")
	}
	if !got.CanRead("runbooks") || !got.CanRead("public") {
		t.Errorf("expected read on runbooks and public, got %v", got.Named())
	}
	if got.CanRead("secret") {
		t.Error("no rule granted 'secret'")
	}
	if !got.Can("runbooks", authz.CapTeamWrite) {
		t.Error("team-write should be carried through even though nothing enforces it yet")
	}
	if id := got.Identity(); id.Subject != "u1" || id.Tenant != "acme" {
		t.Errorf("identity = %+v", id)
	}
}

func TestMiddleware_TransparentWhenNoProviderConfigured(t *testing.T) {
	v, err := authn.NewVerifier(context.Background(), authn.Options{Config: nil})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	gate := authn.NewGate(v, "")

	var sawGrants bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawGrants = authz.FromContext(r.Context()) != nil
		w.WriteHeader(http.StatusTeapot)
	})
	rec := httptest.NewRecorder()
	gate.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d — an unconfigured gate must be transparent", rec.Code)
	}
	if sawGrants {
		t.Fatal("an unconfigured gate must install NO grants, so downstream reads 'no policy in force'")
	}
}

func TestMiddleware_DenyHookSeesReason(t *testing.T) {
	iss := authntest.NewIssuer(t)
	v := newVerifier(t, iss, authz.Provider{Issuer: iss.URL, Audience: testAudience})

	var reasons []authn.Reason
	gate := authn.NewGate(v, authn.MetadataURL(testResource),
		authn.WithDenyHook(func(_ *http.Request, reason authn.Reason, _ error) {
			reasons = append(reasons, reason)
		}))

	serve(gate, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	bad := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	bad.Header.Set("Authorization", "Bearer nonsense")
	serve(gate, bad)

	want := []authn.Reason{authn.ReasonMissingToken, authn.ReasonInvalidToken}
	if len(reasons) != len(want) {
		t.Fatalf("reasons = %v, want %v", reasons, want)
	}
	for i := range want {
		if reasons[i] != want[i] {
			t.Fatalf("reasons = %v, want %v", reasons, want)
		}
	}
}

func TestMetadataURL(t *testing.T) {
	for _, tc := range []struct{ resource, want string }{
		{testResource, wantMetadataURL},
		{"https://mcp.example.com", "https://mcp.example.com/.well-known/oauth-protected-resource"},
		{"https://mcp.example.com/", "https://mcp.example.com/.well-known/oauth-protected-resource"},
		{"", ""},
		{"not-a-url", ""},
	} {
		if got := authn.MetadataURL(tc.resource); got != tc.want {
			t.Errorf("MetadataURL(%q) = %q, want %q", tc.resource, got, tc.want)
		}
	}
}

func TestMetadataHandler_ServesRFC9728Document(t *testing.T) {
	cfg := &authz.Config{
		Resource: testResource,
		Providers: []authz.Provider{
			{Issuer: "https://z.example.com"},
			{Issuer: "https://a.example.com"},
		},
	}
	rec := httptest.NewRecorder()
	authn.MetadataHandler(cfg, "meerkat").ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, authn.MetadataPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		BearerMethods        []string `json:"bearer_methods_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body: %v", err)
	}
	if doc.Resource != testResource {
		t.Errorf("resource = %q", doc.Resource)
	}
	if len(doc.AuthorizationServers) != 2 || doc.AuthorizationServers[0] != "https://a.example.com" {
		t.Errorf("authorization_servers = %v, want both issuers, sorted", doc.AuthorizationServers)
	}
	if len(doc.BearerMethods) != 1 || doc.BearerMethods[0] != "header" {
		t.Errorf("bearer_methods_supported = %v", doc.BearerMethods)
	}
}

func newGate(t *testing.T, rules []authz.Rule) (*authn.Gate, *authntest.Issuer) {
	t.Helper()
	iss := authntest.NewIssuer(t)
	v, err := authn.NewVerifier(context.Background(), authn.Options{
		Config: &authz.Config{
			Resource:  testResource,
			Providers: []authz.Provider{{Issuer: iss.URL, Audience: testAudience}},
			Rules:     rules,
		},
		HTTPClient: iss.Client(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return authn.NewGate(v, authn.MetadataURL(testResource)), iss
}

func serve(gate *authn.Gate, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	gate.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	return rec
}
