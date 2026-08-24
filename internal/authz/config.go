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
//	    - name: public-handbook
//	      anonymous: true              # no token at all
//	      collections: [handbook]
//	      capabilities: [read]         # the only one an anonymous rule may hold
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
	//
	// It is also refused alongside an `anonymous: true` rule. The two say
	// contradictory things about the same deployment — "everything is
	// public, somebody upstream authenticates" versus "these named
	// collections are public and the rest still needs a token here" — and
	// the coarse one wins silently, which would leave an operator reading
	// a carefully scoped anonymous rule that grants nothing it does not
	// already have. See Validate.
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
//
// `anonymous: true` is the one selector that is not about a claim, and
// it is exclusive with all the others: see the field.
type Rule struct {
	// Name is a label for the rule, used in error messages and access
	// logs. Optional but strongly encouraged: it is what an operator
	// reads when asking why a caller got what they got.
	Name string `yaml:"name,omitempty"`

	// Anonymous publishes this rule's collections to the caller who
	// presented NO bearer token at all, and — because public means
	// public — to every authenticated caller as well, so a collection
	// does not have to be restated in every other rule to stay readable
	// once it is published.
	//
	// It is the ONLY way an unauthenticated caller acquires any grant:
	// every other rule, including a selector-less one, describes "every
	// AUTHENTICATED caller" and contributes nothing to the anonymous
	// principal. Authenticated-by-default therefore survives; a
	// deployment that writes no anonymous rule behaves exactly as it did.
	//
	// Two things are refused at load time (Rule.validate):
	//
	//   - combining it with subjects/emails/groups/tenant/issuer. Those
	//     select on claims of a verified token, and there is no token
	//     here; a rule carrying both would read as a narrowing and act as
	//     none.
	//   - any capability but `read`. An anonymous grant is read-only,
	//     structurally: nobody can be held to an anonymous write, an
	//     anonymous personal memory has no owner (see
	//     internal/memory.Authorize), and `admin` would confer whatever
	//     capability a later meerkat adds — on the internet.
	Anonymous bool `yaml:"anonymous,omitempty"`

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
	// The anonymous-rule checks come FIRST, ahead of the two general ones
	// below that would otherwise catch the same configurations with a
	// vaguer sentence. An operator who wrote `anonymous: true` should be
	// told what is wrong with that rule, not handed a generic complaint
	// about rules and providers that does not mention the line they just
	// added.
	if i, r, ok := c.firstAnonymousRule(); ok {
		if c.AllowUnauthenticated {
			return fmt.Errorf("%s sets anonymous: true, which cannot be combined with auth.allow_unauthenticated — "+
				"allow_unauthenticated already publishes EVERY mounted collection to every caller, so the rule's "+
				"collections list would be silently widened to all of them; keep the rule and drop the flag to publish "+
				"only what it names", ruleLabel(i, r))
		}
		if len(c.Providers) == 0 {
			return fmt.Errorf("%s sets anonymous: true but no auth.providers are configured — anonymous access is a "+
				"carve-out from an authenticated server, and with no provider there is nothing it carves out of: every "+
				"collection is already readable without a token", ruleLabel(i, r))
		}
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
	label := ruleLabel(i, r)
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
	if !r.Anonymous {
		return nil
	}
	// An anonymous rule selects on the ABSENCE of a token, so every
	// claim selector beside it is unsatisfiable. Refusing rather than
	// ignoring: a rule that reads as narrow and acts as wide is the
	// failure mode this whole file exists to prevent.
	if sel := r.claimSelectors(); len(sel) > 0 {
		return fmt.Errorf("%s sets anonymous: true together with %s — an anonymous caller presents no token, "+
			"so there are no claims to select on; use one or the other",
			label, strings.Join(sel, ", "))
	}
	for _, capName := range r.Capabilities {
		c, _ := ParseCapability(capName)
		if c != CapRead {
			return fmt.Errorf("%s grants %q to anonymous callers — an anonymous rule is read-only, because a write "+
				"by a caller nobody can name has no owner, no audit trail and no reviewer (and %q would confer "+
				"whatever capability a later meerkat adds); use capabilities: [read]",
				label, c, CapAdmin)
		}
	}
	return nil
}

// claimSelectors names the token-claim selectors the rule sets, for the
// exclusivity error above.
func (r Rule) claimSelectors() []string {
	var out []string
	if len(r.Subjects) > 0 {
		out = append(out, "subjects")
	}
	if len(r.Emails) > 0 {
		out = append(out, "emails")
	}
	if len(r.Groups) > 0 {
		out = append(out, "groups")
	}
	if r.Tenant != "" {
		out = append(out, "tenant")
	}
	if r.Issuer != "" {
		out = append(out, "issuer")
	}
	return out
}

// ruleLabel names a rule in an error message: by its `name` when it has
// one, by its index otherwise.
func ruleLabel(i int, r Rule) string {
	if r.Name != "" {
		return fmt.Sprintf("auth.rules[%s]", r.Name)
	}
	return fmt.Sprintf("auth.rules[%d]", i)
}

// firstAnonymousRule returns the first rule that publishes to anonymous
// callers, for the config-level checks that need to name one.
func (c *Config) firstAnonymousRule() (int, Rule, bool) {
	if c == nil {
		return 0, Rule{}, false
	}
	for i, r := range c.Rules {
		if r.Anonymous {
			return i, r, true
		}
	}
	return 0, Rule{}, false
}

// HasAnonymousRules reports whether any rule publishes collections to
// unauthenticated callers. It is what a server's banner and its
// diagnostics ask; the grants themselves come from
// Policy.EvaluateAnonymous.
func (c *Config) HasAnonymousRules() bool {
	_, _, ok := c.firstAnonymousRule()
	return ok
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

// Evaluate returns the grants an AUTHENTICATED identity holds. Every
// matching rule contributes its capabilities over its collections; a
// caller matched by three rules holds the union.
//
// Rules published to anonymous callers (`anonymous: true`) contribute
// here too, unconditionally. Public means public: a collection an
// operator put on the internet is not one an employee has to be granted
// separately, and requiring the restatement would be a standing
// invitation to publish something and forget to re-grant it — with the
// failure landing on the people who are supposed to have it.
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
	return p.evaluate(id, false)
}

// EvaluateAnonymous returns the grants a caller who presented NO token
// holds: the union of the `anonymous: true` rules, and nothing else.
//
// It is the whole of the anonymous authorization path. There is no
// second evaluator, no bypass and no "public" flag threaded through the
// registry — the result is an ordinary *Grants over an ordinary
// Identity{} (no subject, no issuer), so every surface downstream
// filters, rebuilds tool descriptions, reports capabilities and refuses
// memory writes by the code it already had. The one thing that is new
// is which rules were allowed to contribute.
//
// The grants are EMPTY when no rule publishes anything, which is what
// the authentication gate reads as "there is nothing to admit an
// anonymous caller to" — see internal/authn.Gate.Middleware.
func (p *Policy) EvaluateAnonymous() *Grants {
	return p.evaluate(Identity{}, true)
}

// evaluate is the single rule loop both entry points share. anon says
// the caller presented no token, which is the only thing that changes
// which rules are eligible.
func (p *Policy) evaluate(id Identity, anon bool) *Grants {
	if p == nil {
		return nil
	}
	g := &Grants{byCollection: map[string]CapabilitySet{}, identity: id}
	for _, r := range p.cfg.Rules {
		if !r.applies(id, anon) {
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

// Anonymous reports whether the policy publishes anything to
// unauthenticated callers. Nil-safe: no policy publishes nothing.
func (p *Policy) Anonymous() bool {
	if p == nil || p.cfg == nil {
		return false
	}
	return p.cfg.HasAnonymousRules()
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

// applies reports whether the rule contributes to a caller. anon says
// the caller presented no bearer token.
//
// The two branches above the claim check are the entire anonymous
// authorization model, and they are asymmetric on purpose:
//
//	anonymous rule, any caller  -> applies. The rule is a statement
//	                               about the COLLECTION ("this is
//	                               published"), not about who is asking.
//	other rule, anonymous caller-> never applies. Every claim selector
//	                               needs a verified token, and a rule
//	                               with NO selector means "every
//	                               authenticated caller" — which an
//	                               unauthenticated one is not. This is
//	                               what keeps authenticated-by-default
//	                               true for the selector-less rule that
//	                               every existing policy is full of.
func (r Rule) applies(id Identity, anon bool) bool {
	if r.Anonymous {
		return true
	}
	if anon {
		return false
	}
	return r.matches(id)
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
