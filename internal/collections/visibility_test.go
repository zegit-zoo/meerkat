package collections

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// visibility_test.go covers per-page read visibility at the registry
// level: which pages a view's Pages/Search/Show return, and — the part
// that matters — that a page a viewer may not see is ABSENT rather than
// refused, all the way down to the ambiguity error's count.

const (
	aliceNS = "alice-1111111111111111"
	bobNS   = "bob-2222222222222222"
)

// personalKey is the store key a personal memory for ns lands at.
func personalKey(ns, slug string) string {
	return "personal/" + ns + "/" + slug + ".md"
}

// personalID is the page ID that key resolves to.
func personalID(ns, slug string) string {
	return kb.PrivatePrefix + ns + "/" + slug
}

// memCollWithStore builds a named collection with a local memory store
// and the given content pages.
func memCollWithStore(t *testing.T, name string, pages []kb.Page) *Collection {
	t.Helper()
	c := FromPages(name, pages)
	store, err := memory.OpenLocal(filepath.Join(t.TempDir(), "memory-"+name))
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := c.AttachMemory(context.Background(), store); err != nil {
		t.Fatalf("AttachMemory: %v", err)
	}
	return c
}

// save writes a memory document through the collection.
func save(t *testing.T, c *Collection, key, title, body string) kb.Page {
	t.Helper()
	_, page, err := c.SaveMemory(context.Background(), key, memoryDoc(t, title, body), memory.CreateOnly())
	if err != nil {
		t.Fatalf("SaveMemory(%s): %v", key, err)
	}
	return page
}

func ids(refs []PageRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.QualifiedID()
	}
	return out
}

func hasID(refs []PageRef, id string) bool {
	for _, r := range refs {
		if r.Page.ID == id {
			return true
		}
	}
	return false
}

// --- the core property -------------------------------------------------

func TestViewedBy_PersonalMemoriesAreVisibleOnlyToTheirOwner(t *testing.T) {
	ctx := context.Background()
	c := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "how we onboard")})
	reg, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	save(t, c, personalKey(aliceNS, "salary"), "Salary", "the axolotl detail")
	save(t, c, personalKey(bobNS, "budget"), "Budget", "the axolotl budget")
	save(t, c, "team/runbook.md", "Runbook", "the axolotl runbook")

	alice := reg.ViewedBy(kb.AsOwner(aliceNS))
	bob := reg.ViewedBy(kb.AsOwner(bobNS))

	// list
	alicePages, err := alice.Pages("")
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(alicePages, personalID(aliceNS, "salary")) {
		t.Errorf("alice cannot list her own memory: %v", ids(alicePages))
	}
	if hasID(alicePages, personalID(bobNS, "budget")) {
		t.Errorf("alice can list bob's memory: %v", ids(alicePages))
	}
	if !hasID(alicePages, "memory/team/runbook") {
		t.Error("the team memory disappeared — team reads must be unchanged")
	}
	bobPages, err := bob.Pages("")
	if err != nil {
		t.Fatal(err)
	}
	if hasID(bobPages, personalID(aliceNS, "salary")) {
		t.Errorf("bob can list alice's memory: %v", ids(bobPages))
	}

	// search
	hits, err := bob.Search(ctx, "", "axolotl", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Page.ID == personalID(aliceNS, "salary") {
			t.Error("bob's search found alice's personal memory")
		}
		if strings.Contains(h.Snippet, "axolotl detail") {
			t.Errorf("bob's search snippet leaks alice's body: %q", h.Snippet)
		}
	}

	// show
	if _, err := bob.Show("", personalID(aliceNS, "salary")); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("bob's mk_show of alice's memory = %v, want kb.ErrNotFound", err)
	}
	if _, err := alice.Show("", personalID(aliceNS, "salary")); err != nil {
		t.Errorf("alice cannot show her own memory: %v", err)
	}
}

