package authz

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Config is the `auth:` block of a content-source.yaml (or of a
// standalone file passed to `mk mcp serve-http --auth-config`). It is
// the whole authentication + authorization schema:
//
//	auth:
//	  resource: https://mcp.example.com/mcp
//	  providers:
//	    - issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
//	      audience: api://meerkat
//	      claims: { groups: groups, email: preferred_username, tenant: tid }
//	  rules:
//	    - name: sre
//	      groups: [sre]
//	      collections: [runbooks, architecture]
//	      capabilities: [read]
//	    - name: platform-admins
//	      groups: [platform-admins]
//	      collections: ["*"]
//	      capabilities: [admin]
//
// The zero Config means "no auth configured", which is what keeps every
// pre-existing deployment — stdio MCP, the static-token HTTP server, a
// content-source.yaml with no auth: key — working unchanged.
type Config struct {
	// Resource is this server's RFC 9728 resource identifier: the
	// absolute https URL clients reach it at, and the `resource` value
	// published in the protected-resource metadata and expected in
	// tokens' `aud`. Required once providers are configured — an
	// audience-unbound resource server accepts tokens minted for
	// somebody else.
	Resource string `yaml:"resource"`

	// Providers lists the OpenID Connect issuers whose tokens are
	// accepted. Discovery is generic (/.well-known/openid-configuration
	// + JWKS): Entra ID, Google and Okta are configuration, not code.
	Providers []Provider `yaml:"providers"`

	// Rules maps callers to collections and capabilities. Every matching
	// rule contributes; there is no first-match-wins and no deny rule.
	// See Policy.Evaluate.
	Rules []Rule `yaml:"rules"`

	// AllowUnauthenticated relaxes the requirement for a bearer token on
	// the hosted MCP endpoint. It exists for a single case: putting the
	// hosted transport behind an authenticating gateway that has already
	// established identity. Nothing downstream then has an identity to
	// filter on, so EVERY mounted collection is visible to every caller
	// — which is why it is off by default and refuses to combine with
	// any providers: entry.
	AllowUnauthenticated bool `yaml:"allow_unauthenticated,omitempty"`
}

// Provider is one OpenID Connect issuer.
type Provider struct {
	// Issuer is the OIDC issuer URL — exactly the `iss` value its
	// tokens carry, which is also where discovery is performed
	// (<issuer>/.well-known/openid-configuration). Must be https.
	Issuer string `yaml:"issuer"`

	// Audience is the audience this server's tokens must carry. Defaults
	// to Config.Resource when empty, which is the value RFC 8707 /
	// the MCP authorization spec expect a client to request.
	Audience string `yaml:"audience,omitempty"`

	// Claims maps meerkat's identity fields onto the claim names this
	// provider actually emits. Every field is optional; see
	// ClaimMapping's defaults.
	Claims ClaimMapping `yaml:"claims,omitempty"`

	// RequireTenant, when set, rejects any token whose mapped tenant
	// claim is not exactly this value. Useful with multi-tenant issuers
	// (Entra's `common`/`organizations` endpoints) where the issuer URL
	// alone does not pin the directory a caller came from.
	RequireTenant string `yaml:"require_tenant,omitempty"`

	// SkipIssuerCheck disables go-oidc's assertion that the token's
	// `iss` equals Issuer. Needed only for issuers that legitimately
	// serve discovery from a different host than they mint tokens for.
	// Off by default and warned about at startup.
	SkipIssuerCheck bool `yaml:"skip_issuer_check,omitempty"`
}

// ClaimMapping names the claims that carry each identity field. Empty
// fields fall back to the defaults below, which are what a
// standards-shaped OIDC token carries.
type ClaimMapping struct {
	// Groups names the claim carrying group/role membership. Default
	// "groups". The claim may be a JSON array of strings or a single
	// string (some providers emit one group unwrapped); both are
	// accepted. Set it to "roles" for an Entra app-roles deployment.
	Groups string `yaml:"groups,omitempty"`
	// Email names the claim carrying the caller's email. Default
	// "email"; Entra deployments commonly need "preferred_username".
	Email string `yaml:"email,omitempty"`
	// Tenant names the claim carrying the caller's tenant/organization.
	// Default "tid" (Entra); Okta/Google deployments usually leave it
	// unset and get an empty tenant, which no rule then matches on.
	Tenant string `yaml:"tenant,omitempty"`
}

// Defaulted returns cm with every empty field filled in.
func (cm ClaimMapping) Defaulted() ClaimMapping {
	if cm.Groups == "" {
		cm.Groups = "groups"
	}
	if cm.Email == "" {
		cm.Email = "email"
	}
	if cm.Tenant == "" {
		cm.Tenant = "tid"
	}
	return cm
}

// Rule grants capabilities over collections to the callers it matches.
//
// Matching is a disjunction WITHIN a selector (any one of `groups`
// matches) and a conjunction ACROSS selectors (a rule with both
// `groups:` and `tenant:` needs both). A rule with no selector at all
// matches every authenticated caller — the way to express "everyone who
// can get a token may read the public collection".
type Rule struct {
	// Name is a label for the rule, used in error messages and access
	// logs. Optional but strongly encouraged: it is what an operator
	// reads when asking why a caller got what they got.
	Name string `yaml:"name,omitempty"`

	// Subjects matches the token's `sub` exactly (case-sensitive: a
	// subject is an opaque identifier).
	Subjects []string `yaml:"subjects,omitempty"`
	// Emails matches the mapped email claim, case-insensitively.
	Emails []string `yaml:"emails,omitempty"`
	// Groups matches any of the mapped group claim's values,
	// case-insensitively.
	Groups []string `yaml:"groups,omitempty"`
	// Tenant matches the mapped tenant claim exactly.
	Tenant string `yaml:"tenant,omitempty"`
	// Issuer matches the issuer the token was verified against. Use it
	// to keep two providers' rules apart when subjects or group names
	// could collide.
	Issuer string `yaml:"issuer,omitempty"`

	// Collections names the collections this rule grants over. The
	// single entry "*" means every mounted collection, including any
	// added later.
	Collections []string `yaml:"collections"`
	// Capabilities names what is granted. Defaults to [read] when
	// omitted, which is the only capability enforced today.
	Capabilities []string `yaml:"capabilities,omitempty"`
}

// Enabled reports whether the config asks for anything at all. A
// disabled config is the back-compat path: no token is required, no
// collection is filtered.
func (c *Config) Enabled() bool {
	return c != nil && (len(c.Providers) > 0 || c.AllowUnauthenticated)
}

// Validate checks the config for the mistakes that would otherwise
// produce a server which looks configured but authorizes nothing (or
// everything). It is called at load time, so a bad policy fails the
// process rather than the first request.
func (c *Config) Validate() error {
	if c == nil || (!c.Enabled() && len(c.Rules) == 0) {
		return nil
	}
	if len(c.Rules) > 0 && !c.Enabled() {
		return fmt.Errorf("auth.rules is set but no auth.providers are configured — rules can only be evaluated against a verified identity")
	}
	if c.AllowUnauthenticated && len(c.Providers) > 0 {
		return fmt.Errorf("auth.allow_unauthenticated cannot be combined with auth.providers — it exists for delegating authentication to a gateway, and would make every configured provider decorative")
	}
	if !c.AllowUnauthenticated {
		if c.Resource == "" {
			return fmt.Errorf("auth.resource is required: the https URL clients reach this server at, published as the RFC 9728 resource identifier and required in every token's audience")
		}
		if err := validateHTTPSURL("auth.resource", c.Resource); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		if p.Issuer == "" {
			return fmt.Errorf("auth.providers[%d].issuer is required", i)
		}
		if err := validateHTTPSURL(fmt.Sprintf("auth.providers[%d].issuer", i), p.Issuer); err != nil {
			return err
		}
		if seen[p.Issuer] {
			return fmt.Errorf("auth.providers: duplicate issuer %q", p.Issuer)
		}
		seen[p.Issuer] = true
	}
	for i, r := range c.Rules {
		if err := r.validate(i); err != nil {
			return err
		}
	}
	return nil
}

func (r Rule) validate(i int) error {
	label := fmt.Sprintf("auth.rules[%d]", i)
	if r.Name != "" {
		label = fmt.Sprintf("auth.rules[%s]", r.Name)
	}
	if len(r.Collections) == 0 {
		return fmt.Errorf("%s.collections is required — a rule that names no collection grants nothing", label)
	}
	for _, name := range r.Collections {
		if name == "" {
			return fmt.Errorf("%s.collections contains an empty name", label)
		}
	}
	for _, capName := range r.Capabilities {
		if _, ok := ParseCapability(capName); !ok {
			return fmt.Errorf("%s.capabilities: unknown capability %q — one of %s",
				label, capName, strings.Join(capabilityNames(), ", "))
		}
	}
	return nil
}

func capabilityNames() []string {
	out := make([]string, 0, len(AllCapabilities()))
	for _, c := range AllCapabilities() {
		out = append(out, string(c))
	}
	return out
}

func validateHTTPSURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("%s must be an absolute https URL, got %q", field, raw)
	}
	// http://localhost is allowed so a developer can run the whole flow
	// against a local IdP without a certificate; anything else must be
	// https, because a bearer token crosses this URL.
	if strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	if strings.EqualFold(u.Scheme, "http") && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s must use https (http is permitted only for localhost), got %q", field, raw)
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// --- evaluation ------------------------------------------------------

