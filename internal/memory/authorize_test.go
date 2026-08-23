package memory

import (
	"errors"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// authorize_test.go pins the scope x capability matrix. It is the whole
// write-side authorization model, so it is tested exhaustively rather
// than by example.

func grants(t *testing.T, caps ...authz.Capability) *authz.Grants {
	t.Helper()
	return authz.NewGrants(
		authz.Identity{Subject: "user-1", Issuer: "https://idp.example.com"},
		map[string][]authz.Capability{"notes": caps},
	)
}

func TestAuthorize_Matrix(t *testing.T) {
	tests := []struct {
		name  string
		caps  []authz.Capability
		scope Scope
		want  Outcome
	}{
		// The capability that matches the scope: a direct write.
		{"personal with personal-write", []authz.Capability{authz.CapPersonalWrite}, ScopePersonal, OutcomeWrite},
		{"team with team-write", []authz.Capability{authz.CapTeamWrite}, ScopeTeam, OutcomeWrite},
		{"global with global-write", []authz.Capability{authz.CapGlobalWrite}, ScopeGlobal, OutcomeWrite},

		// admin implies every capability, including global-write.
		{"personal with admin", []authz.Capability{authz.CapAdmin}, ScopePersonal, OutcomeWrite},
		{"team with admin", []authz.Capability{authz.CapAdmin}, ScopeTeam, OutcomeWrite},
		{"global with admin", []authz.Capability{authz.CapAdmin}, ScopeGlobal, OutcomeWrite},

		// A writer at another scope proposes rather than writing.
		{"team from a personal writer", []authz.Capability{authz.CapPersonalWrite}, ScopeTeam, OutcomeStage},
		{"global from a personal writer", []authz.Capability{authz.CapPersonalWrite}, ScopeGlobal, OutcomeStage},
		{"global from a team writer", []authz.Capability{authz.CapTeamWrite}, ScopeGlobal, OutcomeStage},
		{"team from a global writer", []authz.Capability{authz.CapGlobalWrite}, ScopeTeam, OutcomeStage},

		// A reader is not a writer, at any scope.
		{"personal from a reader", []authz.Capability{authz.CapRead}, ScopePersonal, OutcomeRefused},
		{"team from a reader", []authz.Capability{authz.CapRead}, ScopeTeam, OutcomeRefused},
		{"global from a reader", []authz.Capability{authz.CapRead}, ScopeGlobal, OutcomeRefused},

		// Personal has no staging row: an unauthorized personal write is
		// refused outright, because a personal memory has no reviewer.
		{"personal from a team writer", []authz.Capability{authz.CapTeamWrite}, ScopePersonal, OutcomeRefused},
		{"personal from a global writer", []authz.Capability{authz.CapGlobalWrite}, ScopePersonal, OutcomeRefused},

		{"nothing at all", nil, ScopeTeam, OutcomeRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Authorize(grants(t, tc.caps...), "notes", tc.scope, false)
			if d.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v (err=%v)", d.Outcome, tc.want, err)
			}
			if tc.want == OutcomeRefused {
				if err == nil || !errors.Is(err, ErrRefused) {
					t.Fatalf("err = %v, want ErrRefused", err)
				}
				if d.Reason == "" {
					t.Error("a refusal carries no reason")
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

func TestAuthorize_IdentityAndNamespaceComeFromTheGrants(t *testing.T) {
	id := authz.Identity{Subject: "the-real-user", Issuer: "https://idp.example.com", Email: "a@b.c"}
	g := authz.NewGrants(id, map[string][]authz.Capability{"notes": {authz.CapPersonalWrite}})

	d, err := Authorize(g, "notes", ScopePersonal, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Identity.Subject != id.Subject || d.Identity.Issuer != id.Issuer || d.Identity.Email != id.Email {
		t.Errorf("Identity = %+v, want the verified one %+v", d.Identity, id)
	}
	if d.Namespace != Namespace(id) {
		t.Errorf("Namespace = %q, want %q", d.Namespace, Namespace(id))
	}
	// There is no argument to Authorize that names a subject, an owner
	// or a namespace: the only inputs are the grants, the collection and
	// the scope. That is the anti-spoofing property, and it is
	// structural rather than checked.
}

func TestAuthorize_AnonymousPersonalWrite(t *testing.T) {
	anon := authz.NewGrants(authz.Identity{}, map[string][]authz.Capability{"notes": {authz.CapPersonalWrite, authz.CapTeamWrite}})

	// Hosted: refused. Every anonymous caller would share one namespace.
	d, err := Authorize(anon, "notes", ScopePersonal, false)
	if err == nil || !errors.Is(err, ErrRefused) {
		t.Fatalf("anonymous personal write on a hosted transport: err = %v, want ErrRefused", err)
	}
	if !strings.Contains(d.Reason, "verified identity") {
		t.Errorf("reason = %q, want it to explain the missing identity", d.Reason)
	}

	// stdio: allowed, in the fixed anonymous namespace.
	d, err = Authorize(anon, "notes", ScopePersonal, true)
	if err != nil {
		t.Fatalf("anonymous personal write on stdio: %v", err)
	}
	if d.Outcome != OutcomeWrite || d.Namespace != anonymousNamespace {
		t.Errorf("outcome=%v namespace=%q, want a write in %q", d.Outcome, d.Namespace, anonymousNamespace)
	}

	// A team write needs no identity at all, so the exemption does not
	// change it either way.
	if d, err := Authorize(anon, "notes", ScopeTeam, false); err != nil || d.Outcome != OutcomeWrite {
		t.Errorf("anonymous team write: outcome=%v err=%v", d.Outcome, err)
	}
}

func TestAuthorize_NoPolicyInForceAllowsEverything(t *testing.T) {
	// A nil *Grants is "no policy in force" — stdio, and any hosted
	// deployment with no auth: block. It must behave exactly as meerkat
	// did before authorization existed.
	for _, scope := range AllScopes() {
		d, err := Authorize(nil, "notes", scope, true)
		if err != nil || d.Outcome != OutcomeWrite {
			t.Errorf("nil grants, scope %s: outcome=%v err=%v", scope, d.Outcome, err)
		}
	}
}

func TestAuthorize_RefusalNamesTheCapabilityAndWhatIsHeld(t *testing.T) {
	_, err := Authorize(grants(t, authz.CapRead), "notes", ScopeTeam, false)
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"team-write", `"notes"`, "read"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}
}

func TestAuthorize_WildcardGrantCoversEveryCollection(t *testing.T) {
	g := authz.NewGrants(
		authz.Identity{Subject: "s", Issuer: "i"},
		map[string][]authz.Capability{authz.Wildcard: {authz.CapTeamWrite}},
	)
	for _, name := range []string{"notes", "runbooks", "a-collection-added-later"} {
		d, err := Authorize(g, name, ScopeTeam, false)
		if err != nil || d.Outcome != OutcomeWrite {
			t.Errorf("%s: outcome=%v err=%v", name, d.Outcome, err)
		}
	}
}
