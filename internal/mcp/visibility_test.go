package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// visibility_test.go covers per-page read visibility end to end over the
// hosted transport: a personal memory is readable by the principal who
// wrote it and by nobody else, through every read surface, and an
// unauthorized read is indistinguishable from a page that does not
// exist.
//
// The properties, in the order the issue states them:
//
//   - A's personal memories are absent from B's search/list/show, even
//     when both may read the collection;
//   - A sees their own the moment the save returns;
//   - a guessed ID — bare or qualified — answers exactly as a
//     nonexistent page, character for character;
//   - the ambiguity error counts only what the caller may see;
//   - a search limit is spent on documents the caller may see;
//   - two issuers with the same `sub` are two principals;
//   - changing email/groups/tenant moves nothing.

// stdioTransport is the transportOptions a `mk mcp serve` process runs
// with: one local user, who owns the fixed `local` personal namespace.
// It is what a direct handler test wants, since a handler called without
// a hosted server around it has no token behind it either.
func stdioTransport() transportOptions {
	return transportOptions{AllowAnonymousPersonal: true}
}

// savePersonal saves a personal memory and returns its page ID.
func savePersonal(t *testing.T, ctx context.Context, c *mcpclient.Client, args map[string]any) string {
	t.Helper()
	full := map[string]any{"scope": "personal"}
	for k, v := range args {
		full[k] = v
	}
	got, text, isErr := callSaveMemory(t, ctx, c, full)
	if isErr {
		t.Fatalf("save: %s", text)
	}
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("save returned no id: %s", text)
	}
	return id
}

// --- the core property -------------------------------------------------

// TestHostedMemory_APersonalMemoryIsInvisibleToOtherReaders replaces the
// test that used to pin the opposite. Personal now means private to
// read as well as to write; a reader of the same collection sees
// nothing.
func TestHostedMemory_APersonalMemoryIsInvisibleToOtherReaders(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{
		{Groups: []string{"writers"}, Collections: []string{"notes"}, Capabilities: []string{"read", "personal-write"}},
		{Groups: []string{"readers"}, Collections: []string{"notes"}, Capabilities: []string{"read"}},
	})
	writer := f.client(ctx, f.token("alice", "writers"))
	id := savePersonal(t, ctx, writer, map[string]any{
		"key": "salary", "title": "Mine", "content": "the axolotl detail",
	})

	// A reader of the same collection, with full `read` on it.
	reader := f.client(ctx, f.token("rita", "readers"))

	found, _ := callText(t, ctx, reader, toolSearch, map[string]any{"query": "axolotl"})
	if strings.Contains(found, id) || strings.Contains(found, "axolotl detail") {
		t.Errorf("a reader can search another principal's personal memory:\n%s", found)
	}
	listed, _ := callText(t, ctx, reader, toolList, map[string]any{"prefix": "memory/"})
	if strings.Contains(listed, id) || strings.Contains(listed, "Mine") {
		t.Errorf("a reader can list another principal's personal memory:\n%s", listed)
	}
	shown, isErr := callText(t, ctx, reader, toolShow, map[string]any{"id": id})
	if !isErr {
		t.Errorf("a reader can show another principal's personal memory:\n%s", shown)
	}
	if strings.Contains(shown, "axolotl detail") {
		t.Errorf("the refusal leaks the body:\n%s", shown)
	}

	// A second writer — same capabilities as alice, different principal
	// — is no more privileged here than a plain reader.
	other := f.client(ctx, f.token("bob", "writers"))
	found, _ = callText(t, ctx, other, toolSearch, map[string]any{"query": "axolotl"})
	if strings.Contains(found, id) {
		t.Errorf("another writer can search alice's personal memory:\n%s", found)
	}
}