// Policy is a validated Config, ready to evaluate identities against.
type Policy struct {
	cfg *Config
}

// NewPolicy validates cfg and returns the policy it describes. A nil or
// disabled cfg yields a nil *Policy, which evaluates every identity to
// nil Grants — i.e. no restriction.
func NewPolicy(cfg *Config) (*Policy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg == nil || (!cfg.Enabled() && len(cfg.Rules) == 0) {
		return nil, nil
	}
	return &Policy{cfg: cfg}, nil
}

// Config returns the policy's underlying configuration.
func (p *Policy) Config() *Config {
	if p == nil {
		return nil
	}
	return p.cfg
}

// Len reports how many rules the policy evaluates. It exists for
// telemetry: a decision span records how many rules were considered,
// which is a bounded number an operator can act on, where the rules
// themselves (names, group selectors, collection lists) are exactly what
// must not leave the process. Nil-safe — an unconfigured policy has no
// rules.
func (p *Policy) Len() int {
	if p == nil || p.cfg == nil {
		return 0
	}
	return len(p.cfg.Rules)
}

// Evaluate returns the grants an identity holds. Every matching rule
// contributes its capabilities over its collections; a caller matched
// by three rules holds the union.
//
// There is deliberately no deny rule and no rule ordering. Union-only
// evaluation means a policy's effect can be read off any single rule
// ("this rule grants read on runbooks to the sre group") without
// holding the whole document in your head to check whether a later line
// takes it away. It also means the safe edit — narrowing access — is
// deleting a rule, which is hard to get wrong.
//
// A caller matched by no rule at all gets empty, non-nil Grants: they
// authenticated, and they may read nothing.
func (p *Policy) Evaluate(id Identity) *Grants {
	if p == nil {
		return nil
	}
	g := &Grants{byCollection: map[string]CapabilitySet{}, identity: id}
	for _, r := range p.cfg.Rules {
		if !r.matches(id) {
			continue
		}
		caps := ruleCapabilities(r)
		for _, name := range r.Collections {
			if name == Wildcard {
				g.wildcard = mergeCaps(g.wildcard, caps)
				continue
			}
			g.byCollection[name] = mergeCaps(g.byCollection[name], caps)
		}
	}
	return g
}

