// Package authn verifies OpenID Connect bearer tokens and turns them
// into an authz.Identity the rest of meerkat can authorize against.
//
// It is generic OIDC: an issuer URL is discovered
// (<issuer>/.well-known/openid-configuration), its JWKS is fetched and
// cached by github.com/coreos/go-oidc, and the ID/access token's
// signature, issuer, audience and expiry are checked. Entra ID, Google
// Workspace and Okta are therefore configuration, not code — there is
// no provider-specific branch anywhere in this package, only a
// per-provider claim mapping (authz.ClaimMapping) for the fact that
// directory products disagree about which claim carries groups, email
// and tenant.
//
// The HTTP surface it provides is Middleware, which implements the MCP
// authorization spec's side of RFC 9728: an absent, malformed, expired
// or unauthorized token gets 401 with a WWW-Authenticate header
// pointing at this server's protected-resource metadata, so a client
// can discover where to go and get a token without being told anything
// else.
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// Errors distinguishing the reasons a token was refused. They are used
// for metrics labels and log lines; the HTTP response deliberately does
// NOT vary in a way that tells an unauthenticated caller which one
// applied beyond RFC 6750's own vocabulary.
var (
	// ErrNoToken means no Authorization: Bearer header was present.
	ErrNoToken = errors.New("no bearer token")
	// ErrInvalidToken means a token was present but did not verify
	// against any configured provider.
	ErrInvalidToken = errors.New("invalid bearer token")
)

// Verifier checks bearer tokens against one or more OIDC providers.
// Safe for concurrent use; the underlying JWKS caches are.
type Verifier struct {
	providers []*providerVerifier
	policy    *authz.Policy
	cfg       *authz.Config
}

type providerVerifier struct {
	cfg      authz.Provider
	claims   authz.ClaimMapping
	verifier *oidc.IDTokenVerifier
}

// Options configures NewVerifier.
type Options struct {
	// Config is the auth: block. Required.
	Config *authz.Config
	// HTTPClient is used for discovery and JWKS fetches. Nil uses a
	// client with a sane timeout rather than http.DefaultClient, so a
	// hung IdP can't wedge startup forever.
	HTTPClient *http.Client
	// Now overrides the clock used for expiry checks. Tests set it; a
	// nil value means time.Now.
	Now func() time.Time
}

// NewVerifier performs OIDC discovery for every configured provider and
// returns a Verifier ready to check tokens.
//
// Discovery happens once, here, rather than lazily on the first
// request: an unreachable or misconfigured issuer should fail the
// process at startup — where a deployment notices — not surface as
// intermittent 401s under load. JWKS refresh after that point is
// go-oidc's remote key set, which re-fetches on an unknown key ID.
func NewVerifier(ctx context.Context, opts Options) (*Verifier, error) {
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pol, err := authz.NewPolicy(cfg)
	if err != nil {
		return nil, err
	}
	v := &Verifier{policy: pol, cfg: cfg}
	if cfg == nil || len(cfg.Providers) == 0 {
		return v, nil
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	ctx = oidc.ClientContext(ctx, client)

	for _, p := range cfg.Providers {
		provider, perr := oidc.NewProvider(ctx, p.Issuer)
		if perr != nil {
			return nil, fmt.Errorf("oidc discovery for issuer %q: %w", p.Issuer, perr)
		}
		audience := p.Audience
		if audience == "" {
			audience = cfg.Resource
		}
		oc := &oidc.Config{
			ClientID:        audience,
			SkipIssuerCheck: p.SkipIssuerCheck,
			Now:             opts.Now,
		}
		if audience == "" {
			// Unreachable for a validated config (Validate requires
			// auth.resource whenever providers exist), but skipping the
			// audience check silently would accept tokens minted for any
			// other relying party of the same IdP — the single most
			// consequential misconfiguration available here. Refuse.
			return nil, fmt.Errorf("oidc provider %q: no audience — set auth.providers[].audience or auth.resource", p.Issuer)
		}
		v.providers = append(v.providers, &providerVerifier{
			cfg:      p,
			claims:   p.Claims.Defaulted(),
			verifier: provider.Verifier(oc),
		})
	}
	return v, nil
}

// Policy returns the authorization policy the verifier evaluates
// identities against. Nil when no rules are configured.
func (v *Verifier) Policy() *authz.Policy { return v.policy }

// Config returns the auth configuration in force.
func (v *Verifier) Config() *authz.Config { return v.cfg }

// Enabled reports whether any provider is configured.
func (v *Verifier) Enabled() bool { return v != nil && len(v.providers) > 0 }

// Verify checks a raw bearer token against every configured provider
// and returns the identity it establishes.
//
// Providers are tried in configuration order and the FIRST successful
// verification wins. A token can only verify against the provider that
// issued it (the issuer and audience checks see to that), so "first
// success" is not a precedence rule in any meaningful sense — it is
// just the order the single matching provider is found in.
func (v *Verifier) Verify(ctx context.Context, raw string) (authz.Identity, error) {
	if raw == "" {
		return authz.Identity{}, ErrNoToken
	}
	var lastErr error
	for _, p := range v.providers {
		tok, err := p.verifier.Verify(ctx, raw)
		if err != nil {
			lastErr = err
			continue
		}
		id, cerr := p.identity(tok)
		if cerr != nil {
			lastErr = cerr
			continue
		}
		return id, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no OIDC provider is configured")
	}
	return authz.Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, lastErr)
}

// identity maps a verified token's claims onto an authz.Identity using
// this provider's claim mapping.
func (p *providerVerifier) identity(tok *oidc.IDToken) (authz.Identity, error) {
	var raw map[string]json.RawMessage
	if err := tok.Claims(&raw); err != nil {
		return authz.Identity{}, fmt.Errorf("decode claims: %w", err)
	}
	id := authz.Identity{
		Subject: tok.Subject,
		Issuer:  tok.Issuer,
		Email:   stringClaim(raw, p.claims.Email),
		Groups:  stringsClaim(raw, p.claims.Groups),
		Tenant:  stringClaim(raw, p.claims.Tenant),
	}
	if id.Subject == "" {
		return authz.Identity{}, errors.New("token has no subject")
	}
	if p.cfg.RequireTenant != "" && id.Tenant != p.cfg.RequireTenant {
		return authz.Identity{}, fmt.Errorf("token tenant %q does not match the required tenant", id.Tenant)
	}
	return id, nil
}

// stringClaim reads a string claim, tolerating its absence.
func stringClaim(raw map[string]json.RawMessage, name string) string {
	if name == "" {
		return ""
	}
	b, ok := raw[name]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return ""
	}
	return s
}

// stringsClaim reads a claim that should be an array of strings but
// might be a single bare string. Providers differ (and some emit one
// group unwrapped), and the difference is not worth failing a token
// over.
func stringsClaim(raw map[string]json.RawMessage, name string) []string {
	if name == "" {
		return nil
	}
	b, ok := raw[name]
	if !ok {
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		return list
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil && one != "" {
		return []string{one}
	}
	return nil
}

// BearerToken extracts the token from an Authorization header. The
// scheme comparison is case-insensitive per RFC 7235 §2.1.
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