// TestHostedMemory_AdminDoesNotConferAnotherPrincipalsMemories pins
// that ownership is not a capability. `admin` implies every capability
// over a COLLECTION, present and future — it does not make its holder
// somebody else. An operator who needs to reach a personal memory does
// it at the store, where the access is auditable, or configures
// personal_visibility: collection and accepts what that says.
func TestHostedMemory_AdminDoesNotConferAnotherPrincipalsMemories(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{
		{Groups: []string{"writers"}, Collections: []string{"notes"}, Capabilities: []string{"read", "personal-write"}},
		{Groups: []string{"admins"}, Collections: []string{"*"}, Capabilities: []string{"admin"}},
	})
	alice := f.client(ctx, f.token("alice", "writers"))
	id := savePersonal(t, ctx, alice, map[string]any{
		"key": "salary", "title": "Mine", "content": "the axolotl detail",
	})

	admin := f.client(ctx, f.token("root", "admins"))
	found, _ := callText(t, ctx, admin, toolSearch, map[string]any{"query": "axolotl"})
	if strings.Contains(found, id) || strings.Contains(found, "axolotl detail") {
		t.Errorf("admin can search another principal's personal memory:\n%s", found)
	}
	shown, isErr := callText(t, ctx, admin, toolShow, map[string]any{"id": id})
	if !isErr {
		t.Errorf("admin can show another principal's personal memory:\n%s", shown)
	}
	listed, _ := callText(t, ctx, admin, toolList, map[string]any{"prefix": "memory/"})
	if strings.Contains(listed, id) {
		t.Errorf("admin can list another principal's personal memory:\n%s", listed)
	}
}

// TestHostedMemory_OwnerSeesTheirOwnMemoryImmediately is the other half:
// the feature has to still work. The save must be searchable, listable
// and showable by its owner on the very next call, with no restart —
// including from a second session with a fresh token.
func TestHostedMemory_OwnerSeesTheirOwnMemoryImmediately(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write"},
	}})
	alice := f.client(ctx, f.token("alice", "writers"))
	id := savePersonal(t, ctx, alice, map[string]any{
		"key": "salary", "title": "Mine", "content": "the axolotl detail",
	})

	found, _ := callText(t, ctx, alice, toolSearch, map[string]any{"query": "axolotl"})
	if !strings.Contains(found, id) {
		t.Errorf("the owner cannot search their own memory:\n%s", found)
	}
	listed, _ := callText(t, ctx, alice, toolList, map[string]any{"prefix": "memory/personal/"})
	if !strings.Contains(listed, id) {
		t.Errorf("the owner cannot list their own memory:\n%s", listed)
	}
	shown, isErr := callText(t, ctx, alice, toolShow, map[string]any{"id": id})
	if isErr || !strings.Contains(shown, "axolotl detail") {
		t.Errorf("the owner cannot show their own memory:\n%s", shown)
	}

	// A second session, a second token, the same principal.
	again := f.client(ctx, f.token("alice", "writers"))
	found, _ = callText(t, ctx, again, toolSearch, map[string]any{"query": "axolotl"})
	if !strings.Contains(found, id) {
		t.Errorf("a fresh session of the same principal cannot see the memory:\n%s", found)
	}
}

// TestHostedMemory_GuessedIDAnswersAsANonexistentPage pins the
// invisibility standard: not merely refused, but answered with the same
// bytes a page nobody ever wrote is answered with. Both the bare and the
// qualified form.
func TestHostedMemory_GuessedIDAnswersAsANonexistentPage(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write"},
	}})
	alice := f.client(ctx, f.token("alice", "writers"))
	id := savePersonal(t, ctx, alice, map[string]any{
		"key": "salary", "title": "Mine", "content": "private",
	})
	bob := f.client(ctx, f.token("bob", "writers"))

	// The real ID, and a fictional one in the same namespace.
	fictional := strings.TrimSuffix(id, "salary") + "no-such-memory"
	realAnswer, realIsErr := callText(t, ctx, bob, toolShow, map[string]any{"id": id})
	fictionalAnswer, fictionalIsErr := callText(t, ctx, bob, toolShow, map[string]any{"id": fictional})

	if !realIsErr || !fictionalIsErr {
		t.Fatalf("mk_show did not error: real=%v fictional=%v", realIsErr, fictionalIsErr)
	}
	// The messages differ only where the ID they quote does, so
	// normalise that away and compare the rest exactly.
	if strings.Replace(realAnswer, id, "<ID>", 1) != strings.Replace(fictionalAnswer, fictional, "<ID>", 1) {
		t.Errorf("a real hidden ID answers differently from a fictional one:\n%s\n%s", realAnswer, fictionalAnswer)
	}

	// The qualified form degrades the same way.
	qualified, qIsErr := callText(t, ctx, bob, toolShow, map[string]any{"id": "notes:" + id})
	qFictional, qfIsErr := callText(t, ctx, bob, toolShow, map[string]any{"id": "notes:" + fictional})
	if !qIsErr || !qfIsErr {
		t.Fatalf("qualified mk_show did not error: real=%v fictional=%v", qIsErr, qfIsErr)
	}
	if strings.Replace(qualified, id, "<ID>", 1) != strings.Replace(qFictional, fictional, "<ID>", 1) {
		t.Errorf("a qualified hidden ID answers differently from a qualified fictional one:\n%s\n%s", qualified, qFictional)
	}

	// Naming the collection explicitly changes nothing.
	withArg, wIsErr := callText(t, ctx, bob, toolShow, map[string]any{"id": id, "collection": "notes"})
	if !wIsErr || strings.Contains(withArg, "private") {
		t.Errorf("mk_show with an explicit collection leaked: %s", withArg)
	}
}

