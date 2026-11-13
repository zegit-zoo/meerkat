package controller

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

const accessAuth = `resource: https://meerkat.example/mcp
rules:
  - name: everyone
    collections: [root]
  - name: platform-team
    groups: [platform]
    collections: [platform, root]
    capabilities: [read, team-write]
  - name: named
    subjects: [alice, mallory]
    collections: [platform]
  - name: strangers
    subjects: [mallory]
    collections: [platform]
  - name: admins
    groups: [admins]
    collections: ['*']
    capabilities: [admin]
`

// accessUnits is the tree with platform dedicated and restricted to
// alice and bob, planned under placement single, with the mob's auth.
func accessUnits(t *testing.T, authYAML string, ids ...string) ([]unit, *authz.Config) {
	t.Helper()
	kbs, _ := subtree("root", tree())
	kbs[1].Spec.Placement = v1alpha1.PlacementDedicated // platform
	kbs[1].Spec.Access = &v1alpha1.Access{Identities: ids}
	auth, err := renderAuth(testMob("").Spec.Auth, authYAML)
	if err != nil {
		t.Fatal(err)
	}
	return planUnits(testMob(v1alpha1.PlacementSingle), kbs), auth
}

func unitNamed(t *testing.T, units []unit, name string) *unit {
	t.Helper()
	for i := range units {
		if units[i].Name == name {
			return &units[i]
		}
	}
	t.Fatalf("no unit %s", name)
	return nil
}

func rulesByName(cfg *authz.Config) map[string]authz.Rule {
	out := map[string]authz.Rule{}
	for _, r := range cfg.Rules {
		out[r.Name] = r
	}
	return out
}

func TestAuthFor_NarrowsEveryRuleThatGrantsTheDedicatedKB(t *testing.T) {
	units, auth := accessUnits(t, accessAuth, "alice", "bob", "alice")
	got, err := authFor(unitNamed(t, units, "mob-kb-platform"), auth)
	if err != nil {
		t.Fatal(err)
	}
	rules := rulesByName(got)
	want := map[string][]string{
		"everyone":      nil,              // does not grant platform: as written
		"platform-team": {"alice", "bob"}, // no subjects: gets the identities, repeats dropped
		"named":         {"alice"},        // its own subjects ∩ identities
		"admins":        {"alice", "bob"}, // grants platform through "*"
	}
	if len(rules) != len(want) {
		t.Errorf("rules = %v; want %v (strangers has no subject left and is dropped)", names2(got), keys(want))
	}
	for name, subjects := range want {
		r, ok := rules[name]
		if !ok {
			t.Errorf("rule %s was dropped", name)
			continue
		}
		if !reflect.DeepEqual(r.Subjects, subjects) {
			t.Errorf("%s subjects = %v, want %v", name, r.Subjects, subjects)
		}
	}
	// Only the subjects change: groups and capabilities stay as written.
	if pt := rules["platform-team"]; !reflect.DeepEqual(pt.Groups, []string{"platform"}) || !reflect.DeepEqual(pt.Capabilities, []string{"read", "team-write"}) {
		t.Errorf("platform-team = %+v", pt)
	}
	if a := rules["admins"]; !reflect.DeepEqual(a.Capabilities, []string{"admin"}) || !reflect.DeepEqual(a.Collections, []string{"*"}) {
		t.Errorf("admins = %+v", a)
	}
	// The mob's own block is not touched: other units still get it whole.
	if !reflect.DeepEqual(rulesByName(auth)["platform-team"].Subjects, []string(nil)) || len(auth.Rules) != 5 {
		t.Errorf("the mob's auth block was changed: %+v", auth.Rules)
	}
}

func TestAuthFor_LeavesOtherUnitsAlone(t *testing.T) {
	units, auth := accessUnits(t, accessAuth, "alice")
	// The shared unit holding root, flux and vendors.
	if got, err := authFor(unitNamed(t, units, "mob-all"), auth); err != nil || got != auth {
		t.Errorf("shared unit: %v, %v; want the mob's block", got, err)
	}
	// A dedicated KnowledgeBase without identities: nothing to narrow.
	units, auth = accessUnits(t, accessAuth)
	if got, err := authFor(unitNamed(t, units, "mob-kb-platform"), auth); err != nil || got != auth {
		t.Errorf("no identities: %v, %v; want the mob's block", got, err)
	}
	// A KnowledgeBase that lists identities but is not dedicated is not
	// narrowed: under perKB its unit holds it alone, and still not.
	kbs, _ := subtree("root", tree())
	kbs[1].Spec.Access = &v1alpha1.Access{Identities: []string{"alice"}}
	for _, u := range planUnits(testMob(v1alpha1.PlacementPerKB), kbs) {
		if got, err := authFor(&u, auth); err != nil || got != auth {
			t.Errorf("%s: %v, %v; want the mob's block", u.Name, got, err)
		}
	}
}

