package authz

import (
	"context"
	"strings"
	"testing"
)

func TestCapabilitySet_AdminImpliesEverything(t *testing.T) {
	admin := CapabilitySet{CapAdmin: true}
	for _, c := range AllCapabilities() {
		if !admin.Has(c) {
			t.Errorf("admin set should imply %q", c)
		}
	}
	// Including a capability that doesn't exist yet — the whole point of
	// the implication is that a later meerkat's new capability is
	// already covered.
	if !admin.Has(Capability("memory-write")) {
		t.Error("admin should imply a capability added later")
	}

	readOnly := CapabilitySet{CapRead: true}
	if readOnly.Has(CapTeamWrite) {
		t.Error("a read-only set must not confer team-write")
	}
	if !readOnly.Has(CapRead) {
		t.Error("a read-only set must confer read")
	}
	var nilSet CapabilitySet
	if nilSet.Has(CapRead) {
		t.Error("a nil set confers nothing")
	}
}

func TestCapabilitySet_ListReportsWhatThePolicySaid(t *testing.T) {
	set := CapabilitySet{CapAdmin: true, CapRead: true}
	got := set.List()
	want := []Capability{CapRead, CapAdmin}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v (order is least- to most-privileged)", got, want)
		}
	}
}

func TestParseCapability(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Capability
		ok   bool
	}{
		{"read", CapRead, true},
		{"  READ ", CapRead, true},
		{"personal-write", CapPersonalWrite, true},
		{"team-write", CapTeamWrite, true},
		{"admin", CapAdmin, true},
		{"write", "", false},
		{"", "", false},
	} {
		got, ok := ParseCapability(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseCapability(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestGrants_NilMeansUnrestricted(t *testing.T) {
	var g *Grants
	if !g.Can("anything", CapRead) {
		t.Error("nil Grants (no policy in force) must permit everything — it is the stdio/back-compat path")
	}
	if !g.CanRead("anything") {
		t.Error("nil Grants must permit reading everything")
	}
	if g.Empty() {
		t.Error("nil Grants is not 'empty' — it is 'no policy'")
	}
	if !g.Capabilities("anything").Has(CapTeamWrite) {
		t.Error("nil Grants confers every capability")
	}
}

func TestPolicy_UnionOfMatchingRules(t *testing.T) {
	pol := mustPolicy(t, &Config{
		Resource: "https://mcp.example.com/mcp",
		Providers: []Provider{{
			Issuer: "https://issuer.example.com", Audience: "api://meerkat",
		}},
		Rules: []Rule{
			{Name: "everyone", Collections: []string{"public"}},
			{Name: "sre", Groups: []string{"SRE"}, Collections: []string{"runbooks"}, Capabilities: []string{"read", "team-write"}},
			{Name: "leads", Groups: []string{"leads"}, Collections: []string{"runbooks"}, Capabilities: []string{"personal-write"}},
		},
	})

	id := Identity{Subject: "u1", Issuer: "https://issuer.example.com", Groups: []string{"sre", "leads"}}
	g := pol.Evaluate(id)

	if !g.Can("public", CapRead) {
		t.Error("a rule with no selector should match every authenticated caller")
	}
	for _, c := range []Capability{CapRead, CapTeamWrite, CapPersonalWrite} {
		if !g.Can("runbooks", c) {
			t.Errorf("runbooks should hold %q from the union of two matching rules", c)
		}
	}
	if g.Can("runbooks", CapAdmin) {
		t.Error("no rule granted admin")
	}
	if g.Can("secret", CapRead) {
		t.Error("no rule mentioned 'secret'")
	}
}

func TestPolicy_SelectorsAreConjunctive(t *testing.T) {
	pol := mustPolicy(t, &Config{
		Resource:  "https://mcp.example.com/mcp",
		Providers: []Provider{{Issuer: "https://a.example.com", Audience: "x"}},
		Rules: []Rule{{
			Name:        "acme-sre",
			Groups:      []string{"sre"},
			Tenant:      "acme",
			Collections: []string{"runbooks"},
		}},
	})

	both := pol.Evaluate(Identity{Subject: "u", Groups: []string{"sre"}, Tenant: "acme"})
	if !both.Can("runbooks", CapRead) {
		t.Error("matching every selector should grant")
	}
	groupOnly := pol.Evaluate(Identity{Subject: "u", Groups: []string{"sre"}, Tenant: "other"})
	if groupOnly.Can("runbooks", CapRead) {
		t.Error("a rule with both groups: and tenant: needs BOTH — tenant mismatch must not grant")
	}
	if !groupOnly.Empty() {
		t.Error("a caller matched by no rule holds nothing")
	}
}

func TestPolicy_GroupAndEmailMatchingIsCaseInsensitive(t *testing.T) {
	pol := mustPolicy(t, &Config{
		Resource:  "https://mcp.example.com/mcp",
		Providers: []Provider{{Issuer: "https://a.example.com", Audience: "x"}},
		Rules: []Rule{
			{Groups: []string{"Platform-Admins"}, Collections: []string{"a"}},
			{Emails: []string{"Alice@Example.COM"}, Collections: []string{"b"}},
			{Subjects: []string{"exact-sub"}, Collections: []string{"c"}},
		},
	})
	g := pol.Evaluate(Identity{Subject: "exact-sub", Email: "alice@example.com", Groups: []string{"platform-admins"}})
	for _, name := range []string{"a", "b", "c"} {
		if !g.Can(name, CapRead) {
			t.Errorf("collection %q should be granted", name)
		}
	}
	// Subjects are opaque identifiers and compared exactly.
	other := pol.Evaluate(Identity{Subject: "EXACT-SUB"})
	if other.Can("c", CapRead) {
		t.Error("subject matching must be case-sensitive")
	}
}

func TestPolicy_WildcardGrantsFutureCollections(t *testing.T) {
	pol := mustPolicy(t, &Config{
		Resource:  "https://mcp.example.com/mcp",
		Providers: []Provider{{Issuer: "https://a.example.com", Audience: "x"}},
		Rules: []Rule{{
			Name: "admins", Groups: []string{"admins"},
			Collections: []string{Wildcard}, Capabilities: []string{"admin"},
		}},
	})
	g := pol.Evaluate(Identity{Subject: "u", Groups: []string{"admins"}})
	if !g.Can("a-collection-nobody-has-mounted-yet", CapRead) {
		t.Error(`collections: ["*"] must cover collections the policy never names`)
	}
	if !g.Wildcarded(CapTeamWrite) {
		t.Error("an admin wildcard implies team-write over everything")
	}
	if g.Empty() {
		t.Error("wildcard grants are not empty")
	}
	if len(g.Named()) != 0 {
		t.Errorf("Named() reports explicitly-named collections only, got %v", g.Named())
	}
}

func TestPolicy_CapabilityDefaultsToRead(t *testing.T) {
	pol := mustPolicy(t, &Config{
		Resource:  "https://mcp.example.com/mcp",
		Providers: []Provider{{Issuer: "https://a.example.com", Audience: "x"}},
		Rules:     []Rule{{Collections: []string{"docs"}}},
	})
	g := pol.Evaluate(Identity{Subject: "u"})
	if !g.Can("docs", CapRead) {
		t.Error("a rule with no capabilities: should grant read")
	}
	if g.Can("docs", CapTeamWrite) {
		t.Error("a rule with no capabilities: must not grant a write capability")
	}
}

func TestPolicy_NilForUnconfigured(t *testing.T) {
	pol, err := NewPolicy(nil)
	if err != nil {
		t.Fatalf("NewPolicy(nil): %v", err)
	}
	if pol != nil {
		t.Fatal("an unconfigured policy should be nil (no restriction)")
	}
	if g := pol.Evaluate(Identity{Subject: "u"}); g != nil {
		t.Fatal("a nil policy evaluates to nil grants — no policy in force")
	}
}

func TestConfig_Validate(t *testing.T) {
	provider := Provider{Issuer: "https://issuer.example.com", Audience: "api://meerkat"}
	for _, tc := range []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{name: "nil is fine", cfg: nil},
		{name: "empty is fine", cfg: &Config{}},
		{
			name:    "rules without providers",
			cfg:     &Config{Rules: []Rule{{Collections: []string{"a"}}}},
			wantErr: "no auth.providers are configured",
		},
		{
			name:    "providers without resource",
			cfg:     &Config{Providers: []Provider{provider}},
			wantErr: "auth.resource is required",
		},
		{
			name:    "non-https resource",
			cfg:     &Config{Resource: "http://mcp.example.com", Providers: []Provider{provider}},
			wantErr: "must use https",
		},
		{
			name: "http localhost resource is allowed",
			cfg:  &Config{Resource: "http://localhost:4005/mcp", Providers: []Provider{provider}},
		},
		{
			name:    "non-https issuer",
			cfg:     &Config{Resource: "https://m.example.com", Providers: []Provider{{Issuer: "http://evil.example.com", Audience: "x"}}},
			wantErr: "auth.providers[0].issuer must use https",
		},
		{
			name:    "duplicate issuer",
			cfg:     &Config{Resource: "https://m.example.com", Providers: []Provider{provider, provider}},
			wantErr: "duplicate issuer",
		},
		{
			name:    "rule with no collections",
			cfg:     &Config{Resource: "https://m.example.com", Providers: []Provider{provider}, Rules: []Rule{{Name: "oops"}}},
			wantErr: "auth.rules[oops].collections is required",
		},
		{
			name: "unknown capability",
			cfg: &Config{Resource: "https://m.example.com", Providers: []Provider{provider},
				Rules: []Rule{{Name: "oops", Collections: []string{"a"}, Capabilities: []string{"write"}}}},
			wantErr: `unknown capability "write"`,
		},
		{
			name:    "allow_unauthenticated with providers",
			cfg:     &Config{Resource: "https://m.example.com", Providers: []Provider{provider}, AllowUnauthenticated: true},
			wantErr: "cannot be combined with auth.providers",
		},
		{
			name: "allow_unauthenticated alone needs no resource",
			cfg:  &Config{AllowUnauthenticated: true},
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
		})
	}
}

func TestConfig_Issuers(t *testing.T) {
	cfg := &Config{Providers: []Provider{
		{Issuer: "https://z.example.com"},
		{Issuer: "https://a.example.com"},
	}}
	got := cfg.Issuers()
	if len(got) != 2 || got[0] != "https://a.example.com" || got[1] != "https://z.example.com" {
		t.Errorf("Issuers() = %v, want sorted", got)
	}
	var nilCfg *Config
	if nilCfg.Issuers() != nil {
		t.Error("a nil config has no issuers")
	}
}

func TestClaimMapping_Defaults(t *testing.T) {
	got := ClaimMapping{}.Defaulted()
	if got.Groups != "groups" || got.Email != "email" || got.Tenant != "tid" {
		t.Errorf("Defaulted() = %+v", got)
	}
	custom := ClaimMapping{Groups: "roles"}.Defaulted()
	if custom.Groups != "roles" {
		t.Errorf("an explicit mapping must survive defaulting, got %q", custom.Groups)
	}
}

func TestContext_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if FromContext(ctx) != nil {
		t.Fatal("a bare context carries no grants")
	}
	g := NewGrants(Identity{Subject: "u"}, map[string][]Capability{"a": {CapRead}})
	ctx = NewContext(ctx, g)
	if got := FromContext(ctx); got != g {
		t.Fatal("grants should round-trip through the context")
	}
	// Installing nil must not downgrade an authorized context to
	// unrestricted.
	if got := FromContext(NewContext(ctx, nil)); got != g {
		t.Fatal("NewContext(ctx, nil) must be a no-op, not a downgrade")
	}
}