// ruleCapabilities is the rule's capability set, defaulting to read.
func ruleCapabilities(r Rule) CapabilitySet {
	if len(r.Capabilities) == 0 {
		return CapabilitySet{CapRead: true}
	}
	set := make(CapabilitySet, len(r.Capabilities))
	for _, name := range r.Capabilities {
		if c, ok := ParseCapability(name); ok {
			set[c] = true
		}
	}
	return set
}

// matches reports whether id satisfies every selector the rule sets.
func (r Rule) matches(id Identity) bool {
	if r.Issuer != "" && r.Issuer != id.Issuer {
		return false
	}
	if r.Tenant != "" && r.Tenant != id.Tenant {
		return false
	}
	if len(r.Subjects) > 0 && !containsExact(r.Subjects, id.Subject) {
		return false
	}
	if len(r.Emails) > 0 && !containsFold(r.Emails, id.Email) {
		return false
	}
	if len(r.Groups) > 0 && !anyFold(r.Groups, id.Groups) {
		return false
	}
	return true
}

func containsExact(hay []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func containsFold(hay []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, h := range hay {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

func anyFold(want, got []string) bool {
	for _, g := range got {
		if containsFold(want, g) {
			return true
		}
	}
	return false
}

// Issuers returns the configured issuer URLs, sorted — what the RFC
// 9728 metadata publishes as authorization_servers.
func (c *Config) Issuers() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.Providers))
	for _, p := range c.Providers {
		out = append(out, p.Issuer)
	}
	sort.Strings(out)
	return out
}