// TestViewedBy_GuessedIDAnswersExactlyAsANonexistentOne pins that an
// unauthorized read is indistinguishable from a page that was never
// written — the same property a hidden collection has, one level down.
func TestViewedBy_GuessedIDAnswersExactlyAsANonexistentOne(t *testing.T) {
	c := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "how we onboard")})
	reg, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	save(t, c, personalKey(aliceNS, "salary"), "Salary", "private")

	bob := reg.ViewedBy(kb.AsOwner(bobNS))
	real := personalID(aliceNS, "salary")
	fictional := personalID(aliceNS, "no-such-memory")
	nobodys := personalID("carol-3333333333333333", "nothing")

	for _, id := range []string{real, fictional, nobodys, "handbook/does-not-exist"} {
		_, err := bob.Show("", id)
		if !errors.Is(err, kb.ErrNotFound) {
			t.Errorf("Show(%q) = %v, want kb.ErrNotFound", id, err)
		}
	}

	// Qualified forms answer identically too.
	for _, id := range []string{"notes:" + real, "notes:" + fictional} {
		_, err := bob.Show("", id)
		if !errors.Is(err, kb.ErrNotFound) {
			t.Errorf("Show(%q) = %v, want kb.ErrNotFound", id, err)
		}
	}
}

// TestViewedBy_AmbiguityDoesNotCountInvisiblePages is the ambiguity
// oracle, applied to pages instead of collections. The same personal key
// saved into two collections is ambiguous for its OWNER and simply
// absent for everybody else — the error must never say "it exists in 2
// collections" to someone who may see it in none.
func TestViewedBy_AmbiguityDoesNotCountInvisiblePages(t *testing.T) {
	a := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "x")})
	b := memCollWithStore(t, "archive", []kb.Page{page("handbook/history", "History", "y")})
	reg, err := New(a, b)
	if err != nil {
		t.Fatal(err)
	}
	// The same personal memory in both collections: one page ID, two
	// collections.
	save(t, a, personalKey(aliceNS, "salary"), "Salary", "private")
	save(t, b, personalKey(aliceNS, "salary"), "Salary", "private")
	id := personalID(aliceNS, "salary")

	// For the owner: a real ambiguity, naming both qualified IDs.
	_, err = reg.ViewedBy(kb.AsOwner(aliceNS)).Show("", id)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("the owner's Show = %v, want ErrAmbiguous", err)
	}
	if !strings.Contains(err.Error(), "notes:"+id) || !strings.Contains(err.Error(), "archive:"+id) {
		t.Errorf("ambiguity error does not name both: %v", err)
	}

	// For everybody else: not found, with no count and no collection
	// names — byte-identical to a page nobody ever wrote.
	bobErr := showError(t, reg.ViewedBy(kb.AsOwner(bobNS)), id)
	fictionalErr := showError(t, reg.ViewedBy(kb.AsOwner(bobNS)), personalID(aliceNS, "invented"))
	if bobErr != fictionalErr {
		t.Errorf("a real hidden ID answers %q but a fictional one answers %q", bobErr, fictionalErr)
	}
	if strings.Contains(bobErr, "2") || strings.Contains(bobErr, "notes") || strings.Contains(bobErr, "archive") {
		t.Errorf("the not-found answer counts or names something: %q", bobErr)
	}
}

func showError(t *testing.T, reg *Registry, id string) string {
	t.Helper()
	_, err := reg.Show("", id)
	if err == nil {
		t.Fatalf("Show(%q) succeeded, want an error", id)
	}
	return err.Error()
}

