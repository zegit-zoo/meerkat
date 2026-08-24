package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// visibility_test.go pins the agreement between this package (which
// decides WHERE a personal memory is stored, and therefore what its page
// ID is) and internal/kb (which reads the owner back out of that ID).
// The two are in different packages on purpose — kb must not import a
// feature package — so the contract between them needs a test rather
// than a compiler.

// TestResolve_PersonalPageIDCarriesTheOwner is the load-bearing pin: the
// page ID a personal memory gets must be one kb.PrivateOwner reads the
// namespace back out of. If either side's layout changes without the
// other, memories silently become public — this is the test that stops
// that being silent.
func TestResolve_PersonalPageIDCarriesTheOwner(t *testing.T) {
	for _, id := range []authz.Identity{
		{Subject: "alice", Issuer: "https://idp.example.com"},
		{Subject: "../../admin", Issuer: "https://idp.example.com"},
		{Subject: "Ünïcödé Sübject", Issuer: "https://idp.example.com"},
		{Subject: strings.Repeat("long-subject-", 20), Issuer: "https://idp.example.com"},
		{}, // anonymous: the fixed local namespace
	} {
		ns := Namespace(id)
		ref, err := Resolve(Document{Key: "deploy checklist", Title: "Deploy"}, ScopePersonal, ns)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", id.Subject, err)
		}
		if got := kb.PrivateOwner(ref.PageID); got != ns {
			t.Errorf("kb.PrivateOwner(%q) = %q, want the namespace %q", ref.PageID, got, ns)
		}
		if !strings.HasPrefix(ref.PageID, kb.PrivatePrefix) {
			t.Errorf("personal page ID %q does not start with kb.PrivatePrefix %q", ref.PageID, kb.PrivatePrefix)
		}
	}
}

// TestResolve_TeamAndGlobalPageIDsAreNotPrivate pins the other half:
// team and global memories keep their existing, collection-wide read
// behaviour. Nothing about this change may narrow them.
func TestResolve_TeamAndGlobalPageIDsAreNotPrivate(t *testing.T) {
	ns := Namespace(authz.Identity{Subject: "alice", Issuer: "https://idp.example.com"})
	for _, scope := range []Scope{ScopeTeam, ScopeGlobal} {
		ref, err := Resolve(Document{Key: "runbook"}, scope, ns)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", scope, err)
		}
		if got := kb.PrivateOwner(ref.PageID); got != "" {
			t.Errorf("%s memory %q is private to %q — team/global reads must be unchanged", scope, ref.PageID, got)
		}
	}
}

// TestPage_OwnerComesFromTheStoreKeyNotTheDocument pins that the owner
// travels with the STORE KEY, exactly as the page ID does. A document
// whose frontmatter claims another namespace — the shape an attacker
// would write if the body were trusted — is served under the key's owner
// and nobody else's.
func TestPage_OwnerComesFromTheStoreKeyNotTheDocument(t *testing.T) {
	victim := Namespace(authz.Identity{Subject: "victim", Issuer: "https://idp"})
	attacker := Namespace(authz.Identity{Subject: "attacker", Issuer: "https://idp"})

	body := []byte("---\n" +
		"id: handbook/onboarding\n" +
		"title: Innocuous\n" +
		"memory_scope: global\n" +
		"memory_namespace: " + victim + "\n" +
		"---\n\nbody\n")

	page, err := Page("personal/"+attacker+"/note.md", body)
	if err != nil {
		t.Fatal(err)
	}
	if got := page.PrivateOwner(); got != attacker {
		t.Errorf("PrivateOwner() = %q, want the store key's namespace %q", got, attacker)
	}
	if kb.AsOwner(victim).CanSee(page) {
		t.Error("the victim named in the frontmatter can see the attacker's document")
	}
	if !kb.AsOwner(attacker).CanSee(page) {
		t.Error("the real owner cannot see their own document")
	}
}

