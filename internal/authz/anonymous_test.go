package authz

import (
	"strings"
	"testing"
)

// anonymous_test.go covers the `anonymous: true` rule selector (#36):
// what the config loader refuses, and exactly which rules contribute to
// which caller.
//
// The load-bearing property under test is asymmetry. An anonymous rule
// contributes to EVERY caller; every other rule — including a
// selector-less one, which every real policy is full of — contributes to
// no anonymous caller at all. Get that backwards in either direction and
// a deployment either publishes its whole knowledge base or silently
// revokes access from the people it just published to.

// anonProvider is a valid provider, so the tests below fail on the thing
// they are about rather than on a missing issuer.
var anonProvider = Provider{Issuer: "https://idp.example.com", Audience: "api://meerkat"}

func anonConfig(rules ...Rule) *Config {
	return &Config{
		Resource:  "https://mcp.example.com/mcp",
		Providers: []Provider{anonProvider},
		Rules:     rules,
	}
}

func TestConfig_ValidateAnonymousRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name: "read-only anonymous rule is accepted",
			cfg: anonConfig(Rule{
				Name: "public", Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"read"},
			}),
		},
		{
			name: "capabilities default to read",
			cfg:  anonConfig(Rule{Name: "public", Anonymous: true, Collections: []string{"handbook"}}),
		},
		{
			name: "wildcard collections are allowed, unwise as they are",
			cfg:  anonConfig(Rule{Name: "everything", Anonymous: true, Collections: []string{Wildcard}}),
		},

		// --- write capabilities are refused, one per capability -------
		{
			name: "personal-write is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"personal-write"}}),
			wantErr: `auth.rules[public] grants "personal-write" to anonymous callers`,
		},
		{
			name: "team-write is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"team-write"}}),
			wantErr: `grants "team-write" to anonymous callers`,
		},
		{
			name: "global-write is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"global-write"}}),
			wantErr: `grants "global-write" to anonymous callers`,
		},
		{
			name: "admin is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"admin"}}),
			wantErr: `grants "admin" to anonymous callers`,
		},
		{
			name: "read alongside a write is still refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"read", "team-write"}}),
			wantErr: `grants "team-write" to anonymous callers`,
		},
		{
			name: "an unnamed rule is identified by index",
			cfg: anonConfig(Rule{Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"admin"}}),
			wantErr: "auth.rules[0] grants",
		},

		// --- selector exclusivity, one per selector -------------------
		{
			name: "anonymous with groups is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Groups: []string{"sre"}, Collections: []string{"handbook"}}),
			wantErr: "auth.rules[public] sets anonymous: true together with groups",
		},
		{
			name: "anonymous with subjects is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Subjects: []string{"u1"}, Collections: []string{"handbook"}}),
			wantErr: "together with subjects",
		},
		{
			name: "anonymous with emails is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Emails: []string{"a@example.com"}, Collections: []string{"handbook"}}),
			wantErr: "together with emails",
		},
		{
			name: "anonymous with tenant is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Tenant: "acme", Collections: []string{"handbook"}}),
			wantErr: "together with tenant",
		},
		{
			name: "anonymous with issuer is refused",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Issuer: "https://idp.example.com", Collections: []string{"handbook"}}),
			wantErr: "together with issuer",
		},
		{
			name: "every offending selector is named",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Groups: []string{"sre"}, Tenant: "acme", Collections: []string{"handbook"}}),
			wantErr: "together with groups, tenant",
		},

		// --- interaction with the pre-existing escape hatch -----------
		{
			name: "anonymous with allow_unauthenticated is refused",
			cfg: &Config{
				AllowUnauthenticated: true,
				Rules: []Rule{{Name: "public", Anonymous: true,
					Collections: []string{"handbook"}}},
			},
			wantErr: "cannot be combined with auth.allow_unauthenticated",
		},
		{
			// And the message names the rule, rather than falling through
			// to the general "rules without providers" complaint that does
			// not mention the line the operator just wrote.
			name: "anonymous with no providers is refused",
			cfg: &Config{
				Resource: "https://mcp.example.com/mcp",
				Rules: []Rule{{Name: "public", Anonymous: true,
					Collections: []string{"handbook"}}},
			},
			wantErr: "auth.rules[public] sets anonymous: true but no auth.providers are configured",
		},
		{
			name: "the ordinary collections/capability checks still apply",
			cfg: anonConfig(Rule{Name: "public", Anonymous: true,
				Collections: []string{"handbook"}, Capabilities: []string{"scribble"}}),
			wantErr: `unknown capability "scribble"`,
		},
		{
			name:    "an anonymous rule naming no collection is refused",
			cfg:     anonConfig(Rule{Name: "public", Anonymous: true}),
			wantErr: "auth.rules[public].collections is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
			// NewPolicy must refuse exactly what Validate refuses: a policy
			// that constructed from an invalid config would be a validation
			// gap with a running server behind it.
			_, perr := NewPolicy(tc.cfg)
			if (perr != nil) != (err != nil) {
				t.Errorf("NewPolicy err = %v but Validate err = %v — they must agree", perr, err)
			}
		})
	}
}

func TestConfig_HasAnonymousRules(t *testing.T) {
	if (*Config)(nil).HasAnonymousRules() {
		t.Error("a nil config publishes nothing")
	}
	if anonConfig(Rule{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}}).HasAnonymousRules() {
		t.Error("a policy with no anonymous rule reports one")
	}
	cfg := anonConfig(
		Rule{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}},
		Rule{Name: "public", Anonymous: true, Collections: []string{"handbook"}},
	)
	if !cfg.HasAnonymousRules() {
		t.Error("a policy with an anonymous rule does not report one")
	}
}

