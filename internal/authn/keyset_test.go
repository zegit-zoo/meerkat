package authn_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/zegit-zoo/meerkat/internal/authn"
	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
)

// fakeClock is a settable clock shared by the verifier under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newKeySetVerifier(t *testing.T, iss *authntest.Issuer, clk *fakeClock, reg prometheus.Registerer) *authn.Verifier {
	t.Helper()
	v, err := authn.NewVerifier(context.Background(), authn.Options{
		Config: &authz.Config{
			Resource:  "https://mcp.example.com/mcp",
			Providers: []authz.Provider{{Issuer: iss.URL, Audience: testAudience}},
		},
		HTTPClient:     iss.Client(),
		Now:            clk.Now,
		JWKSMinRefresh: time.Minute,
		Metrics:        reg,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func counterValue(t *testing.T, reg *prometheus.Registry, name, label string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if label == "" || hasLabel(m, label) {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("metric %s{%s} not found", name, label)
	return 0
}

func hasLabel(m *dto.Metric, value string) bool {
	for _, l := range m.GetLabel() {
		if l.GetValue() == value {
			return true
		}
	}
	return false
}

// TestJWKS_ForgedTokenFloodCausesNoRefetch: tokens with the right issuer,
// audience and expiry but a bad signature must not make the server fetch
// the provider's keys. A flood of tokens naming an unknown key ID costs
// at most one fetch per refresh interval.
func TestJWKS_ForgedTokenFloodCausesNoRefetch(t *testing.T) {
	iss := authntest.NewIssuer(t)
	clk := &fakeClock{t: time.Now()}
	reg := prometheus.NewRegistry()
	v := newKeySetVerifier(t, iss, clk, reg)
	ctx := context.Background()
	claims := authntest.Claims{Subject: "u", Audience: testAudience}

	if _, err := v.Verify(ctx, iss.Token(t, claims)); err != nil {
		t.Fatalf("a valid token must verify: %v", err)
	}
	if got := iss.JWKSFetches(); got != 1 {
		t.Fatalf("fetches after the first token = %d, want 1 (the initial load)", got)
	}

	// Known key ID, wrong signature: invalid, and never a fetch.
	forged := iss.TokenSignedByOther(t, claims)
	for range 200 {
		if _, err := v.Verify(ctx, forged); err == nil {
			t.Fatal("forged token verified")
		}
	}
	if got := iss.JWKSFetches(); got != 1 {
		t.Fatalf("fetches after 200 forged tokens with a known kid = %d, want 1", got)
	}

	// Unknown key ID inside the refresh interval: held back.
	unknown := iss.TokenSignedByOtherWithKID(t, claims, "never-published")
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { _, _ = v.Verify(ctx, unknown) })
	}
	wg.Wait()
	if got := iss.JWKSFetches(); got != 1 {
		t.Fatalf("fetches for unknown kids inside the interval = %d, want 1", got)
	}

	// Once the interval has passed, a burst of unknown key IDs costs
	// exactly one fetch.
	clk.Advance(61 * time.Second)
	for range 50 {
		wg.Go(func() { _, _ = v.Verify(ctx, unknown) })
	}
	wg.Wait()
	if got := iss.JWKSFetches(); got != 2 {
		t.Fatalf("fetches for a burst of unknown kids after the interval = %d, want 2", got)
	}

	if got := counterValue(t, reg, "meerkat_oidc_jwks_fetches_total", "unknown_kid"); got != 1 {
		t.Errorf("forced refetch metric = %v, want 1", got)
	}
	if got := counterValue(t, reg, "meerkat_oidc_jwks_fetches_total", "initial"); got != 1 {
		t.Errorf("initial fetch metric = %v, want 1", got)
	}
	if got := counterValue(t, reg, "meerkat_oidc_jwks_refetch_throttled_total", ""); got < 1 {
		t.Errorf("throttled metric = %v, want at least 1", got)
	}
}

// TestJWKS_RotationIsPickedUp: a key published after startup is found by
// the first token that names it once the refresh interval allows, and
// tokens from the old key keep verifying throughout.
func TestJWKS_RotationIsPickedUp(t *testing.T) {
	iss := authntest.NewIssuer(t)
	clk := &fakeClock{t: time.Now()}
	v := newKeySetVerifier(t, iss, clk, nil)
	ctx := context.Background()
	claims := authntest.Claims{Subject: "u", Audience: testAudience}

	oldToken := iss.Token(t, claims)
	if _, err := v.Verify(ctx, oldToken); err != nil {
		t.Fatalf("old key: %v", err)
	}
	iss.Rotate(t)
	newToken := iss.Token(t, claims)

	if _, err := v.Verify(ctx, newToken); err == nil {
		t.Fatal("a key first seen inside the refresh interval must not trigger a fetch")
	}
	clk.Advance(61 * time.Second)
	if _, err := v.Verify(ctx, newToken); err != nil {
		t.Fatalf("rotated key after the interval: %v", err)
	}
	if _, err := v.Verify(ctx, oldToken); err != nil {
		t.Fatalf("old key after rotation: %v", err)
	}
	if got := iss.JWKSFetches(); got != 2 {
		t.Fatalf("fetches = %d, want 2 (initial + one rotation)", got)
	}
}