func TestAuthFor_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, auth string
		ids        []string
		want       string
	}{
		{"anonymous rule naming the KB", "resource: https://meerkat.example/mcp\nrules:\n  - name: public\n    anonymous: true\n    collections: [platform]\n",
			[]string{"alice"}, "auth.rules[public] publishes KnowledgeBase \"platform\" to anonymous callers"},
		{"anonymous rule with *", "resource: https://meerkat.example/mcp\nrules:\n  - anonymous: true\n    collections: ['*']\n",
			[]string{"alice"}, "auth.rules[0] publishes"},
		{"two providers, no issuer", "resource: https://meerkat.example/mcp\nproviders:\n  - issuer: https://second.example\nrules:\n  - name: team\n    collections: [platform]\n",
			[]string{"alice"}, "needs issuer:"},
		{"empty identity", accessAuth, []string{"alice", ""}, "access.identities contains an empty entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units, auth := accessUnits(t, tc.auth, tc.ids...)
			if _, err := authFor(unitNamed(t, units, "mob-kb-platform"), auth); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v; want it to mention %q", err, tc.want)
			}
		})
	}
	// allow_unauthenticated comes from a block with no providers, which
	// renderAuth never yields; build it directly.
	units, _ := accessUnits(t, accessAuth, "alice")
	open := &authz.Config{AllowUnauthenticated: true, Rules: []authz.Rule{{Collections: []string{"root"}}}}
	if _, err := authFor(unitNamed(t, units, "mob-kb-platform"), open); err == nil || !strings.Contains(err.Error(), "allow_unauthenticated") {
		t.Errorf("allow_unauthenticated: err = %v", err)
	}
	// An anonymous rule that does not grant the KB is fine: it is not
	// mounted there.
	units, auth := accessUnits(t, "resource: https://meerkat.example/mcp\nrules:\n  - anonymous: true\n    collections: [root]\n", "alice")
	if _, err := authFor(unitNamed(t, units, "mob-kb-platform"), auth); err != nil {
		t.Errorf("an anonymous rule for another KB: %v", err)
	}
	// Two providers and a rule with issuer: narrowed, not refused.
	units, auth = accessUnits(t, "resource: https://meerkat.example/mcp\nproviders:\n  - issuer: https://second.example\nrules:\n  - name: team\n    issuer: https://issuer.example\n    collections: [platform]\n", "alice")
	got, err := authFor(unitNamed(t, units, "mob-kb-platform"), auth)
	if err != nil || !reflect.DeepEqual(got.Rules[0].Subjects, []string{"alice"}) || got.Rules[0].Issuer != "https://issuer.example" {
		t.Errorf("two providers with issuer: %+v, %v", got, err)
	}
}

// The rendered file, loaded and evaluated the way the server does it:
// the identities get what their rules give them, nobody else gets the
// KnowledgeBase at all.
func TestAuthFor_TheServerEnforcesIt(t *testing.T) {
	units, auth := accessUnits(t, accessAuth, "alice", "bob")
	u := unitNamed(t, units, "mob-kb-platform")
	narrowed, err := authFor(u, auth)
	if err != nil {
		t.Fatal(err)
	}
	body, err := renderConfig(u, narrowed, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := contentsource.Parse(body, "rendered")
	if err != nil {
		t.Fatalf("the server refuses the rendered file: %v\n%s", err, body)
	}
	policy, err := authz.NewPolicy(cfg.Auth)
	if err != nil {
		t.Fatal(err)
	}
	iss := "https://issuer.example"
	for _, tc := range []struct {
		id          authz.Identity
		read, write bool
	}{
		{authz.Identity{Subject: "alice", Issuer: iss}, true, false},                            // named
		{authz.Identity{Subject: "bob", Issuer: iss, Groups: []string{"platform"}}, true, true}, // platform-team, narrowed
		{authz.Identity{Subject: "carol", Issuer: iss, Groups: []string{"platform"}}, false, false},
		{authz.Identity{Subject: "carol", Issuer: iss, Groups: []string{"admins"}}, false, false},
		{authz.Identity{Subject: "mallory", Issuer: iss}, false, false}, // named and strangers, both narrowed away
	} {
		g := policy.Evaluate(tc.id)
		if g.CanRead("platform") != tc.read || g.CanWrite("platform") != tc.write {
			t.Errorf("%s %v: read %v write %v; want %v %v", tc.id.Subject, tc.id.Groups, g.CanRead("platform"), g.CanWrite("platform"), tc.read, tc.write)
		}
	}
}

func names2(cfg *authz.Config) []string {
	var out []string
	for _, r := range cfg.Rules {
		out = append(out, r.Name)
	}
	return out
}

func keys(m map[string][]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
