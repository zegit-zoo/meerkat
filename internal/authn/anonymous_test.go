package authn_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/authn"
	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
)

// anonymous_test.go is the authentication gate's decision table for
// `anonymous: true` rules (#36).
//
// The row that carries the most weight is the third one: a token that is
// PRESENT but does not verify gets 401 whether or not the policy
// publishes anything. Downgrading it to anonymous would turn every
// expiry into a silent, partial-data outage — the client keeps working,
// the answers keep missing the collections the caller is entitled to, and
// the 401 challenge that would have told it to refresh never arrives.

// publicRule publishes `handbook` to callers with no token.
var publicRule = authz.Rule{
	Name: "public", Anonymous: true,
	Collections: []string{"handbook"}, Capabilities: []string{"read"},
}

// sreRule is an ordinary claim-selected rule, so the tests can tell
// "the anonymous rule fired" from "some rule fired".
var sreRule = authz.Rule{
	Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"},
}

// gateResult is what one trip through the gate produced.
type gateResult struct {
	status    int
	grants    *authz.Grants
	challenge string
	denied    []authn.Reason
	anonymous []authn.Reason
}

// runGate drives a gate built over rules with the given Authorization
// header value (empty for none) and reports everything the request
// produced, hooks included.
func runGate(t *testing.T, rules []authz.Rule, authorization string) gateResult {
	t.Helper()
	v, _ := newVerifierFor(t, rules)
	return runGateWith(t, v, authorization)
}

func runGateWith(t *testing.T, v *authn.Verifier, authorization string) gateResult {
	t.Helper()
	var res gateResult
	gate := authn.NewGate(v, authn.MetadataURL(testResource),
		authn.WithDenyHook(func(_ *http.Request, reason authn.Reason, _ error) {
			res.denied = append(res.denied, reason)
		}),
		authn.WithAnonymousHook(func(_ *http.Request, reason authn.Reason) {
			res.anonymous = append(res.anonymous, reason)
		}))

	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	gate.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		res.grants = authz.FromContext(req.Context())
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)

	res.status = rec.Code
	res.challenge = rec.Header().Get("WWW-Authenticate")
	return res
}

// newVerifierFor builds a verifier over rules against a fresh issuer,
// returning both so a test can mint its own tokens.
func newVerifierFor(t *testing.T, rules []authz.Rule) (*authn.Verifier, *authntest.Issuer) {
	t.Helper()
	iss := authntest.NewIssuer(t)
	v, err := authn.NewVerifier(t.Context(), authn.Options{
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
	return v, iss
}

func TestGate_NoTokenWithAnonymousRuleIsAdmitted(t *testing.T) {
	res := runGate(t, []authz.Rule{sreRule, publicRule}, "")

	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an anonymous rule publishes the collection", res.status)
	}
	if res.grants == nil {
		t.Fatal("an admitted anonymous request must carry non-nil grants: a nil *Grants means " +
			"NO POLICY IN FORCE, which every read surface treats as unrestricted")
	}
	if !res.grants.CanRead("handbook") {
		t.Error("the anonymous caller should read the published collection")
	}
	if res.grants.CanRead("runbooks") {
		t.Error("the anonymous caller must not inherit a claim-selected rule's collections")
	}
	if id := res.grants.Identity(); id.Subject != "" {
		t.Errorf("an anonymous caller must carry no subject, got %+v", id)
	}
	if len(res.anonymous) != 1 || res.anonymous[0] != authn.ReasonAnonymous {
		t.Errorf("anonymous hook = %v, want one %q", res.anonymous, authn.ReasonAnonymous)
	}
	if len(res.denied) != 0 {
		t.Errorf("an admitted request must not reach the deny hook: %v", res.denied)
	}
}

func TestGate_NoTokenWithoutAnonymousRuleIs401(t *testing.T) {
	// The zero-behaviour-change row: unchanged status, unchanged
	// challenge, unchanged RFC 9728 pointer.
	res := runGate(t, []authz.Rule{sreRule}, "")

	if res.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.status)
	}
	if !strings.HasPrefix(res.challenge, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q", res.challenge)
	}
	if !strings.Contains(res.challenge, `resource_metadata="`+wantMetadataURL+`"`) {
		t.Errorf("WWW-Authenticate = %q, want the RFC 9728 metadata pointer", res.challenge)
	}
	if len(res.denied) != 1 || res.denied[0] != authn.ReasonMissingToken {
		t.Errorf("deny hook = %v, want one %q", res.denied, authn.ReasonMissingToken)
	}
	if len(res.anonymous) != 0 {
		t.Errorf("nothing was published, so nothing should have been admitted: %v", res.anonymous)
	}
}