// TestHostedMemory_AmbiguityDoesNotCountInvisibleMemories pins that the
// ambiguity error — meerkat's richest enumeration surface — counts only
// what the caller may see, for pages exactly as it already did for
// collections.
func TestHostedMemory_AmbiguityDoesNotCountInvisibleMemories(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes", "secrets"},
		Capabilities: []string{"read", "personal-write"},
	}})
	alice := f.client(ctx, f.token("alice", "writers"))
	// The same key in both collections: one page ID, two collections.
	id := savePersonal(t, ctx, alice, map[string]any{
		"collection": "notes", "key": "salary", "title": "Mine", "content": "private",
	})
	if got := savePersonal(t, ctx, alice, map[string]any{
		"collection": "secrets", "key": "salary", "title": "Mine", "content": "private",
	}); got != id {
		t.Fatalf("the two saves produced different page IDs: %q and %q", id, got)
	}

	// The owner gets a real ambiguity naming both.
	ambiguous, isErr := callText(t, ctx, alice, toolShow, map[string]any{"id": id})
	if !isErr || !strings.Contains(ambiguous, "notes:"+id) || !strings.Contains(ambiguous, "secrets:"+id) {
		t.Fatalf("the owner's mk_show = %q, want an ambiguity naming both collections", ambiguous)
	}

	// Everybody else gets a plain not-found, with no count.
	bob := f.client(ctx, f.token("bob", "writers"))
	answer, isErr := callText(t, ctx, bob, toolShow, map[string]any{"id": id})
	if !isErr {
		t.Fatalf("bob's mk_show succeeded: %s", answer)
	}
	for _, leak := range []string{"exists in", "2 collections", "notes:" + id, "secrets:" + id} {
		if strings.Contains(answer, leak) {
			t.Errorf("the not-found answer leaks %q:\n%s", leak, answer)
		}
	}
	fictional := strings.TrimSuffix(id, "salary") + "invented"
	other, _ := callText(t, ctx, bob, toolShow, map[string]any{"id": fictional})
	if strings.Replace(answer, id, "<ID>", 1) != strings.Replace(other, fictional, "<ID>", 1) {
		t.Errorf("an ambiguous-but-invisible ID answers differently from a fictional one:\n%s\n%s", answer, other)
	}
}