func TestNewGrants_WildcardKey(t *testing.T) {
	g := NewGrants(Identity{Subject: "u"}, map[string][]Capability{Wildcard: {CapRead}})
	if !g.CanRead("anything") {
		t.Error(`NewGrants should treat "*" as a wildcard`)
	}
}

func TestIdentity_StringOmitsGroups(t *testing.T) {
	id := Identity{Subject: "u1", Issuer: "https://i.example.com", Email: "a@b.c", Groups: []string{"secret-group"}}
	if got := id.String(); strings.Contains(got, "secret-group") {
		t.Errorf("Identity.String() must not carry group membership into logs: %q", got)
	}
}

// --- write capabilities (the memory toolset) ---------------------------

func TestGrants_CanWrite(t *testing.T) {
	id := Identity{Subject: "s", Issuer: "i"}
	for name, tc := range map[string]struct {
		caps []Capability
		want bool
	}{
		"read only":     {[]Capability{CapRead}, false},
		"personal":      {[]Capability{CapPersonalWrite}, true},
		"team":          {[]Capability{CapTeamWrite}, true},
		"global":        {[]Capability{CapGlobalWrite}, true},
		"admin implies": {[]Capability{CapAdmin}, true},
		"nothing":       {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewGrants(id, map[string][]Capability{"notes": tc.caps})
			if got := g.CanWrite("notes"); got != tc.want {
				t.Errorf("CanWrite = %v, want %v", got, tc.want)
			}
			// A collection the rule never mentioned confers nothing,
			// whatever is held elsewhere.
			if g.CanWrite("other") {
				t.Error("CanWrite is true for an unmentioned collection")
			}
		})
	}
	// No policy in force is unrestricted, on the write side too.
	if !(*Grants)(nil).CanWrite("anything") {
		t.Error("nil grants must be unrestricted")
	}
}