// TestViewedBy_CrossCollectionSearchFiltersBeforeTruncation is the
// cross-collection form of the truncation property: the limit must be
// spent on documents the caller may see. Without a clause in each
// collection's query, every slot would be consumed by hidden memories
// and the visible answer would never surface.
func TestViewedBy_CrossCollectionSearchFiltersBeforeTruncation(t *testing.T) {
	ctx := context.Background()
	a := memCollWithStore(t, "notes", nil)
	b := memCollWithStore(t, "archive", []kb.Page{
		// The one visible match, and deliberately the weakest: the term
		// appears once, in the body.
		page("handbook/onboarding", "Onboarding", "an occasional mention of capybara"),
	})
	reg, err := New(a, b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		slug := fmt.Sprintf("capybara-%02d", i)
		save(t, a, personalKey(aliceNS, slug), fmt.Sprintf("Capybara %02d", i), "capybara capybara capybara")
	}

	hits, err := reg.ViewedBy(kb.AsOwner(bobNS)).Search(ctx, "", "capybara", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		got := make([]string, len(hits))
		for i, h := range hits {
			got[i] = h.Collection + ":" + h.Page.ID
		}
		t.Fatalf("bob got %d hits %v, want exactly the 1 he may see", len(hits), got)
	}
	if hits[0].Page.ID != "handbook/onboarding" {
		t.Errorf("hit = %q, want handbook/onboarding", hits[0].Page.ID)
	}

	// The owner's own limit is spent on her own memories, and the public
	// page still competes for a slot.
	own, err := reg.ViewedBy(kb.AsOwner(aliceNS)).Search(ctx, "", "capybara", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 5 {
		t.Errorf("the owner got %d hits, want 5", len(own))
	}
}

// --- the unrestricted default -----------------------------------------

// TestRegistry_WithNoViewerReadsEverything pins the back-compat path: a
// registry nobody attached a viewer to behaves exactly as it did before
// per-page visibility existed. That is the CLI, `mk http serve`, and
// every existing test.
func TestRegistry_WithNoViewerReadsEverything(t *testing.T) {
	ctx := context.Background()
	c := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "how we onboard")})
	reg, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	save(t, c, personalKey(aliceNS, "salary"), "Salary", "the axolotl detail")
	save(t, c, personalKey(bobNS, "budget"), "Budget", "the axolotl budget")

	pages, err := reg.Pages("")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{personalID(aliceNS, "salary"), personalID(bobNS, "budget"), "handbook/onboarding"} {
		if !hasID(pages, want) {
			t.Errorf("an unrestricted registry does not list %q: %v", want, ids(pages))
		}
	}
	hits, err := reg.Search(ctx, "", "axolotl", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Errorf("an unrestricted search found %d of 2 memories", len(hits))
	}
	if _, err := reg.Show("", personalID(aliceNS, "salary")); err != nil {
		t.Errorf("an unrestricted Show cannot read a personal memory: %v", err)
	}
}

// TestViewedBy_ComposesWithRestrictInEitherOrder pins that the two
// narrowings are independent: which collections exist, and which pages
// inside them do.
func TestViewedBy_ComposesWithRestrictInEitherOrder(t *testing.T) {
	a := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "x")})
	b := memCollWithStore(t, "secrets", []kb.Page{page("payroll/salaries", "Salaries", "y")})
	reg, err := New(a, b)
	if err != nil {
		t.Fatal(err)
	}
	save(t, a, personalKey(aliceNS, "note"), "Note", "private")

	onlyNotes := func(name string) bool { return name == "notes" }
	for _, tc := range []struct {
		name string
		view *Registry
	}{
		{"restrict then view", reg.Restrict(onlyNotes).ViewedBy(kb.AsOwner(bobNS))},
		{"view then restrict", reg.ViewedBy(kb.AsOwner(bobNS)).Restrict(onlyNotes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.view.Names(); len(got) != 1 || got[0] != "notes" {
				t.Fatalf("collections in view = %v, want [notes]", got)
			}
			pages, err := tc.view.Pages("")
			if err != nil {
				t.Fatal(err)
			}
			if hasID(pages, personalID(aliceNS, "note")) {
				t.Error("the page viewer was lost when the two composed")
			}
			if !hasID(pages, "handbook/onboarding") {
				t.Error("the public page went missing")
			}
		})
	}
}