// TestHostedMemory_SearchLimitIsSpentOnVisibleDocuments is the
// acceptance criterion that only a pre-ranking filter can satisfy: with
// the limit consumed by hidden entries, a post-filtered implementation
// returns an empty result and the caller is told, falsely, that there is
// nothing to find.
func TestHostedMemory_SearchLimitIsSpentOnVisibleDocuments(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes", "secrets"},
		Capabilities: []string{"read", "personal-write", "global-write"},
	}})
	alice := f.client(ctx, f.token("alice", "writers"))

	// One globally-visible memory, deliberately the weakest match: the
	// term appears once, in the body.
	if _, text, isErr := callSaveMemory(t, ctx, alice, map[string]any{
		"collection": "notes", "scope": "global", "key": "handbook",
		"title": "Handbook", "content": "an occasional mention of capybara here",
	}); isErr {
		t.Fatalf("global save: %s", text)
	}
	// Twenty private ones that all outrank it: the term is in the title
	// (boosted 5x) and in the ID (3x).
	for i := range 20 {
		savePersonal(t, ctx, alice, map[string]any{
			"collection": "notes",
			"key":        fmt.Sprintf("capybara-%02d", i),
			"title":      fmt.Sprintf("Capybara %02d", i),
			"content":    "capybara capybara capybara",
		})
	}

	bob := f.client(ctx, f.token("bob", "writers"))
	body, isErr := callText(t, ctx, bob, toolSearch, map[string]any{"query": "capybara", "limit": float64(5)})
	if isErr {
		t.Fatalf("search: %s", body)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(body), &hits); err != nil {
		t.Fatalf("search result is not JSON: %v\n%s", err, body)
	}
	if len(hits) != 1 {
		t.Fatalf("bob got %d hits, want the 1 he may see — the limit was consumed by hidden memories:\n%s", len(hits), body)
	}
	if id, _ := hits[0]["id"].(string); id != "memory/global/handbook" {
		t.Errorf("hit = %q, want memory/global/handbook", id)
	}

	// The owner's own limit is filled with her own memories.
	ownBody, isErr := callText(t, ctx, alice, toolSearch, map[string]any{"query": "capybara", "limit": float64(5)})
	if isErr {
		t.Fatalf("owner search: %s", ownBody)
	}
	var ownHits []map[string]any
	if err := json.Unmarshal([]byte(ownBody), &ownHits); err != nil {
		t.Fatal(err)
	}
	if len(ownHits) != 5 {
		t.Errorf("the owner got %d hits, want 5", len(ownHits))
	}
}

// TestHostedMemory_PageCountsDoNotCountInvisibleMemories pins the
// counting surface mk_list_collections exposes: a number that included
// other principals' memories would say how many of them there are.
func TestHostedMemory_PageCountsDoNotCountInvisibleMemories(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{{
		Groups: []string{"writers"}, Collections: []string{"notes"},
		Capabilities: []string{"read", "personal-write"},
	}})
	alice := f.client(ctx, f.token("alice", "writers"))
	bob := f.client(ctx, f.token("bob", "writers"))

	before := collectionPageCount(t, ctx, bob, "notes")
	for i := range 3 {
		savePersonal(t, ctx, alice, map[string]any{
			"key": fmt.Sprintf("note-%d", i), "title": "Note", "content": "private",
		})
	}
	after := collectionPageCount(t, ctx, bob, "notes")
	if after != before {
		t.Errorf("bob's page count moved from %d to %d when alice saved 3 private memories", before, after)
	}
	if own := collectionPageCount(t, ctx, alice, "notes"); own != before+3 {
		t.Errorf("alice's page count = %d, want %d", own, before+3)
	}
}

func collectionPageCount(t *testing.T, ctx context.Context, c *mcpclient.Client, name string) int {
	t.Helper()
	body, isErr := callText(t, ctx, c, toolListCollections, nil)
	if isErr {
		t.Fatalf("mk_list_collections: %s", body)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("mk_list_collections result is not JSON: %v\n%s", err, body)
	}
	for _, entry := range out {
		if entry["name"] == name {
			n, _ := entry["pages"].(float64)
			return int(n)
		}
	}
	t.Fatalf("collection %q absent from mk_list_collections:\n%s", name, body)
	return 0
}

// --- identity ----------------------------------------------------------

