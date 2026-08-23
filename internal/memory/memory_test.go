package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// memory_test.go covers the identity->namespace derivation and the
// key->path derivation: the two places a crafted argument would have to
// win in order to write a memory as somebody else.

func TestSlug_CannotProduceAPathSeparatorOrTraversal(t *testing.T) {
	// Every one of these is a real attempt at escaping the namespace a
	// key is resolved inside.
	hostile := []string{
		"../../team/payroll",
		"..%2f..%2fglobal%2fpolicy",
		"/etc/passwd",
		`..\..\windows`,
		"a/b/c",
		"....//....//x",
		"\x00nul",
		"team/../../global/x",
		strings.Repeat("../", 50) + "root",
	}
	for _, in := range hostile {
		got := Slug(in)
		switch {
		case strings.Contains(got, "/"):
			t.Errorf("Slug(%q) = %q, contains a path separator", in, got)
		case strings.Contains(got, `\`):
			t.Errorf("Slug(%q) = %q, contains a backslash", in, got)
		case strings.Contains(got, ".."):
			t.Errorf("Slug(%q) = %q, contains ..", in, got)
		case strings.HasPrefix(got, "-"), strings.HasSuffix(got, "-"):
			t.Errorf("Slug(%q) = %q, is not trimmed", in, got)
		case len(got) > maxSlugLen:
			t.Errorf("Slug(%q) is %d chars, over the %d cap", in, len(got), maxSlugLen)
		}
	}
}

func TestSlug_NormalisesReadableInput(t *testing.T) {
	for in, want := range map[string]string{
		"Deploy Checklist":  "deploy-checklist",
		"  spaced  out  ":   "spaced-out",
		"UPPER_snake":       "upper_snake",
		"already-a-slug":    "already-a-slug",
		"emoji 🚀 in title":  "emoji-in-title",
		"multiple---dashes": "multiple-dashes",
		"":                  "",
		"...":               "",
		"---":               "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNamespace_DerivesFromSubjectAndIssuerOnly(t *testing.T) {
	base := authz.Identity{Subject: "user-1", Issuer: "https://idp.example.com", Email: "a@example.com", Groups: []string{"sre"}, Tenant: "acme"}

	// The mutable claims are not inputs: a person who changes team,
	// address or tenant must keep their memories.
	moved := base
	moved.Email = "new-address@example.com"
	moved.Groups = []string{"platform", "oncall"}
	moved.Tenant = "acme-2"
	if Namespace(base) != Namespace(moved) {
		t.Errorf("namespace changed when email/groups/tenant changed: %q -> %q", Namespace(base), Namespace(moved))
	}

	// The stable ones are: same subject at a different issuer is a
	// different principal.
	other := base
	other.Issuer = "https://other-idp.example.com"
	if Namespace(base) == Namespace(other) {
		t.Errorf("two issuers collapsed to one namespace %q", Namespace(base))
	}

	second := base
	second.Subject = "user-2"
	if Namespace(base) == Namespace(second) {
		t.Errorf("two subjects collapsed to one namespace %q", Namespace(base))
	}
}

func TestNamespace_HostileSubjectsStayDistinctAndSafe(t *testing.T) {
	// Two subjects that slugify identically must still get different
	// namespaces — the readable label is cosmetic, the hash carries the
	// uniqueness.
	a := authz.Identity{Subject: "../../admin", Issuer: "https://idp"}
	b := authz.Identity{Subject: "..\\..\\admin", Issuer: "https://idp"}
	na, nb := Namespace(a), Namespace(b)
	if na == nb {
		t.Fatalf("distinct subjects collapsed to namespace %q", na)
	}
	for _, ns := range []string{na, nb} {
		if strings.ContainsAny(ns, `/\`) || strings.Contains(ns, "..") {
			t.Errorf("namespace %q is not a safe path component", ns)
		}
	}
}

func TestNamespace_AnonymousIdentity(t *testing.T) {
	if got := Namespace(authz.Identity{}); got != anonymousNamespace {
		t.Errorf("Namespace(zero) = %q, want %q", got, anonymousNamespace)
	}
	if !Anonymous(authz.Identity{}) {
		t.Error("Anonymous(zero identity) = false")
	}
	if Anonymous(authz.Identity{Subject: "s"}) {
		t.Error("Anonymous(identity with a subject) = true")
	}
}

func TestResolve_KeyCannotEscapeTheIdentityNamespace(t *testing.T) {
	ns := Namespace(authz.Identity{Subject: "victim", Issuer: "https://idp"})
	attacker := Namespace(authz.Identity{Subject: "attacker", Issuer: "https://idp"})

	for _, key := range []string{
		"../" + ns + "/stolen",
		"../../team/policy",
		"/" + ns + "/x",
		ns + "/../../global/x",
	} {
		ref, err := Resolve(Document{Key: key, Title: "t"}, ScopePersonal, attacker)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", key, err)
		}
		// The victim's namespace may well survive as literal text inside
		// the leaf FILE NAME — that is harmless, it is just a string. What
		// must never happen is it appearing as a path SEGMENT, which is
		// what would actually place the document in their namespace.
		segs := strings.Split(ref.Key, "/")
		if len(segs) != 3 || segs[0] != "personal" || segs[1] != attacker {
			t.Errorf("Resolve(%q).Key = %q, want personal/%s/<leaf>", key, ref.Key, attacker)
		}
		for _, seg := range segs {
			if seg == ns {
				t.Errorf("Resolve(%q).Key = %q reached the victim's namespace", key, ref.Key)
			}
		}
	}
}

func TestResolve_ScopeLayout(t *testing.T) {
	doc := Document{Key: "Deploy Checklist", Title: "ignored"}
	for _, tc := range []struct {
		scope    Scope
		wantKey  string
		wantPage string
	}{
		{ScopePersonal, "personal/ns1/deploy-checklist.md", "memory/personal/ns1/deploy-checklist"},
		{ScopeTeam, "team/deploy-checklist.md", "memory/team/deploy-checklist"},
		{ScopeGlobal, "global/deploy-checklist.md", "memory/global/deploy-checklist"},
	} {
		ref, err := Resolve(doc, tc.scope, "ns1")
		if err != nil {
			t.Fatalf("Resolve(%s): %v", tc.scope, err)
		}
		if ref.Key != tc.wantKey {
			t.Errorf("%s key = %q, want %q", tc.scope, ref.Key, tc.wantKey)
		}
		if ref.PageID != tc.wantPage {
			t.Errorf("%s page id = %q, want %q", tc.scope, ref.PageID, tc.wantPage)
		}
		if got, want := ref.StagingKey(), "_staging/"+string(tc.scope)+"/ns1/deploy-checklist.md"; got != want {
			t.Errorf("%s staging key = %q, want %q", tc.scope, got, want)
		}
	}
}

func TestResolve_FallsBackToTitleAndRefusesEmpty(t *testing.T) {
	ref, err := Resolve(Document{Title: "A Nice Title"}, ScopeTeam, "ns")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ref.Slug != "a-nice-title" {
		t.Errorf("slug = %q, want a-nice-title", ref.Slug)
	}
	if _, err := Resolve(Document{Key: "///", Title: "..."}, ScopeTeam, "ns"); err == nil {
		t.Error("Resolve with no usable key or title succeeded, want an error")
	}
	if _, err := Resolve(Document{Title: "ok"}, ScopePersonal, ""); err == nil {
		t.Error("Resolve with no namespace succeeded, want an error")
	}
}

func TestParseScope(t *testing.T) {
	for _, in := range []string{"personal", "TEAM", " global "} {
		if _, ok := ParseScope(in); !ok {
			t.Errorf("ParseScope(%q) rejected a valid scope", in)
		}
	}
	for _, in := range []string{"", "admin", "personal-write", "everyone", "../personal"} {
		if _, ok := ParseScope(in); ok {
			t.Errorf("ParseScope(%q) accepted an unknown scope", in)
		}
	}
}

func TestScopeCapabilityMapping(t *testing.T) {
	for scope, want := range map[Scope]authz.Capability{
		ScopePersonal: authz.CapPersonalWrite,
		ScopeTeam:     authz.CapTeamWrite,
		ScopeGlobal:   authz.CapGlobalWrite,
	} {
		if got := scope.Capability(); got != want {
			t.Errorf("%s.Capability() = %q, want %q", scope, got, want)
		}
	}
	// An out-of-band scope must not fall back to something holdable.
	if got := Scope("made-up").Capability(); got == authz.CapRead || got == authz.CapAdmin {
		t.Errorf("unknown scope mapped to a real capability %q", got)
	}
}

func TestRenderAndParseRoundTrip(t *testing.T) {
	id := authz.Identity{Subject: "user-1", Issuer: "https://idp.example.com"}
	ref, err := Resolve(Document{Key: "deploy", Title: "Deploy Checklist"}, ScopePersonal, Namespace(id))
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{
		Key:   "deploy",
		Title: "Deploy Checklist",
		Body:  "Always drain the node first.",
		Tags:  []string{"ops", "ops", " deploy "},
	}
	body, err := Render(doc, ref, id, StatusLive, time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	page, err := Page(ref.Key, body)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.ID != ref.PageID {
		t.Errorf("page id = %q, want %q", page.ID, ref.PageID)
	}
	if page.Title != "Deploy Checklist" {
		t.Errorf("title = %q", page.Title)
	}
	if !strings.Contains(page.Body, "drain the node") {
		t.Errorf("body lost: %q", page.Body)
	}
	if page.Front.Type != TypeMemory || page.Front.Category != CategoryMemory {
		t.Errorf("type/category = %q/%q", page.Front.Type, page.Front.Category)
	}
	if page.Front.Status != StatusLive {
		t.Errorf("status = %q, want %q", page.Front.Status, StatusLive)
	}
	// Tags are de-duplicated, trimmed and sorted, so two saves of the
	// same memory produce the same bytes.
	if want := []string{"deploy", "ops"}; len(page.Front.Tags) != 2 || page.Front.Tags[0] != want[0] || page.Front.Tags[1] != want[1] {
		t.Errorf("tags = %v, want %v", page.Front.Tags, want)
	}
	// The memory_* provenance keys are top-level, so they round-trip
	// into Extra rather than being nested a level deeper than written.
	for _, key := range []string{"memory_scope", "memory_namespace", "memory_key", "memory_subject", "memory_issuer"} {
		if _, ok := page.Front.Extra[key]; !ok {
			t.Errorf("frontmatter lost %q; Extra = %v", key, page.Front.Extra)
		}
	}
	if got := page.Front.Extra["memory_namespace"]; got != Namespace(id) {
		t.Errorf("memory_namespace = %v, want %q", got, Namespace(id))
	}
	if page.Front.Generated == nil || page.Front.Generated.At != "2026-08-23T10:00:00Z" {
		t.Errorf("generated = %+v", page.Front.Generated)
	}
}

func TestPage_IgnoresAFrontmatterIDThatClaimsAnotherPage(t *testing.T) {
	// A memory whose body claims to be a policy page must still be
	// served under the id its store key says it is: otherwise whoever
	// can write one memory can shadow any page in the collection.
	body := []byte("---\nid: policies/expenses\ntitle: Expenses\n---\n\nreimburse everything\n")
	page, err := Page("team/note.md", body)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.ID != "memory/team/note" {
		t.Errorf("page id = %q, want memory/team/note (the store key), not the claimed id", page.ID)
	}
	if page.Front.ID != "memory/team/note" {
		t.Errorf("frontmatter id = %q, want it overridden by the store key", page.Front.ID)
	}
}