func TestGlobalWriteIsNotImpliedByAnyOtherWriteCapability(t *testing.T) {
	// The reason global-write exists as its own capability rather than
	// as an overload of admin or team-write: neither of those may widen
	// into it, because both are already written into policies that
	// predate it.
	id := Identity{Subject: "s", Issuer: "i"}
	for _, held := range []Capability{CapRead, CapPersonalWrite, CapTeamWrite} {
		g := NewGrants(id, map[string][]Capability{"notes": {held}})
		if g.Can("notes", CapGlobalWrite) {
			t.Errorf("%q conferred global-write", held)
		}
	}
	// admin is the one exception, and that is its documented meaning.
	admin := NewGrants(id, map[string][]Capability{"notes": {CapAdmin}})
	if !admin.Can("notes", CapGlobalWrite) {
		t.Error("admin does not imply global-write")
	}
}

func TestParseCapability_AcceptsGlobalWrite(t *testing.T) {
	got, ok := ParseCapability("global-write")
	if !ok || got != CapGlobalWrite {
		t.Fatalf("ParseCapability(global-write) = %q, %v", got, ok)
	}
	// A policy written today must round-trip through Validate.
	cfg := &Config{
		Resource:  "https://mcp.example.com/mcp",
		Providers: []Provider{{Issuer: "https://idp.example.com", Audience: "api://meerkat"}},
		Rules:     []Rule{{Name: "publishers", Collections: []string{"notes"}, Capabilities: []string{"read", "global-write"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a policy granting global-write is rejected: %v", err)
	}
}

func TestGrants_EmptyIsCapabilityAgnostic(t *testing.T) {
	id := Identity{Subject: "s", Issuer: "i"}
	// This is what keeps the authentication gate's 403 from refusing a
	// principal who holds only write capabilities: a "drop notes here,
	// don't read the others'" grant must pass the gate and reach the
	// memory tool, even though every read surface shows them nothing.
	for _, caps := range [][]Capability{
		{CapPersonalWrite}, {CapTeamWrite}, {CapGlobalWrite}, {CapRead},
	} {
		g := NewGrants(id, map[string][]Capability{"notes": caps})
		if g.Empty() {
			t.Errorf("grants holding %v report Empty", caps)
		}
	}
	if !NewGrants(id, nil).Empty() {
		t.Error("grants holding nothing at all do not report Empty")
	}
}

func mustPolicy(t *testing.T, cfg *Config) *Policy {
	t.Helper()
	p, err := NewPolicy(cfg)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	if p == nil {
		t.Fatal("NewPolicy returned nil for a configured policy")
	}
	return p
}