// TestHostedMemory_TwoIssuersWithTheSameSubjectAreTwoPrincipals pins
// that `sub` is only unique within an issuer. Two IdPs that both mint
// "user-1" must not share a personal memory space — the read side of the
// rule the namespace derivation already enforces on the write side.
func TestHostedMemory_TwoIssuersWithTheSameSubjectAreTwoPrincipals(t *testing.T) {
	ctx := context.Background()
	f := newTwoIssuerFixture(t)

	first := f.clientFor(ctx, 0, "user-1")
	second := f.clientFor(ctx, 1, "user-1")

	id := savePersonal(t, ctx, first, map[string]any{
		"key": "salary", "title": "Mine", "content": "the axolotl detail",
	})

	// The same `sub` at the other issuer sees nothing of it.
	found, _ := callText(t, ctx, second, toolSearch, map[string]any{"query": "axolotl"})
	if strings.Contains(found, id) || strings.Contains(found, "axolotl detail") {
		t.Errorf("the same sub at another issuer can search the memory:\n%s", found)
	}
	shown, isErr := callText(t, ctx, second, toolShow, map[string]any{"id": id})
	if !isErr {
		t.Errorf("the same sub at another issuer can show the memory:\n%s", shown)
	}
	listed, _ := callText(t, ctx, second, toolList, map[string]any{"prefix": "memory/"})
	if strings.Contains(listed, id) {
		t.Errorf("the same sub at another issuer can list the memory:\n%s", listed)
	}

	// And writing at the same key does not collide: two namespaces, two
	// documents.
	otherID := savePersonal(t, ctx, second, map[string]any{
		"key": "salary", "title": "Mine too", "content": "a different note",
	})
	if otherID == id {
		t.Fatalf("both issuers' user-1 wrote to one page ID %q", id)
	}
	// Each still sees only their own.
	ownFound, _ := callText(t, ctx, first, toolSearch, map[string]any{"query": "axolotl detail"})
	if strings.Contains(ownFound, otherID) {
		t.Errorf("the first principal can see the second's memory:\n%s", ownFound)
	}
}

// TestHostedMemory_ChangedClaimsDoNotMoveOwnership pins that ownership
// is (issuer, subject) and nothing else: the same principal with a new
// email address, new groups and a new tenant still reads their own
// memories, and a DIFFERENT subject carrying the old claims does not
// inherit them.
func TestHostedMemory_ChangedClaimsDoNotMoveOwnership(t *testing.T) {
	ctx := context.Background()
	f := newMemFixture(t, []authz.Rule{
		{Groups: []string{"writers"}, Collections: []string{"notes"}, Capabilities: []string{"read", "personal-write"}},
		{Groups: []string{"platform"}, Collections: []string{"notes"}, Capabilities: []string{"read", "personal-write"}},
	})
	before := f.client(ctx, f.issuer.Token(t, authntest.Claims{
		Subject: "user-1", Audience: testAudience,
		Email: "alice@example.com", Groups: []string{"writers"}, Tenant: "acme",
	}))
	id := savePersonal(t, ctx, before, map[string]any{
		"key": "salary", "title": "Mine", "content": "the axolotl detail",
	})

	// Same subject, same issuer, everything else changed.
	after := f.client(ctx, f.issuer.Token(t, authntest.Claims{
		Subject: "user-1", Audience: testAudience,
		Email: "alice.smith@example.org", Groups: []string{"platform"}, Tenant: "acme-2",
	}))
	found, _ := callText(t, ctx, after, toolSearch, map[string]any{"query": "axolotl"})
	if !strings.Contains(found, id) {
		t.Errorf("changing email/groups/tenant lost the principal their own memory:\n%s", found)
	}
	shown, isErr := callText(t, ctx, after, toolShow, map[string]any{"id": id})
	if isErr || !strings.Contains(shown, "axolotl detail") {
		t.Errorf("the same principal cannot show their memory after a claim change:\n%s", shown)
	}

	// A different subject carrying the ORIGINAL claims inherits nothing.
	impostor := f.client(ctx, f.issuer.Token(t, authntest.Claims{
		Subject: "user-2", Audience: testAudience,
		Email: "alice@example.com", Groups: []string{"writers"}, Tenant: "acme",
	}))
	found, _ = callText(t, ctx, impostor, toolSearch, map[string]any{"query": "axolotl"})
	if strings.Contains(found, id) {
		t.Errorf("another subject inherited the memory by carrying the same email and groups:\n%s", found)
	}
}

// twoIssuerFixture is a hosted server that trusts two independent
// issuers, so a test can mint the same `sub` at each.
type twoIssuerFixture struct {
	*hostedFixture
	issuers []*authntest.Issuer
}

