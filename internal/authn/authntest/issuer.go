// Package authntest provides a fake OpenID Connect issuer for tests.
//
// It serves a real discovery document and a real JWKS over httptest,
// and mints real RS256-signed JWTs against a key generated for the test
// process. Nothing here is a stub of meerkat's own verification path:
// tokens are checked by github.com/coreos/go-oidc exactly as a token
// from Entra ID, Google or Okta would be, so a test that passes here
// exercises signature, issuer, audience and expiry checking for real.
//
// No test needs the network, a real identity provider, or credentials.
package authntest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Issuer is a running fake OIDC provider.
type Issuer struct {
	// URL is the issuer identifier — the value tokens carry as `iss`,
	// and what an auth.providers[].issuer should be set to.
	URL string

	srv    *httptest.Server
	key    *rsa.PrivateKey
	keyID  string
	signer jose.Signer
}

// keyBits is deliberately the smallest size crypto/rsa will sign with
// RS256 comfortably. Tests generate a key per issuer and key generation
// dominates their runtime; 2048 is the smallest size that is both
// realistic and not slow.
const keyBits = 2048

// NewIssuer starts a fake issuer and registers cleanup on t.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	iss := &Issuer{key: key, keyID: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss.URL,
			"authorization_endpoint":                iss.URL + "/authorize",
			"token_endpoint":                        iss.URL + "/token",
			"jwks_uri":                              iss.URL + "/jwks",
			"userinfo_endpoint":                     iss.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     iss.keyID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}})
	})

	srv := httptest.NewServer(mux)
	iss.srv = srv
	iss.URL = srv.URL

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", iss.keyID),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	iss.signer = signer

	t.Cleanup(srv.Close)
	return iss
}

// Client returns an HTTP client that can reach the fake issuer. Pass it
// as authn.Options.HTTPClient (and HostedConfig.HTTPClient) so
// discovery and JWKS fetches resolve.
func (i *Issuer) Client() *http.Client { return i.srv.Client() }

// Claims is a token's payload. Fields left zero are omitted, so a test
// can mint a token that is missing a claim on purpose.
type Claims struct {
	Subject  string
	Audience string
	Email    string
	Groups   []string
	Tenant   string
	// IssuedAt / Expiry default to now and now+1h.
	IssuedAt time.Time
	Expiry   time.Time
	// Issuer overrides the `iss` claim, for testing issuer mismatch.
	Issuer string
	// Extra adds arbitrary claims, for exercising a custom claim
	// mapping (roles instead of groups, say).
	Extra map[string]any
}

// Token mints a signed JWT for c.
func (i *Issuer) Token(t *testing.T, c Claims) string {
	t.Helper()
	now := time.Now()
	if c.IssuedAt.IsZero() {
		c.IssuedAt = now
	}
	if c.Expiry.IsZero() {
		c.Expiry = now.Add(time.Hour)
	}
	issuer := c.Issuer
	if issuer == "" {
		issuer = i.URL
	}
	payload := map[string]any{
		"iss": issuer,
		"sub": c.Subject,
		"aud": c.Audience,
		"iat": c.IssuedAt.Unix(),
		"exp": c.Expiry.Unix(),
	}
	if c.Email != "" {
		payload["email"] = c.Email
	}
	if len(c.Groups) > 0 {
		payload["groups"] = c.Groups
	}
	if c.Tenant != "" {
		payload["tid"] = c.Tenant
	}
	for k, v := range c.Extra {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	jws, err := i.signer.Sign(body)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return raw
}

// TokenSignedByOther mints a token with the right claims but the WRONG
// key — the forged-token case. It uses a second key that no JWKS
// publishes, so verification must fail on the signature alone.
func (i *Issuer) TokenSignedByOther(t *testing.T, c Claims) string {
	t.Helper()
	other, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: other},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", i.keyID),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	forged := *i
	forged.signer = signer
	return forged.Token(t, c)
}