// TestViewedBy_DoesNotMutateTheRegistryItDerivesFrom pins that a view is
// a view: one request's viewer must never become another's.
func TestViewedBy_DoesNotMutateTheRegistryItDerivesFrom(t *testing.T) {
	c := memCollWithStore(t, "notes", nil)
	reg, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	save(t, c, personalKey(aliceNS, "note"), "Note", "private")

	_ = reg.ViewedBy(kb.AsOwner(bobNS))
	pages, err := reg.Pages("")
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(pages, personalID(aliceNS, "note")) {
		t.Error("deriving a view changed the registry it was derived from")
	}
	// A derived view borrows rather than owns, so closing it is a no-op.
	if err := reg.ViewedBy(kb.AsOwner(aliceNS)).Close(); err != nil {
		t.Errorf("Close on a derived view: %v", err)
	}
	if _, err := c.Index(); err != nil {
		t.Errorf("the underlying collection's index was torn down: %v", err)
	}
}

// --- the collection-wide opt-out ---------------------------------------

func TestSetPersonalVisibility_CollectionRestoresTheLegacyBehaviour(t *testing.T) {
	ctx := context.Background()
	c := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "x")})
	c.SetPersonalVisibility(memory.VisibilityCollection)
	reg, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	save(t, c, personalKey(aliceNS, "salary"), "Salary", "the axolotl detail")

	bob := reg.ViewedBy(kb.AsOwner(bobNS))
	pages, err := bob.Pages("")
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(pages, personalID(aliceNS, "salary")) {
		t.Error("personal_visibility: collection did not restore collection-wide reads")
	}
	if _, err := bob.Show("", personalID(aliceNS, "salary")); err != nil {
		t.Errorf("Show under the legacy setting: %v", err)
	}
	hits, err := bob.Search(ctx, "", "axolotl", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("search under the legacy setting found %d hits, want 1", len(hits))
	}
	if !c.PersonalReadsAreCollectionWide() {
		t.Error("PersonalReadsAreCollectionWide() = false after opting in")
	}
}

func TestSetPersonalVisibility_AnythingElseStaysPrivate(t *testing.T) {
	c := FromPages("notes", nil)
	for _, v := range []string{"", memory.VisibilityPrivate, "public", "PRIVATE", "Collection"} {
		c.SetPersonalVisibility(v)
		if c.PersonalReadsAreCollectionWide() {
			t.Errorf("SetPersonalVisibility(%q) opened personal reads", v)
		}
	}
	// The zero value of a collection nobody configured is private too.
	if FromPages("fresh", nil).PersonalReadsAreCollectionWide() {
		t.Error("a collection built by FromPages defaults to collection-wide personal reads")
	}
}

