package authn

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// Reason classifies why a request was refused, for metrics and access
// logs. It is never sent to the client beyond what RFC 6750's own
// error codes already say.
type Reason string

const (
	// ReasonMissingToken — no Authorization: Bearer header.
	ReasonMissingToken Reason = "missing_token"
	// ReasonInvalidToken — a token that failed signature, issuer,
	// audience, expiry or tenant verification.
	ReasonInvalidToken Reason = "invalid_token"
	// ReasonNoGrants — a valid token whose identity no policy rule
	// matched, so it holds no capability over any collection: it may
	// neither read nor write anything.
	ReasonNoGrants Reason = "no_grants"
)

// MetadataPath is the RFC 9728 well-known path for OAuth 2.0 Protected
// Resource Metadata.
const MetadataPath = mcpserver.WellKnownProtectedResourcePath

// Gate authenticates requests and attaches the resulting authorization
// grants to the request context.
type Gate struct {
	verifier *Verifier
	// metadataURL is the absolute URL of this server's protected-resource
	// metadata, advertised in WWW-Authenticate so an unauthenticated
	// client can discover where to get a token (MCP authorization spec /
	// RFC 9728 §5.1).
	metadataURL string
	// onDeny is called for every refusal, for metrics/logging. Optional.
	onDeny func(r *http.Request, reason Reason, err error)
}

// GateOption configures a Gate.
type GateOption func(*Gate)

// WithDenyHook registers a callback invoked on every refused request.
func WithDenyHook(fn func(r *http.Request, reason Reason, err error)) GateOption {
	return func(g *Gate) { g.onDeny = fn }
}

// NewGate builds the authentication gate for a verifier. metadataURL is
// the absolute URL the RFC 9728 metadata is served at; it is what a 401
// points clients to.
func NewGate(v *Verifier, metadataURL string, opts ...GateOption) *Gate {
	g := &Gate{verifier: v, metadataURL: metadataURL}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Middleware wraps next with bearer-token authentication.
//
// When no provider is configured the gate is transparent: the request
// proceeds with NO grants in context, which every downstream surface
// reads as "no policy in force". That is the back-compat path — an
// operator who has not written an auth: block gets the same server they
// had before, and `auth.allow_unauthenticated: true` is the explicit,
// documented way to ask for it in front of a gateway that authenticates
// on meerkat's behalf.
//
// Otherwise:
//
//	no token          -> 401, WWW-Authenticate: Bearer resource_metadata=...
//	token won't verify-> 401, ... error="invalid_token"
//	verifies, no rule -> 403 (the caller exists and holds no capability
//	                     at all; this says nothing about what exists)
//	verifies, matched -> next, with *authz.Grants in the context
//
// The 403 case is worth being precise about: it is a statement about
// the CALLER, not about the collections. It leaks nothing, because the
// answer is the same whether the deployment mounts one collection or
// fifty. Callers who hold some grants never reach it, and the
// collections they don't hold are filtered out of the registry rather
// than refused — see authz's package comment.
//
// "Some grants" means ANY capability, not `read`: Grants.Empty is
// capability-agnostic, so a principal granted only `personal-write`
// passes the gate and reaches the memory tool, even though every read
// surface will show them an empty registry.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.verifier.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		raw := BearerToken(r)
		if raw == "" {
			g.deny(w, r, http.StatusUnauthorized, ReasonMissingToken, "invalid_request",
				"a bearer token is required", ErrNoToken)
			return
		}
		id, err := g.verifier.Verify(r.Context(), raw)
		if err != nil {
			g.deny(w, r, http.StatusUnauthorized, ReasonInvalidToken, "invalid_token",
				"the access token is expired, malformed, or issued for another audience", err)
			return
		}
		grants := g.verifier.Policy().Evaluate(id)
		if grants.Empty() {
			g.deny(w, r, http.StatusForbidden, ReasonNoGrants, "insufficient_scope",
				"this identity is not granted access to any collection", nil)
			return
		}
		next.ServeHTTP(w, r.WithContext(authz.NewContext(r.Context(), grants)))
	})
}

// deny writes an RFC 6750 §3 challenge and a small JSON body.
//
// The WWW-Authenticate header carries resource_metadata (RFC 9728 §5.1)
// so an MCP client that gets a 401 can discover the authorization
// servers to talk to without any out-of-band configuration — that
// discovery loop is the entire reason the metadata endpoint exists.
func (g *Gate) deny(w http.ResponseWriter, r *http.Request, status int, reason Reason, code, desc string, err error) {
	if g.onDeny != nil {
		g.onDeny(r, reason, err)
	}
	w.Header().Set("WWW-Authenticate", g.challenge(code, desc))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

// challenge builds the WWW-Authenticate value.
func (g *Gate) challenge(code, desc string) string {
	var b strings.Builder
	b.WriteString(`Bearer`)
	if g.metadataURL != "" {
		b.WriteString(` resource_metadata="`)
		b.WriteString(quoteParam(g.metadataURL))
		b.WriteString(`"`)
	}
	if code != "" {
		b.WriteString(`, error="`)
		b.WriteString(quoteParam(code))
		b.WriteString(`"`)
	}
	if desc != "" {
		b.WriteString(`, error_description="`)
		b.WriteString(quoteParam(desc))
		b.WriteString(`"`)
	}
	return b.String()
}

// quoteParam escapes the two characters an RFC 7235 quoted-string
// can't carry raw, and drops CR/LF outright. Values here are
// server-controlled (a configured URL, a fixed error code and a fixed
// description), so this guards against a header-splitting
// *configuration* mistake rather than untrusted input.
func quoteParam(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// Metadata builds the RFC 9728 Protected Resource Metadata document for
// a configuration.
//
// scopes are advertised so a client's authorization request can ask for
// something meaningful; meerkat itself authorizes on the policy's
// group/subject rules rather than on scopes, so these are a hint to the
// authorization server, not an enforcement point.
func Metadata(cfg *authz.Config, resourceName string) mcpserver.ProtectedResourceMetadataConfig {
	return mcpserver.ProtectedResourceMetadataConfig{
		Resource:               cfg.Resource,
		AuthorizationServers:   cfg.Issuers(),
		BearerMethodsSupported: []string{"header"},
		ResourceName:           resourceName,
		ScopesSupported:        []string{"openid", "profile", "email"},
	}
}

// MetadataHandler serves the RFC 9728 document.
func MetadataHandler(cfg *authz.Config, resourceName string) http.Handler {
	return mcpserver.NewProtectedResourceMetadataHandler(Metadata(cfg, resourceName))
}

// MetadataURL is the absolute URL the metadata is served at for a given
// resource identifier — the value a 401's WWW-Authenticate points to.
//
// RFC 9728 §3.1 puts a path-qualified resource's metadata under the
// well-known prefix (https://host/.well-known/oauth-protected-resource/mcp
// for a resource of https://host/mcp), which is what
// mcpserver.ProtectedResourceMetadataPath computes.
func MetadataURL(resource string) string {
	if resource == "" {
		return ""
	}
	base, err := originOf(resource)
	if err != nil {
		return ""
	}
	return base + mcpserver.ProtectedResourceMetadataPath(resource)
}

// originOf reduces an absolute URL to scheme://host.
func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !u.IsAbs() || u.Host == "" {
		return "", errNotAbsolute
	}
	return u.Scheme + "://" + u.Host, nil
}

var errNotAbsolute = errors.New("not an absolute URL")