// TestGate_ABadTokenIsNeverDowngradedToAnonymous is the security-critical
// row. Every way a token can fail, against a policy that DOES publish a
// collection: all of them 401, none of them silently become the anonymous
// caller they would have been with no header at all.
func TestGate_ABadTokenIsNeverDowngradedToAnonymous(t *testing.T) {
	v, iss := newVerifierFor(t, []authz.Rule{sreRule, publicRule})

	// Sanity: this policy really does admit a token-less caller, so a
	// passing 401 below is about the token and not about the policy.
	if res := runGateWith(t, v, ""); res.status != http.StatusOK {
		t.Fatalf("precondition: no token = %d, want 200", res.status)
	}

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"expired", "Bearer " + iss.Token(t, authntest.Claims{
			Subject: "alice", Audience: testAudience,
			IssuedAt: time.Now().Add(-2 * time.Hour), Expiry: time.Now().Add(-time.Hour),
		})},
		{"forged signature", "Bearer " + iss.TokenSignedByOther(t, authntest.Claims{
			Subject: "mallory", Audience: testAudience,
		})},
		{"wrong audience", "Bearer " + iss.Token(t, authntest.Claims{
			Subject: "alice", Audience: "api://somebody-else",
		})},
		{"wrong issuer", "Bearer " + iss.Token(t, authntest.Claims{
			Subject: "alice", Audience: testAudience, Issuer: "https://evil.example.com",
		})},
		{"no subject", "Bearer " + iss.Token(t, authntest.Claims{Audience: testAudience})},
		{"malformed", "Bearer not-a-jwt-at-all"},
		{"empty bearer value", "Bearer "},
		{"garbage scheme", "Basic dXNlcjpwYXNz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runGateWith(t, v, tc.header)

			if res.status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 — a %s credential must NEVER be downgraded to anonymous access", res.status, tc.name)
			}
			if res.grants != nil {
				t.Errorf("a refused request reached the handler with grants: %v", res.grants.Named())
			}
			if len(res.anonymous) != 0 {
				t.Errorf("a %s credential was classified as an anonymous admission: %v", tc.name, res.anonymous)
			}
			// "Bearer " with nothing after it, and a scheme meerkat does not
			// accept, are a caller who ATTEMPTED to authenticate: there is
			// no token to verify, so they keep the missing_token reason they
			// have always had — but they do not become anonymous.
			wantReason := authn.ReasonInvalidToken
			wantError := `error="invalid_token"`
			if tc.name == "empty bearer value" || tc.name == "garbage scheme" {
				wantReason, wantError = authn.ReasonMissingToken, `error="invalid_request"`
			}
			if len(res.denied) != 1 || res.denied[0] != wantReason {
				t.Errorf("deny hook = %v, want one %q", res.denied, wantReason)
			}
			if !strings.Contains(res.challenge, wantError) {
				t.Errorf("WWW-Authenticate = %q, want %s", res.challenge, wantError)
			}
			// Still points at the metadata: the challenge is what tells a
			// client where to go and refresh.
			if !strings.Contains(res.challenge, `resource_metadata="`+wantMetadataURL+`"`) {
				t.Errorf("WWW-Authenticate = %q, want the RFC 9728 metadata pointer", res.challenge)
			}
		})
	}
}

func TestGate_ValidTokenUnionsTheAnonymousGrants(t *testing.T) {
	v, iss := newVerifierFor(t, []authz.Rule{sreRule, publicRule})
	res := runGateWith(t, v, "Bearer "+iss.Token(t, authntest.Claims{
		Subject: "alice", Audience: testAudience, Groups: []string{"sre"},
	}))

	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if !res.grants.CanRead("runbooks") {
		t.Error("the caller's own rule should apply")
	}
	if !res.grants.CanRead("handbook") {
		t.Error("a published collection must be readable without duplicating it into the caller's rule")
	}
	if id := res.grants.Identity(); id.Subject != "alice" {
		t.Errorf("identity = %+v, want the verified subject", id)
	}
	if len(res.anonymous) != 0 {
		t.Errorf("an authenticated request must not be classified as an anonymous admission: %v", res.anonymous)
	}
}

// TestGate_MatchedByNothingWithSomethingPublished pins the one
// intentional change to an existing status code. Before #36 a verified
// token matched by no rule got 403. Once a collection is PUBLISHED, that
// caller holds the public set — which they could read with no token at
// all — so refusing them would be strictly stranger than admitting them.
func TestGate_MatchedByNothingWithSomethingPublished(t *testing.T) {
	v, iss := newVerifierFor(t, []authz.Rule{sreRule, publicRule})
	res := runGateWith(t, v, "Bearer "+iss.Token(t, authntest.Claims{
		Subject: "outsider", Audience: testAudience, Groups: []string{"interns"},
	}))

	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the caller holds the published collection", res.status)
	}
	if !res.grants.CanRead("handbook") {
		t.Error("want read on the published collection")
	}
	if res.grants.CanRead("runbooks") {
		t.Error("and nothing else")
	}
	// The identity is still theirs: this is an authenticated request that
	// happens to hold only public grants, not an anonymous one.
	if id := res.grants.Identity(); id.Subject != "outsider" {
		t.Errorf("identity = %+v, want the verified subject", id)
	}
	if len(res.anonymous) != 0 {
		t.Errorf("hook = %v, want none: the caller authenticated", res.anonymous)
	}
}

func TestGate_MatchedByNothingWithNothingPublishedIsStill403(t *testing.T) {
	v, iss := newVerifierFor(t, []authz.Rule{sreRule})
	res := runGateWith(t, v, "Bearer "+iss.Token(t, authntest.Claims{
		Subject: "outsider", Audience: testAudience, Groups: []string{"interns"},
	}))

	if res.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — unchanged for a policy that publishes nothing", res.status)
	}
	if len(res.denied) != 1 || res.denied[0] != authn.ReasonNoGrants {
		t.Errorf("deny hook = %v, want one %q", res.denied, authn.ReasonNoGrants)
	}
}

// TestGate_AnonymousRuleForAnUnmountedCollectionStillAdmits records a
// deliberate boundary. The gate authorizes; it does not know what is
// mounted. A published collection that does not exist yields non-empty
// grants and a 200, and the caller then sees an empty registry — exactly
// what an authenticated caller granted a decommissioned collection has
// always got (see internal/mcp's tool filter, which offers them no tools).
func TestGate_AnonymousRuleForAnUnmountedCollectionStillAdmits(t *testing.T) {
	res := runGate(t, []authz.Rule{{
		Name: "public", Anonymous: true, Collections: []string{"a-collection-nobody-mounted"},
	}}, "")
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.grants.Empty() {
		t.Error("the grants should be non-empty; whether anything is mounted is not the gate's question")
	}
}