// TestOpen_AppliesThePersonalVisibilityConfig pins the config plumbing
// end to end: what an operator writes in content-source.yaml is what the
// mounted collection enforces — and, for the collections that say
// nothing, that the answer is the secure one.
func TestOpen_AppliesThePersonalVisibilityConfig(t *testing.T) {
	dirs := map[string]string{}
	for _, name := range []string{"legacy", "explicit", "silent", "nostore"} {
		d := t.TempDir()
		writeTree(t, d, map[string]string{"wiki/a.md": "---\nid: a\ntitle: A\n---\nbody\n"})
		dirs[name] = d
	}
	spec := func(dir, visibility string) *memory.Spec {
		return &memory.Spec{
			Type:               memory.BackendLocal,
			Path:               filepath.Join(dir, "memory-store"),
			PersonalVisibility: visibility,
		}
	}
	layout := contentsource.MergeLayout(contentsource.Layout{})
	reg, err := Open(context.Background(), []contentsource.ResolvedCollection{
		{Name: "legacy", Dir: dirs["legacy"], Source: contentsource.Source{
			Type: "local", Layout: layout, Memory: spec(dirs["legacy"], memory.VisibilityCollection)}},
		{Name: "explicit", Dir: dirs["explicit"], Source: contentsource.Source{
			Type: "local", Layout: layout, Memory: spec(dirs["explicit"], memory.VisibilityPrivate)}},
		{Name: "silent", Dir: dirs["silent"], Source: contentsource.Source{
			Type: "local", Layout: layout, Memory: spec(dirs["silent"], "")}},
		{Name: "nostore", Dir: dirs["nostore"], Source: contentsource.Source{
			Type: "local", Layout: layout}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	for name, want := range map[string]bool{
		"legacy":   true,
		"explicit": false,
		"silent":   false,
		"nostore":  false,
	} {
		c, err := reg.Get(name)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if got := c.PersonalReadsAreCollectionWide(); got != want {
			t.Errorf("collection %q: PersonalReadsAreCollectionWide() = %v, want %v", name, got, want)
		}
	}
}

// --- overlay/content interaction ---------------------------------------

// TestPagesFor_APrivateOverlayDoesNotHideAPublicContentPage pins the
// shadowing rule: the overlay wins over a content page with the same ID,
// but only for the viewers who can actually SEE the overlay entry.
// Otherwise a private document could make a public page disappear for
// everybody else by sitting on its ID.
func TestPagesFor_APrivateOverlayDoesNotHideAPublicContentPage(t *testing.T) {
	// A content page that happens to live under the reserved prefix.
	id := personalID(aliceNS, "overlap")
	c := memCollWithStore(t, "notes", []kb.Page{page(id, "Content page", "from the content tree")})
	save(t, c, personalKey(aliceNS, "overlap"), "Overlay", "from the overlay")

	own, err := c.PagesFor(kb.AsOwner(aliceNS))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range own {
		if p.ID == id {
			n++
			if p.Title != "Overlay" {
				t.Errorf("the owner sees %q, want the overlay to win", p.Title)
			}
		}
	}
	if n != 1 {
		t.Errorf("the owner sees the ID %d times, want once", n)
	}

	// Another principal sees neither: the reserved prefix makes the
	// content page private to alice too, which is the safe direction.
	other, err := c.PagesFor(kb.AsOwner(bobNS))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range other {
		if p.ID == id {
			t.Errorf("another principal sees %q under the reserved prefix", p.ID)
		}
	}
}

// TestPagesFor_ReservedPrefixAppliesToContentPagesToo pins that the
// prefix is reserved everywhere, not only in the memory overlay: an
// ingested page under memory/personal/<x>/ is private to <x>, so there
// is no way to publish into somebody's private namespace and no
// ambiguity about which tree owns the ID.
func TestPagesFor_ReservedPrefixAppliesToContentPagesToo(t *testing.T) {
	c := FromPages("notes", []kb.Page{
		page("handbook/onboarding", "Onboarding", "public"),
		page(personalID(aliceNS, "planted"), "Planted", "from the content tree"),
	})
	pages, err := c.PagesFor(kb.AsOwner(bobNS))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].ID != "handbook/onboarding" {
		t.Errorf("bob sees %d pages, want only the public one", len(pages))
	}
	if _, err := c.LoadFor(kb.AsOwner(bobNS), personalID(aliceNS, "planted")); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("LoadFor = %v, want kb.ErrNotFound", err)
	}
	if _, err := c.LoadFor(kb.AsOwner(aliceNS), personalID(aliceNS, "planted")); err != nil {
		t.Errorf("the owner cannot load it: %v", err)
	}
}

// --- remount -----------------------------------------------------------

// TestViewedBy_SurvivesARemount pins that visibility is a property of
// the stored document, not of the process that wrote it: a memory
// re-read from its store into a fresh overlay is still private to the
// same principal.
//
// This is the path issue #28's hot reload will take — rebuild the
// overlay and the index from the store — so it is pinned here rather
// than left implicit. (Whether the two BACKENDS agree about the key a
// document comes back under is pinned one layer down, in
// internal/memory's visibility_test.go, where the GCS fake lives.)
func TestViewedBy_SurvivesARemount(t *testing.T) {
	ctx := context.Background()
	c := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "the axolotl handbook")})
	save(t, c, personalKey(aliceNS, "salary"), "Salary", "the axolotl detail")
	id := personalID(aliceNS, "salary")

	remounted := FromPages("notes", []kb.Page{page("handbook/onboarding", "Onboarding", "the axolotl handbook")})
	if err := remounted.AttachMemory(ctx, c.Memory()); err != nil {
		t.Fatalf("remount: %v", err)
	}
	reg, err := New(remounted)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reg.ViewedBy(kb.AsOwner(aliceNS)).Show("", id); err != nil {
		t.Errorf("after a remount the owner cannot read their memory: %v", err)
	}
	if _, err := reg.ViewedBy(kb.AsOwner(bobNS)).Show("", id); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("after a remount the memory became readable by another principal: %v", err)
	}
	hits, err := reg.ViewedBy(kb.AsOwner(bobNS)).Search(ctx, "", "axolotl", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Page.ID != "handbook/onboarding" {
		t.Errorf("after a remount bob's search = %+v, want only the public page", hits)
	}
}