// TestNamespace_TwoIssuersWithTheSameSubjectSeeDifferentMemories is the
// read-side consequence of the namespace derivation: `sub` is only
// unique WITHIN an issuer, so two identity providers that both mint
// "user-1" must not share a personal memory space.
func TestNamespace_TwoIssuersWithTheSameSubjectSeeDifferentMemories(t *testing.T) {
	a := authz.Identity{Subject: "user-1", Issuer: "https://idp-a.example.com"}
	b := authz.Identity{Subject: "user-1", Issuer: "https://idp-b.example.com"}

	refA, err := Resolve(Document{Key: "note"}, ScopePersonal, Namespace(a))
	if err != nil {
		t.Fatal(err)
	}
	refB, err := Resolve(Document{Key: "note"}, ScopePersonal, Namespace(b))
	if err != nil {
		t.Fatal(err)
	}
	if refA.PageID == refB.PageID {
		t.Fatalf("two issuers produced one page ID %q", refA.PageID)
	}
	pageA := kb.Page{ID: refA.PageID}
	if kb.AsOwner(Namespace(b)).CanSee(pageA) {
		t.Error("the same sub at another issuer can read this principal's memory")
	}
	if !kb.AsOwner(Namespace(a)).CanSee(pageA) {
		t.Error("the owner cannot read their own memory")
	}
}

// TestNamespace_ChangedClaimsDoNotMoveOwnership pins that ownership
// follows (issuer, subject) and nothing else. A person who changes team,
// email address or tenant keeps reading their own memories — and a
// caller cannot acquire somebody else's by having their directory
// attributes changed to match.
func TestNamespace_ChangedClaimsDoNotMoveOwnership(t *testing.T) {
	before := authz.Identity{
		Subject: "user-1", Issuer: "https://idp.example.com",
		Email: "alice@example.com", Groups: []string{"sre"}, Tenant: "acme",
	}
	after := authz.Identity{
		Subject: "user-1", Issuer: "https://idp.example.com",
		Email: "alice.smith@example.org", Groups: []string{"platform", "oncall"}, Tenant: "acme-2",
	}
	// A different principal who now carries the OLD claims.
	impostor := authz.Identity{
		Subject: "user-2", Issuer: "https://idp.example.com",
		Email: "alice@example.com", Groups: []string{"sre"}, Tenant: "acme",
	}

	ref, err := Resolve(Document{Key: "note"}, ScopePersonal, Namespace(before))
	if err != nil {
		t.Fatal(err)
	}
	page := kb.Page{ID: ref.PageID}

	if !kb.AsOwner(Namespace(after)).CanSee(page) {
		t.Error("changing email/groups/tenant lost the principal their own memory")
	}
	if kb.AsOwner(Namespace(impostor)).CanSee(page) {
		t.Error("another subject inherited the memory by carrying the same email and groups")
	}
}

