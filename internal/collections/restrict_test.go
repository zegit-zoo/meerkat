package collections

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// restrict_test.go pins the invisibility contract: a collection a
// caller may not read must be indistinguishable from a collection that
// was never mounted. Every test below is an ENUMERATION ORACLE that a
// merely-denying implementation would leave open.

// threeCollections mounts runbooks, architecture and secrets. All three
// hold "shared/overview", so a filtered registry that still counted the
// hidden ones would give itself away through the ambiguity error.
func threeCollections(t *testing.T) *Registry {
	t.Helper()
	reg, err := New(
		FromPages("runbooks", []kb.Page{
			page("incidents/paging", "Paging", "who to page during an incident"),
			page("shared/overview", "Runbook Overview", "overview of the runbooks"),
		}),
		FromPages("architecture", []kb.Page{
			page("adr/0001-storage", "ADR 1 Storage", "we chose object storage"),
			page("shared/overview", "Architecture Overview", "overview of the architecture"),
		}),
		FromPages("secrets", []kb.Page{
			page("payroll/salaries", "Salaries", "confidential compensation data"),
			page("shared/overview", "Secrets Overview", "overview of the secrets"),
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// only builds an allow predicate over a fixed name set.
func only(names ...string) func(string) bool {
	allowed := make(map[string]bool, len(names))
	for _, n := range names {
		allowed[n] = true
	}
	return func(name string) bool { return allowed[name] }
}

func TestRestrict_HidesFromEveryEnumeration(t *testing.T) {
	view := threeCollections(t).Restrict(only("runbooks", "architecture"))

	if got := strings.Join(view.Names(), ","); got != "runbooks,architecture" {
		t.Errorf("Names() = %q — a hidden collection must not be named", got)
	}
	if view.Len() != 2 {
		t.Errorf("Len() = %d, want 2", view.Len())
	}
	if len(view.All()) != 2 {
		t.Errorf("All() returned %d collections, want 2", len(view.All()))
	}
	for _, c := range view.All() {
		if c.Name == "secrets" {
			t.Fatal("All() exposed a hidden collection")
		}
	}
}

func TestRestrict_UnknownCollectionErrorIsNotAnOracle(t *testing.T) {
	view := threeCollections(t).Restrict(only("runbooks"))

	hidden, hiddenErr := view.Get("secrets")
	absent, absentErr := view.Get("no-such-collection")

	if hidden != nil || absent != nil {
		t.Fatal("neither lookup should return a collection")
	}
	if !errors.Is(hiddenErr, ErrUnknownCollection) || !errors.Is(absentErr, ErrUnknownCollection) {
		t.Fatalf("both should wrap ErrUnknownCollection: %v / %v", hiddenErr, absentErr)
	}
	// The two messages differ only in the name echoed back. If a hidden
	// collection produced a different SHAPE of error — or appeared in
	// the "available:" list — the message would confirm it exists.
	normalise := func(err error, name string) string {
		return strings.ReplaceAll(err.Error(), name, "<name>")
	}
	if got, want := normalise(hiddenErr, "secrets"), normalise(absentErr, "no-such-collection"); got != want {
		t.Errorf("a hidden collection must error identically to an absent one:\n hidden: %s\n absent: %s", got, want)
	}
	if strings.Contains(hiddenErr.Error(), "available: runbooks, architecture") {
		t.Error("the available: list must be the caller's view, not the mounted set")
	}
	if !strings.Contains(hiddenErr.Error(), "available: runbooks") {
		t.Errorf("the available: list should name the visible collections: %v", hiddenErr)
	}
}

func TestRestrict_AmbiguityCountsOnlyVisibleCollections(t *testing.T) {
	reg := threeCollections(t)

	// Unrestricted: the ID is in all three.
	if _, err := reg.Show("", "shared/overview"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("unrestricted Show should be ambiguous, got %v", err)
	} else if !strings.Contains(err.Error(), "exists in 3 collections") {
		t.Fatalf("expected a count of 3, got %v", err)
	}

	// Two visible: still ambiguous, but the count and the suggested
	// qualified IDs must mention only those two.
	twoVisible := reg.Restrict(only("runbooks", "architecture"))
	_, err := twoVisible.Show("", "shared/overview")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("expected ambiguity, got %v", err)
	}
	if !strings.Contains(err.Error(), "exists in 2 collections") {
		t.Errorf("the count must reflect the caller's view, not the mounted set: %v", err)
	}
	if strings.Contains(err.Error(), "secrets") {
		t.Errorf("the ambiguity error named a hidden collection: %v", err)
	}

	// One visible: no ambiguity at all. The caller gets the page, with
	// no hint that the ID exists elsewhere.
	oneVisible := reg.Restrict(only("runbooks"))
	ref, err := oneVisible.Show("", "shared/overview")
	if err != nil {
		t.Fatalf("Show with one visible collection: %v", err)
	}
	if ref.Collection != "runbooks" || ref.Page.Title != "Runbook Overview" {
		t.Errorf("got %s:%s (%q)", ref.Collection, ref.Page.ID, ref.Page.Title)
	}
}

func TestRestrict_ShowIntoAHiddenCollectionIsNotFound(t *testing.T) {
	view := threeCollections(t).Restrict(only("runbooks"))

	// Named explicitly: the same unknown-collection error a
	// never-mounted name would produce.
	if _, err := view.Show("secrets", "payroll/salaries"); !errors.Is(err, ErrUnknownCollection) {
		t.Errorf("naming a hidden collection should be ErrUnknownCollection, got %v", err)
	}
	// Bare ID that only exists in the hidden collection: not found, not
	// forbidden.
	if _, err := view.Show("", "payroll/salaries"); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("a page only in a hidden collection should be ErrNotFound, got %v", err)
	}
}

func TestRestrict_QualifiedIDForAHiddenCollectionDegradesToABarePageID(t *testing.T) {
	reg := threeCollections(t)
	view := reg.Restrict(only("runbooks"))

	// Unrestricted, "secrets:" is recognised as a qualification.
	if col, id := reg.SplitQualified("secrets:payroll/salaries"); col != "secrets" || id != "payroll/salaries" {
		t.Fatalf("SplitQualified = (%q, %q)", col, id)
	}
	// Restricted, it must NOT be — otherwise the parse result itself
	// tells the caller that a collection named "secrets" is mounted.
	col, id := view.SplitQualified("secrets:payroll/salaries")
	if col != "" || id != "secrets:payroll/salaries" {
		t.Fatalf("SplitQualified = (%q, %q), want the whole string treated as a bare page ID", col, id)
	}
	// And the resulting lookup is a plain not-found.
	if _, err := view.Show("", "secrets:payroll/salaries"); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestRestrict_SearchAndPagesSpanOnlyVisibleCollections(t *testing.T) {
	view := threeCollections(t).Restrict(only("runbooks"))

	refs, err := view.Pages("")
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	for _, ref := range refs {
		if ref.Collection != "runbooks" {
			t.Fatalf("Pages() returned a page from %q", ref.Collection)
		}
	}
	if len(refs) != 2 {
		t.Errorf("Pages() returned %d pages, want runbooks' 2", len(refs))
	}

	hits, err := view.Search(context.Background(), "", "overview", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	for _, h := range hits {
		if h.Collection != "runbooks" {
			t.Fatalf("Search returned a hit from %q", h.Collection)
		}
	}

	// A term that only appears in the hidden collection returns nothing.
	secret, err := view.Search(context.Background(), "", "compensation", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(secret) != 0 {
		t.Fatalf("a term unique to a hidden collection returned %d hits", len(secret))
	}
}

func TestRestrict_ToOneCollectionGivesSingleCollectionBehaviour(t *testing.T) {
	view := threeCollections(t).Restrict(only("architecture"))
	if !view.Single() {
		t.Fatal("a caller who can read exactly one collection should get Single() == true")
	}
	if got := view.Provenance(); got != "memory" {
		t.Errorf("Provenance() = %q, want the single collection's own", got)
	}
}

func TestRestrict_ToNothing(t *testing.T) {
	view := threeCollections(t).Restrict(only())

	if view.Len() != 0 || len(view.Names()) != 0 {
		t.Fatalf("expected an empty view, got %v", view.Names())
	}
	refs, err := view.Pages("")
	if err != nil || len(refs) != 0 {
		t.Fatalf("Pages() = (%d refs, %v), want (0, nil)", len(refs), err)
	}
	hits, err := view.Search(context.Background(), "", "overview", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("Search() = (%d hits, %v), want (0, nil)", len(hits), err)
	}
	if _, err := view.Show("", "shared/overview"); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("Show() = %v, want ErrNotFound", err)
	}
	// The error must not read as "available: " with an empty list.
	_, err = view.Get("runbooks")
	if !errors.Is(err, ErrUnknownCollection) || !strings.Contains(err.Error(), "no collections are mounted") {
		t.Errorf("Get() = %v, want the same message an empty registry would give", err)
	}
}

func TestRestrict_NilPredicateIsIdentity(t *testing.T) {
	reg := threeCollections(t)
	if got := reg.Restrict(nil); got != reg {
		t.Fatal("Restrict(nil) should return the registry itself — no policy costs nothing")
	}
}

func TestRestrict_ViewSharesIndexesAndDoesNotOwnThem(t *testing.T) {
	reg := threeCollections(t)

	// Build the index through the view; the parent must then hold it.
	view := reg.Restrict(only("runbooks"))
	if _, err := view.Search(context.Background(), "", "paging", 5); err != nil {
		t.Fatalf("Search: %v", err)
	}
	built, err := reg.Get("runbooks")
	if err != nil {
		t.Fatal(err)
	}
	if built.builtIndex() == nil {
		t.Fatal("a search through the view should have populated the shared collection's index")
	}

	// Closing the view must be a no-op: the server is still serving
	// other requests from that index.
	if err := view.Close(); err != nil {
		t.Fatalf("view.Close(): %v", err)
	}
	if built.builtIndex() == nil {
		t.Fatal("closing a derived registry must not release the parent's index")
	}
	if _, err := view.Search(context.Background(), "", "paging", 5); err != nil {
		t.Fatalf("the view must still be usable after Close: %v", err)
	}
}

func TestRestrict_IsOrderPreserving(t *testing.T) {
	view := threeCollections(t).Restrict(only("secrets", "runbooks"))
	if got := strings.Join(view.Names(), ","); got != "runbooks,secrets" {
		t.Errorf("Names() = %q — a view must keep configuration order, not the predicate's", got)
	}
}

func TestReady_ReportsHealthyCollections(t *testing.T) {
	reg := threeCollections(t)
	ready, health := reg.Ready()
	if !ready {
		t.Fatalf("expected ready, got %+v", health)
	}
	if len(health) != 3 {
		t.Fatalf("expected 3 health entries, got %d", len(health))
	}
	for _, h := range health {
		if !h.Ready || h.Pages != 2 || h.Error != "" {
			t.Errorf("health = %+v", h)
		}
	}
}

func TestReady_EmptyRegistryIsReady(t *testing.T) {
	view := threeCollections(t).Restrict(only())
	ready, health := view.Ready()
	if !ready || len(health) != 0 {
		t.Errorf("an empty view is trivially ready, got (%v, %+v)", ready, health)
	}
}