// TestPolicy_AnonymousGrantsOnlyComeFromAnonymousRules is the
// authenticated-by-default guarantee, stated as a test. The
// selector-less rule is the one that matters: it means "every
// authenticated caller", and if it leaked into the anonymous evaluation
// every existing deployment that wrote one would have published a
// collection to the internet by upgrading.
func TestPolicy_AnonymousGrantsOnlyComeFromAnonymousRules(t *testing.T) {
	p := mustPolicy(t, anonConfig(
		Rule{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}},
		Rule{Name: "every-authenticated-caller", Collections: []string{"vendor-docs"}},
		Rule{Name: "public", Anonymous: true, Collections: []string{"handbook"}},
	))

	anon := p.EvaluateAnonymous()
	if anon.Empty() {
		t.Fatal("an anonymous rule should produce non-empty anonymous grants")
	}
	if !anon.CanRead("handbook") {
		t.Error("the anonymous caller should read the published collection")
	}
	for _, name := range []string{"runbooks", "vendor-docs"} {
		if anon.CanRead(name) {
			t.Errorf("the anonymous caller must not read %q — no anonymous rule names it", name)
		}
	}
	if got := anon.Named(); len(got) != 1 || got[0] != "handbook" {
		t.Errorf("Named() = %v, want exactly [handbook]", got)
	}
	// And they are nobody: an empty identity is what the memory layer
	// recognises as anonymous and refuses personal writes for.
	if id := anon.Identity(); id.Subject != "" || id.Issuer != "" {
		t.Errorf("anonymous grants carry an identity: %+v", id)
	}
	if anon.CanWrite("handbook") {
		t.Error("anonymous grants must confer no write capability")
	}
}

func TestPolicy_NoAnonymousRuleMeansEmptyAnonymousGrants(t *testing.T) {
	// The zero-behaviour-change case, at the policy layer: this is what
	// the authentication gate reads as "there is nothing to admit an
	// anonymous caller to", and it is what keeps the 401 unchanged.
	p := mustPolicy(t, anonConfig(
		Rule{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}},
		Rule{Name: "everyone", Collections: []string{"vendor-docs"}},
	))
	if !p.EvaluateAnonymous().Empty() {
		t.Fatal("a policy with no anonymous rule must produce EMPTY anonymous grants")
	}
	if p.Anonymous() {
		t.Error("Policy.Anonymous() should be false")
	}
	if (*Policy)(nil).Anonymous() {
		t.Error("a nil policy publishes nothing")
	}
	if (*Policy)(nil).EvaluateAnonymous() != nil {
		t.Error("a nil policy evaluates to nil grants, which downstream reads as unrestricted")
	}
}

// TestPolicy_AuthenticatedCallersUnionTheAnonymousGrants is the "public
// means public" half. Without it an operator publishing a collection
// would have to add it to every other rule too, and the day they forgot
// their own staff would lose access to something anonymous callers on the
// internet still had.
func TestPolicy_AuthenticatedCallersUnionTheAnonymousGrants(t *testing.T) {
	p := mustPolicy(t, anonConfig(
		Rule{Name: "sre", Groups: []string{"sre"}, Collections: []string{"runbooks"}},
		Rule{Name: "public", Anonymous: true, Collections: []string{"handbook"}},
	))

	sre := p.Evaluate(Identity{Subject: "alice", Issuer: "https://idp.example.com", Groups: []string{"sre"}})
	if !sre.CanRead("runbooks") {
		t.Error("the sre rule should still apply")
	}
	if !sre.CanRead("handbook") {
		t.Error("an authenticated caller must inherit the anonymous grants without a duplicated rule")
	}

	// A caller no ordinary rule matches is NOT 403 material once
	// something is published: they hold the public set, which they could
	// have read with no token at all.
	outsider := p.Evaluate(Identity{Subject: "bob", Issuer: "https://idp.example.com", Groups: []string{"interns"}})
	if outsider.Empty() {
		t.Fatal("a caller matched by no rule should still hold the published collections")
	}
	if !outsider.CanRead("handbook") {
		t.Error("the published collection should be readable by any authenticated caller")
	}
	if outsider.CanRead("runbooks") {
		t.Error("the published rule must not widen anything else")
	}
}

// TestPolicy_AnonymousRuleDoesNotWidenCapabilitiesForAuthenticatedCallers
// checks the union is a union of capability SETS, not a replacement: a
// caller who holds write on a collection an anonymous rule also publishes
// keeps their write.
func TestPolicy_AnonymousRuleDoesNotNarrowAuthenticatedCapabilities(t *testing.T) {
	p := mustPolicy(t, anonConfig(
		Rule{Name: "authors", Groups: []string{"authors"}, Collections: []string{"handbook"},
			Capabilities: []string{"read", "team-write"}},
		Rule{Name: "public", Anonymous: true, Collections: []string{"handbook"}},
	))
	author := p.Evaluate(Identity{Subject: "a", Issuer: "i", Groups: []string{"authors"}})
	if !author.Can("handbook", CapTeamWrite) {
		t.Error("publishing a collection must not take a writer's capability away")
	}
	if got := author.Capabilities("handbook").Strings(); strings.Join(got, ",") != "read,team-write" {
		t.Errorf("capabilities = %v, want [read team-write]", got)
	}
	// The anonymous caller's own view of the same collection stays read.
	if got := p.EvaluateAnonymous().Capabilities("handbook").Strings(); strings.Join(got, ",") != "read" {
		t.Errorf("anonymous capabilities = %v, want [read]", got)
	}
}