// TestBackends_AgreeAboutOwnership is the "local and GCS behave
// identically" criterion, tested at the only layer where they could
// disagree.
//
// A personal memory's owner is derived from its STORE KEY, so what has
// to be identical is the key a document comes back from Load under, and
// the page ID Page() then builds from it. Everything above this — the
// overlay, the index, the viewer — is backend-agnostic by construction:
// it only ever sees kb.Pages. So this drives both backends through one
// table, writes at a personal key, reads the whole store back, and
// asserts the owner survives the round trip in both.
func TestBackends_AgreeAboutOwnership(t *testing.T) {
	ctx := context.Background()
	alice := Namespace(authz.Identity{Subject: "alice", Issuer: "https://idp.example.com"})
	bob := Namespace(authz.Identity{Subject: "bob", Issuer: "https://idp.example.com"})

	for _, tc := range []struct {
		name  string
		store func(t *testing.T) Store
	}{
		{BackendLocal, func(t *testing.T) Store {
			s, err := OpenLocal(filepath.Join(t.TempDir(), "memory"))
			if err != nil {
				t.Fatalf("OpenLocal: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
		{BackendGCS, func(t *testing.T) Store {
			s, _ := newFakeGCSStore(t)
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			refs := map[string]Ref{}
			for who, ns := range map[string]string{"alice": alice, "bob": bob} {
				ref, err := Resolve(Document{Key: "salary"}, ScopePersonal, ns)
				if err != nil {
					t.Fatal(err)
				}
				refs[who] = ref
				body, err := Render(Document{Key: "salary", Title: "Salary", Body: "private"},
					ref, authz.Identity{Subject: who, Issuer: "https://idp.example.com"},
					StatusLive, time.Unix(0, 0))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Put(ctx, ref.Key, body, CreateOnly()); err != nil {
					t.Fatalf("Put(%s): %v", ref.Key, err)
				}
			}
			// A team memory too, so "everything came back private" would
			// not pass by accident.
			teamRef, err := Resolve(Document{Key: "runbook"}, ScopeTeam, alice)
			if err != nil {
				t.Fatal(err)
			}
			teamBody, err := Render(Document{Key: "runbook", Title: "Runbook", Body: "shared"},
				teamRef, authz.Identity{Subject: "alice", Issuer: "https://idp.example.com"},
				StatusLive, time.Unix(0, 0))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(ctx, teamRef.Key, teamBody, CreateOnly()); err != nil {
				t.Fatal(err)
			}

			records, err := store.Load(ctx)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(records) != 3 {
				t.Fatalf("Load returned %d records, want 3", len(records))
			}
			owners := map[string]string{}
			for _, rec := range records {
				page, err := Page(rec.Key, rec.Body)
				if err != nil {
					t.Fatalf("Page(%s): %v", rec.Key, err)
				}
				owners[page.ID] = page.PrivateOwner()
			}
			want := map[string]string{
				refs["alice"].PageID: alice,
				refs["bob"].PageID:   bob,
				teamRef.PageID:       "",
			}
			for id, wantOwner := range want {
				got, ok := owners[id]
				if !ok {
					t.Errorf("%s: page %q did not come back from Load", tc.name, id)
					continue
				}
				if got != wantOwner {
					t.Errorf("%s: page %q owner = %q, want %q", tc.name, id, got, wantOwner)
				}
			}
			// And the read consequence, spelled out.
			for _, p := range []struct{ id, owner string }{
				{refs["alice"].PageID, alice},
				{refs["bob"].PageID, bob},
			} {
				if kb.AsOwner(p.owner).CanSeeOwner(owners[p.id]) != true {
					t.Errorf("%s: the owner cannot see %q", tc.name, p.id)
				}
			}
			if kb.AsOwner(bob).CanSeeOwner(owners[refs["alice"].PageID]) {
				t.Errorf("%s: bob can see alice's memory", tc.name)
			}
		})
	}
}

func TestSpec_Visibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *Spec
		want string
	}{
		{"nil is private", nil, VisibilityPrivate},
		{"unset is private", &Spec{Type: BackendLocal}, VisibilityPrivate},
		{"explicit private", &Spec{Type: BackendLocal, PersonalVisibility: VisibilityPrivate}, VisibilityPrivate},
		{"explicit collection", &Spec{Type: BackendLocal, PersonalVisibility: VisibilityCollection}, VisibilityCollection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Visibility(); got != tc.want {
				t.Errorf("Visibility() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSpec_ValidateRejectsAnUnknownVisibility(t *testing.T) {
	err := (&Spec{Type: BackendLocal, PersonalVisibility: "public"}).
		Validate("collections[notes].memory", false)
	if err == nil {
		t.Fatal("an unknown personal_visibility was accepted — it must be a config error, not an ignored line")
	}
	for _, want := range []string{"personal_visibility", VisibilityPrivate, VisibilityCollection, "public"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