func newTwoIssuerFixture(t *testing.T) *twoIssuerFixture {
	t.Helper()
	f := &twoIssuerFixture{hostedFixture: &hostedFixture{t: t, logs: &bytes.Buffer{}}}
	f.issuers = []*authntest.Issuer{authntest.NewIssuer(t), authntest.NewIssuer(t)}
	// The fixture's own issuer field is the first one, so the shared
	// helpers keep working.
	f.issuer = f.issuers[0]

	// One http.Client that can reach both fake issuers' discovery
	// endpoints. Each authntest issuer serves on its own httptest
	// server, so the default transport reaches both.
	client := &http.Client{}

	dir := t.TempDir()
	col := collections.FromPages("notes", []kb.Page{
		testPage("handbook/onboarding", "Onboarding", "how we onboard", "handbook", "reviewed", "team-a"),
	})
	store, err := memory.OpenLocal(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	if err := col.AttachMemory(context.Background(), store); err != nil {
		t.Fatalf("AttachMemory: %v", err)
	}
	reg, err := collections.New(col)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}

	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: reg,
		Auth: &authz.Config{
			Resource: testResource,
			Providers: []authz.Provider{
				{Issuer: f.issuers[0].URL, Audience: testAudience},
				{Issuer: f.issuers[1].URL, Audience: testAudience},
			},
			Rules: []authz.Rule{{
				Collections:  []string{"notes"},
				Capabilities: []string{"read", "personal-write"},
			}},
		},
		Version:    "test",
		HTTPClient: client,
		Logger:     slog.New(slog.NewJSONHandler(f.logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	f.srv = srv
	f.http = httptest.NewServer(srv.Handler())
	t.Cleanup(f.http.Close)
	return f
}

// clientFor dials as subject, authenticated by the nth issuer.
func (f *twoIssuerFixture) clientFor(ctx context.Context, issuer int, subject string) *mcpclient.Client {
	f.t.Helper()
	return f.client(ctx, f.issuers[issuer].Token(f.t, authntest.Claims{
		Subject: subject, Audience: testAudience,
		Email: subject + "@example.com", Tenant: "acme",
	}))
}

// --- anonymous and local transports ------------------------------------

// TestVisible_AnonymousHostedCallerOwnsNothing pins the fail-closed
// answer for a hosted server that cannot name its caller (no auth:
// block, or allow_unauthenticated in front of a gateway). It already
// refuses to let such a caller WRITE a personal memory; it must not hand
// them somebody else's to read either.
func TestVisible_AnonymousHostedCallerOwnsNothing(t *testing.T) {
	hosted := transportOptions{}
	v := hosted.viewer(nil)
	if v.IsUnfiltered() {
		t.Fatal("an anonymous hosted caller reads unfiltered")
	}
	if v.Owner() != "" {
		t.Errorf("an anonymous hosted caller owns %q, want nothing", v.Owner())
	}
	if v.CanSee(kb.Page{ID: kb.PrivatePrefix + "local/note"}) {
		t.Error("an anonymous hosted caller can read the local namespace")
	}
	if !v.CanSee(kb.Page{ID: "handbook/onboarding"}) {
		t.Error("an anonymous hosted caller cannot read a public page")
	}
}

// TestVisible_StdioCallerOwnsTheLocalNamespace pins the single-user
// experience: `mk mcp serve` writes personal memories into the fixed
// `local` namespace, so it must read that namespace back.
func TestVisible_StdioCallerOwnsTheLocalNamespace(t *testing.T) {
	v := stdioTransport().viewer(nil)
	if v.IsUnfiltered() {
		t.Fatal("stdio reads unfiltered — it should read as the local principal")
	}
	if !v.CanSee(kb.Page{ID: kb.PrivatePrefix + "local/note"}) {
		t.Error("stdio cannot read its own local namespace")
	}
	if v.CanSee(kb.Page{ID: kb.PrivatePrefix + "alice-1111111111111111/note"}) {
		t.Error("stdio can read a hosted principal's namespace out of a shared store")
	}
}

// TestVisible_StdioSeesItsOwnMemoriesEndToEnd drives the stdio-shaped
// handlers directly: the local user saves a personal memory and finds it
// again through all three read tools.
func TestVisible_StdioSeesItsOwnMemoriesEndToEnd(t *testing.T) {
	ctx := context.Background()
	col := collections.FromPages("notes", []kb.Page{
		testPage("handbook/onboarding", "Onboarding", "how we onboard", "handbook", "reviewed", "team-a"),
	})
	store, err := memory.OpenLocal(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if err := col.AttachMemory(ctx, store); err != nil {
		t.Fatal(err)
	}
	reg, err := collections.New(col)
	if err != nil {
		t.Fatal(err)
	}

	res, err := saveMemoryHandler(reg, stdioTransport())(ctx, callTool(map[string]any{
		"scope": "personal", "key": "salary", "title": "Mine", "content": "the axolotl detail",
	}))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if res.IsError {
		t.Fatalf("save: %s", resultText(t, res))
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &saved); err != nil {
		t.Fatal(err)
	}
	id, _ := saved["id"].(string)
	if id != kb.PrivatePrefix+"local/salary" {
		t.Fatalf("stdio saved to %q, want the local namespace", id)
	}

	for _, tc := range []struct {
		tool    string
		handler mcpserver.ToolHandlerFunc
		args    map[string]any
	}{
		{toolSearch, searchHandler(reg, stdioTransport()), map[string]any{"query": "axolotl"}},
		{toolList, listHandler(reg, stdioTransport()), map[string]any{"prefix": "memory/"}},
		{toolShow, showHandler(reg, stdioTransport()), map[string]any{"id": id}},
	} {
		got, err := tc.handler(ctx, callTool(tc.args))
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		text := resultText(t, got)
		if got.IsError || !strings.Contains(text, id) {
			t.Errorf("%s did not return the local user's own memory: %s", tc.tool, text)
		}
	}
}

// --- the collection-wide opt-out ---------------------------------------

// TestHostedMemory_PersonalVisibilityCollectionRestoresTheOldBehaviour
// pins the escape hatch, and the startup warning that comes with it.
func TestHostedMemory_PersonalVisibilityCollectionRestoresTheOldBehaviour(t *testing.T) {
	ctx := context.Background()
	f := newMemFixtureWith(t, []authz.Rule{
		{Groups: []string{"writers"}, Collections: []string{"notes"}, Capabilities: []string{"read", "personal-write"}},
		{Groups: []string{"readers"}, Collections: []string{"notes"}, Capabilities: []string{"read"}},
	}, func(c *collections.Collection) {
		if c.Name == "notes" {
			c.SetPersonalVisibility(memory.VisibilityCollection)
		}
	})

	writer := f.client(ctx, f.token("alice", "writers"))
	id := savePersonal(t, ctx, writer, map[string]any{
		"key": "salary", "title": "Mine", "content": "the axolotl detail",
	})

	reader := f.client(ctx, f.token("rita", "readers"))
	found, _ := callText(t, ctx, reader, toolSearch, map[string]any{"query": "axolotl"})
	if !strings.Contains(found, id) {
		t.Errorf("personal_visibility: collection did not restore collection-wide reads:\n%s", found)
	}
	shown, isErr := callText(t, ctx, reader, toolShow, map[string]any{"id": id})
	if isErr || !strings.Contains(shown, "axolotl detail") {
		t.Errorf("mk_show under the legacy setting: %s", shown)
	}

	// The other collection, which did not opt out, is unaffected.
	logs := f.logs.String()
	if !strings.Contains(logs, "personal memories are readable collection-wide") {
		t.Errorf("no startup warning was logged:\n%s", logs)
	}
	if !strings.Contains(logs, `"collection":"notes"`) {
		t.Errorf("the warning does not name the collection:\n%s", logs)
	}
	if strings.Contains(logs, `"collection":"secrets"`) {
		t.Errorf("the warning named a collection that did not opt out:\n%s", logs)
	}
}

// TestHosted_NoWarningWithoutOIDC pins that the warning is about the
// PAIRING: without authentication there is no principal to keep a memory
// private from, so the setting is not worth a line in the log.
func TestHosted_NoWarningWithoutOIDC(t *testing.T) {
	f := newMemFixtureWith(t, nil, func(c *collections.Collection) {
		c.SetPersonalVisibility(memory.VisibilityCollection)
	})
	if strings.Contains(f.logs.String(), "personal memories are readable collection-wide") {
		t.Errorf("an unauthenticated server warned about a setting that changes nothing there:\n%s", f.logs.String())
	}
}
