package kb

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestOKFBundle_* exercise a small, hand-written OKF (Open Knowledge
// Format) v0.2 conformance fixture under testdata/okf-bundle — see
// https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md.
//
// The fixture is hand-written rather than one of Google's published
// sample bundles to avoid vendoring third-party content (and the
// attribution obligations and repo bloat that would come with it). It
// deliberately packs in one of everything item 6 of the OKF work asks
// for: a concept with the full frontmatter set (tables/orders.md), one
// with only the required `type` (tables/customers.md), a bundle-root
// index.md/log.md with no frontmatter, a nested directory with its own
// frontmatter-less index.md (tables/index.md), a frontmatter-bearing
// index.md that must still be indexed (reviews/index.md), a bare
// `verified` mapping (reviews/revenue.md), a human: verifier alongside
// a non-human one (tables/orders.md), and a broken cross-link
// (tables/orders.md's body links to a non-existent supplier table).
//
// withOKFBundle points kb.List/Load at the fixture for the duration of
// a test, via the same withFS helper security_test.go already uses.
func withOKFBundle(t *testing.T) {
	t.Helper()
	withFS(t, os.DirFS("testdata/okf-bundle"))
}

// TestOKFBundle_ReservedFilenamesWithoutFrontmatterAreSkipped covers
// gap #1 from the OKF issue: index.md/log.md (at the bundle root AND
// nested) must not show up as pages, either via List or a direct Load,
// because they carry no frontmatter (SPEC.md §3.1, §8, §9).
func TestOKFBundle_ReservedFilenamesWithoutFrontmatterAreSkipped(t *testing.T) {
	withOKFBundle(t)

	pages, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range pages {
		if p.ID == "index" || p.ID == "log" || p.ID == "tables/index" {
			t.Errorf("reserved OKF artifact %q should have been skipped by List(), got it back", p.ID)
		}
	}

	for _, id := range []string{"index", "log", "tables/index"} {
		if _, err := Load(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Load(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

// TestOKFBundle_FrontmatterBearingIndexIsStillIndexed covers the other
// half of gap #1: an index.md that carries real meerkat frontmatter
// (id:/title:) is a legitimate landing page, not an OKF artifact, and
// must still load and appear in List().
func TestOKFBundle_FrontmatterBearingIndexIsStillIndexed(t *testing.T) {
	withOKFBundle(t)

	page, err := Load("reviews/index")
	if err != nil {
		t.Fatalf("Load(reviews/index): %v", err)
	}
	if page.Front.Title != "Reviews landing page" {
		t.Errorf("Title = %q, want %q", page.Front.Title, "Reviews landing page")
	}
	if page.Front.Category != "reviews" {
		t.Errorf("Category = %q", page.Front.Category)
	}

	pages, err := List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range pages {
		if p.ID == "reviews/index" {
			found = true
		}
	}
	if !found {
		t.Error("reviews/index (has frontmatter) should be indexed, not skipped")
	}
}

// TestOKFBundle_NestedDirectoryIsIndexed proves every real concept in
// the bundle — across a nested directory — makes it into List(),
// alongside the reserved-artifact exclusions above.
func TestOKFBundle_NestedDirectoryIsIndexed(t *testing.T) {
	withOKFBundle(t)

	pages, err := List()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"tables/orders":    false,
		"tables/customers": false,
		"reviews/index":    false,
		"reviews/revenue":  false,
	}
	for _, p := range pages {
		if _, ok := want[p.ID]; ok {
			want[p.ID] = true
		}
	}
	for id, ok := range want {
		if !ok {
			t.Errorf("expected %q in List(), got pages: %+v", id, ids(pages))
		}
	}
	// Exactly the 4 real concepts above — the 3 reserved artifacts
	// (index, log, tables/index) must not inflate the count.
	if len(pages) != len(want) {
		t.Errorf("List() = %d pages %v, want exactly %d", len(pages), ids(pages), len(want))
	}
}

func ids(pages []Page) []string {
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = p.ID
	}
	return out
}

// TestOKFBundle_RequiredOnlyConceptLoads covers a concept carrying just
// OKF's one required field (SPEC.md §4.1, §11).
func TestOKFBundle_RequiredOnlyConceptLoads(t *testing.T) {
	withOKFBundle(t)

	page, err := Load("tables/customers")
	if err != nil {
		t.Fatalf("Load(tables/customers): %v", err)
	}
	if page.Front.Type != "BigQuery Table" {
		t.Errorf("Type = %q, want %q", page.Front.Type, "BigQuery Table")
	}
	if page.Front.TrustTier() != TrustUnverified {
		t.Errorf("TrustTier() = %q, want %q for a concept with no verified key", page.Front.TrustTier(), TrustUnverified)
	}
}

// TestOKFBundle_UnknownKeysSurviveInExtra proves resource/sources (real
// OKF fields meerkat deliberately does not promote to the core) still
// round-trip through Frontmatter.Extra.
func TestOKFBundle_UnknownKeysSurviveInExtra(t *testing.T) {
	withOKFBundle(t)

	page, err := Load("tables/orders")
	if err != nil {
		t.Fatalf("Load(tables/orders): %v", err)
	}
	if _, ok := page.Front.Extra["resource"]; !ok {
		t.Error("resource should survive in Extra (not promoted to the core)")
	}
	if _, ok := page.Front.Extra["sources"]; !ok {
		t.Error("sources should survive in Extra (not promoted to the core)")
	}
}

// TestOKFBundle_TrustTiers exercises all three OKF trust tiers
// (SPEC.md §5.3) across real, loaded pages: no verified key at all, a
// bare (non-list) mapping by a non-human actor, and a list containing
// both a human: and a non-human verifier.
func TestOKFBundle_TrustTiers(t *testing.T) {
	withOKFBundle(t)

	cases := []struct {
		id   string
		want string
	}{
		{"tables/customers", TrustUnverified},      // no verified key
		{"reviews/revenue", TrustMachineConfirmed}, // bare mapping, process: actor
		{"tables/orders", TrustHumanReviewed},      // list incl. a human: actor
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			page, err := Load(tc.id)
			if err != nil {
				t.Fatalf("Load(%q): %v", tc.id, err)
			}
			if got := page.Front.TrustTier(); got != tc.want {
				t.Errorf("TrustTier() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOKFBundle_BareVerifiedMappingParsesAsOneElementList is the direct
// conformance check behind reviews/revenue.md's trust tier above: OKF
// SPEC.md §5.2/§11 requires a bare `{by, at}` mapping to parse as a
// one-element list, not a parse failure and not a many-element list.
func TestOKFBundle_BareVerifiedMappingParsesAsOneElementList(t *testing.T) {
	withOKFBundle(t)

	page, err := Load("reviews/revenue")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Front.Verified) != 1 {
		t.Fatalf("Verified = %+v, want exactly one element", page.Front.Verified)
	}
	if page.Front.Verified[0].By != "process:finance-nightly" {
		t.Errorf("Verified[0].By = %q", page.Front.Verified[0].By)
	}
}

// TestOKFBundle_HumanAndNonHumanVerifiersInOneList proves tables/orders.md's
// two-entry verified list decodes both entries (order preserved), one
// human: and one process: actor.
func TestOKFBundle_HumanAndNonHumanVerifiersInOneList(t *testing.T) {
	withOKFBundle(t)

	page, err := Load("tables/orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Front.Verified) != 2 {
		t.Fatalf("Verified = %+v, want 2 entries", page.Front.Verified)
	}
	if page.Front.Verified[0].By != "human:ahormati" {
		t.Errorf("Verified[0].By = %q", page.Front.Verified[0].By)
	}
	if page.Front.Verified[1].By != "process:finance-nightly" {
		t.Errorf("Verified[1].By = %q", page.Front.Verified[1].By)
	}
}

// TestOKFBundle_BrokenCrossLinkDoesNotCauseRejection covers OKF's
// "consumers MUST tolerate broken links" rule (SPEC.md §6.1, §11).
// meerkat does no link resolution at all today, so this test's real
// job is to prove the concept carrying a broken link still loads
// cleanly and stays in the KB — a future change that adds link
// checking must not start rejecting pages over it.
func TestOKFBundle_BrokenCrossLinkDoesNotCauseRejection(t *testing.T) {
	withOKFBundle(t)

	page, err := Load("tables/orders")
	if err != nil {
		t.Fatalf("Load(tables/orders) with a broken cross-link: %v", err)
	}
	if !strings.Contains(page.Body, "/tables/suppliers.md") {
		t.Error("expected the broken-link concept body to be preserved verbatim")
	}

	pages, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		if p.ID == "tables/orders" {
			return
		}
	}
	t.Error("tables/orders should still be present in List() despite its broken cross-link")
}

// TestOKFBundle_ByType proves the --type / mk_list filter facet works
// against a real bundle: two BigQuery Table concepts, one Attested
// Computation.
func TestOKFBundle_ByType(t *testing.T) {
	withOKFBundle(t)

	pages, err := List()
	if err != nil {
		t.Fatal(err)
	}
	tables := Filter(pages, ByType("BigQuery Table"))
	if len(tables) != 2 {
		t.Errorf("ByType(BigQuery Table) = %d pages %v, want 2", len(tables), ids(tables))
	}
	computations := Filter(pages, ByType("Attested Computation"))
	if len(computations) != 1 {
		t.Errorf("ByType(Attested Computation) = %d pages %v, want 1", len(computations), ids(computations))
	}
}

// TestOKFBundle_StatusVocabulariesCoexist proves an OKF status value
// ("stable", not one of meerkat's own placeholder/reviewed/...
// literals) round-trips and filters through ByStatus unchanged — the
// "coexist, don't map" decision (see Frontmatter.Status's doc comment).
func TestOKFBundle_StatusVocabulariesCoexist(t *testing.T) {
	withOKFBundle(t)

	page, err := Load("tables/orders")
	if err != nil {
		t.Fatal(err)
	}
	if page.Front.Status != "stable" {
		t.Errorf("Status = %q, want the untranslated OKF value %q", page.Front.Status, "stable")
	}

	pages, err := List()
	if err != nil {
		t.Fatal(err)
	}
	stable := Filter(pages, ByStatus("stable"))
	if len(stable) != 2 { // tables/orders and reviews/revenue
		t.Errorf("ByStatus(stable) = %d pages %v, want 2", len(stable), ids(stable))
	}
}