// --- concurrency -------------------------------------------------------

// TestViewedBy_ConcurrentSavesAndRestrictedReadsAreRaceFree runs several
// principals saving and reading through one shared *Collection, which is
// the real hosted shape: one registry, one overlay, one index, many
// sessions. Run under -race, it is the test that the visibility field
// added to the index doc did not introduce a new shared-state problem.
func TestViewedBy_ConcurrentSavesAndRestrictedReadsAreRaceFree(t *testing.T) {
	ctx := context.Background()
	c := memCollWithStore(t, "notes", []kb.Page{page("handbook/onboarding", "Onboarding", "shared body text")})
	reg, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Index(); err != nil {
		t.Fatal(err)
	}
	owners := []string{aliceNS, bobNS, "carol-3333333333333333"}

	var wg sync.WaitGroup
	for _, owner := range owners {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			for i := range 6 {
				key := personalKey(owner, fmt.Sprintf("note-%d", i))
				if _, _, err := c.SaveMemory(ctx, key, memoryDoc(t, "Note", "shared body text"), memory.CreateOnly()); err != nil {
					t.Errorf("SaveMemory(%s): %v", key, err)
					return
				}
			}
		}(owner)
	}
	for _, owner := range owners {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			view := reg.ViewedBy(kb.AsOwner(owner))
			for range 15 {
				pages, err := view.Pages("")
				if err != nil {
					t.Errorf("Pages: %v", err)
					return
				}
				for _, p := range pages {
					if o := p.Page.PrivateOwner(); o != "" && o != owner {
						t.Errorf("viewer %q listed %q owned by %q", owner, p.Page.ID, o)
						return
					}
				}
				hits, err := view.Search(ctx, "", "shared body", 100)
				if err != nil {
					t.Errorf("Search: %v", err)
					return
				}
				for _, h := range hits {
					if o := h.Page.PrivateOwner(); o != "" && o != owner {
						t.Errorf("viewer %q found %q owned by %q", owner, h.Page.ID, o)
						return
					}
				}
			}
		}(owner)
	}
	wg.Wait()

	for _, owner := range owners {
		pages, err := reg.ViewedBy(kb.AsOwner(owner)).Pages("")
		if err != nil {
			t.Fatal(err)
		}
		own := 0
		for _, p := range pages {
			if strings.HasPrefix(p.Page.ID, kb.PrivatePrefix) {
				own++
			}
		}
		if own != 6 {
			t.Errorf("owner %q sees %d of their own 6 memories", owner, own)
		}
	}
}
