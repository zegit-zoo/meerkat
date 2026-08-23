package collections

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// page builds a fixture page.
func page(id, title, body string) kb.Page {
	return kb.Page{ID: id, Title: title, Body: body, Front: kb.Frontmatter{ID: id, Title: title}}
}

// twoCollections mounts "runbooks" and "architecture", with one page ID
// ("shared/overview") deliberately present in both so the ambiguity
// rules have something to bite on.
func twoCollections(t *testing.T) *Registry {
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
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func TestNew_RejectsEmptyAndDuplicates(t *testing.T) {
	if _, err := New(); err == nil {
		t.Error("expected an error for an empty registry")
	}
	if _, err := New(FromPages("a", nil), FromPages("a", nil)); err == nil {
		t.Error("expected an error for duplicate collection names")
	}
	if _, err := New(FromPages("", nil)); err == nil {
		t.Error("expected an error for an empty collection name")
	}
}

func TestRegistry_NamesAndOrder(t *testing.T) {
	reg := twoCollections(t)
	if got := strings.Join(reg.Names(), ","); got != "runbooks,architecture" {
		t.Errorf("Names() = %q, want configuration order runbooks,architecture", got)
	}
	if reg.Single() {
		t.Error("Single() = true for a two-collection registry")
	}
	if reg.Len() != 2 {
		t.Errorf("Len() = %d, want 2", reg.Len())
	}
}

func TestRegistry_Get_UnknownNamesTheAvailableOnes(t *testing.T) {
	reg := twoCollections(t)
	_, err := reg.Get("nope")
	if !errors.Is(err, ErrUnknownCollection) {
		t.Fatalf("err = %v, want ErrUnknownCollection", err)
	}
	for _, name := range []string{"runbooks", "architecture"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %v should list the available collection %q", err, name)
		}
	}
}

func TestRegistry_Pages_AllVsOne(t *testing.T) {
	reg := twoCollections(t)

	all, err := reg.Pages("")
	if err != nil {
		t.Fatalf("Pages(all): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("Pages(all) returned %d refs, want 4", len(all))
	}
	// Configuration order across collections, ID order within one.
	want := []string{"runbooks:incidents/paging", "runbooks:shared/overview",
		"architecture:adr/0001-storage", "architecture:shared/overview"}
	var got []string
	for _, r := range all {
		got = append(got, r.QualifiedID())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Pages(all) = %v, want %v", got, want)
	}

	one, err := reg.Pages("architecture")
	if err != nil {
		t.Fatalf("Pages(architecture): %v", err)
	}
	if len(one) != 2 {
		t.Fatalf("Pages(architecture) returned %d refs, want 2", len(one))
	}
	for _, r := range one {
		if r.Collection != "architecture" {
			t.Errorf("--collection architecture leaked a %q page", r.Collection)
		}
	}

	if _, err := reg.Pages("nope"); !errors.Is(err, ErrUnknownCollection) {
		t.Errorf("Pages(nope) = %v, want ErrUnknownCollection", err)
	}
}

func TestRegistry_Search_SpansAllAndNarrows(t *testing.T) {
	reg := twoCollections(t)
	ctx := context.Background()

	all, err := reg.Search(ctx, "", "overview", 10)
	if err != nil {
		t.Fatalf("Search(all): %v", err)
	}
	seen := map[string]bool{}
	for _, h := range all {
		seen[h.Collection] = true
	}
	if !seen["runbooks"] || !seen["architecture"] {
		t.Errorf("cross-collection search only reached %v", seen)
	}

	narrowed, err := reg.Search(ctx, "runbooks", "overview", 10)
	if err != nil {
		t.Fatalf("Search(runbooks): %v", err)
	}
	if len(narrowed) == 0 {
		t.Fatal("expected at least one hit in runbooks")
	}
	for _, h := range narrowed {
		if h.Collection != "runbooks" {
			t.Errorf("--collection runbooks leaked a %q hit", h.Collection)
		}
	}

	if _, err := reg.Search(ctx, "nope", "overview", 10); !errors.Is(err, ErrUnknownCollection) {
		t.Errorf("Search(nope) = %v, want ErrUnknownCollection", err)
	}
}

// TestRegistry_Search_LimitAppliesToTheMergedResult: the limit is the
// caller's budget for the whole answer, not per collection.
func TestRegistry_Search_LimitAppliesToTheMergedResult(t *testing.T) {
	reg := twoCollections(t)
	hits, err := reg.Search(context.Background(), "", "overview", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("got %d hits for limit=1 across two collections, want 1", len(hits))
	}
}

func TestRegistry_Show_SingleMatch(t *testing.T) {
	reg := twoCollections(t)
	ref, err := reg.Show("", "incidents/paging")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if ref.Collection != "runbooks" {
		t.Errorf("collection = %q, want runbooks", ref.Collection)
	}
	// The page's own ID is never rewritten.
	if ref.Page.ID != "incidents/paging" {
		t.Errorf("page id = %q, want the unqualified id", ref.Page.ID)
	}
	if ref.QualifiedID() != "runbooks:incidents/paging" {
		t.Errorf("QualifiedID = %q", ref.QualifiedID())
	}
}

// TestRegistry_Show_AmbiguousIsAnErrorNotAPick is the core routing
// decision: a bare ID present in several collections must never resolve
// silently to whichever came first.
func TestRegistry_Show_AmbiguousIsAnErrorNotAPick(t *testing.T) {
	reg := twoCollections(t)
	_, err := reg.Show("", "shared/overview")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
	for _, want := range []string{"runbooks:shared/overview", "architecture:shared/overview"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v should offer the qualified id %q", err, want)
		}
	}
}

func TestRegistry_Show_QualifiedAndFlagResolveTheAmbiguity(t *testing.T) {
	reg := twoCollections(t)

	byQualified, err := reg.Show("", "architecture:shared/overview")
	if err != nil {
		t.Fatalf("Show(qualified): %v", err)
	}
	if byQualified.Page.Title != "Architecture Overview" {
		t.Errorf("qualified id resolved to %q", byQualified.Page.Title)
	}

	byFlag, err := reg.Show("runbooks", "shared/overview")
	if err != nil {
		t.Fatalf("Show(--collection): %v", err)
	}
	if byFlag.Page.Title != "Runbook Overview" {
		t.Errorf("--collection resolved to %q", byFlag.Page.Title)
	}

	// Both, agreeing, is fine.
	if _, err := reg.Show("runbooks", "runbooks:shared/overview"); err != nil {
		t.Errorf("agreeing collection + qualified id: %v", err)
	}
	// Both, disagreeing, is an error rather than a precedence rule.
	if _, err := reg.Show("runbooks", "architecture:shared/overview"); err == nil {
		t.Error("expected an error when --collection and the qualified id disagree")
	}
}

func TestRegistry_Show_NotFound(t *testing.T) {
	reg := twoCollections(t)
	if _, err := reg.Show("", "does/not/exist"); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("err = %v, want kb.ErrNotFound", err)
	}
	if _, err := reg.Show("runbooks", "adr/0001-storage"); !errors.Is(err, kb.ErrNotFound) {
		t.Errorf("a page from another collection should be ErrNotFound under --collection, got %v", err)
	}
	if _, err := reg.Show("nope", "x"); !errors.Is(err, ErrUnknownCollection) {
		t.Errorf("err = %v, want ErrUnknownCollection", err)
	}
}

// TestRegistry_SplitQualified_OnlyRecognisesMountedNames: a page ID that
// happens to contain a colon must not be mistaken for a qualification.
func TestRegistry_SplitQualified_OnlyRecognisesMountedNames(t *testing.T) {
	reg := twoCollections(t)
	cases := []struct{ in, wantColl, wantID string }{
		{"runbooks:incidents/paging", "runbooks", "incidents/paging"},
		{"incidents/paging", "", "incidents/paging"},
		{"notacollection:page", "", "notacollection:page"},
		{"runbooks:", "", "runbooks:"},
	}
	for _, tc := range cases {
		coll, id := reg.SplitQualified(tc.in)
		if coll != tc.wantColl || id != tc.wantID {
			t.Errorf("SplitQualified(%q) = (%q,%q), want (%q,%q)", tc.in, coll, id, tc.wantColl, tc.wantID)
		}
	}
}

// --- single-collection (back-compat) behaviour ---

func TestRegistry_SingleCollection_BehavesAsBefore(t *testing.T) {
	reg, err := New(FromPages(DefaultName, []kb.Page{page("a/b", "AB", "body")}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	if !reg.Single() {
		t.Error("Single() = false for a one-collection registry")
	}
	ref, err := reg.Show("", "a/b")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if ref.Page.ID != "a/b" {
		t.Errorf("id = %q", ref.Page.ID)
	}
	if got := reg.Provenance(); got != "memory" {
		t.Errorf("Provenance() = %q, want the single collection's own provenance", got)
	}
}

func TestRegistry_Provenance_MultiReportsCount(t *testing.T) {
	if got := twoCollections(t).Provenance(); got != "collections:2" {
		t.Errorf("Provenance() = %q, want collections:2", got)
	}
}

// --- Open ---

// TestOpen_SingleCollectionReadsThroughTheGlobals: with one collection,
// Open must NOT open its own filesystem — the caller has already pointed
// the process globals at that content root, and everything that predates
// collections reads through them.
func TestOpen_SingleCollectionReadsThroughTheGlobals(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/only.md": {Data: []byte("---\nid: only\ntitle: Only\n---\nglobal body\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })

	reg, err := Open([]contentsource.ResolvedCollection{
		{Name: DefaultName, Dir: t.TempDir(), Provenance: "disk:x"},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	refs, err := reg.Pages("")
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	if len(refs) != 1 || refs[0].Page.ID != "only" {
		t.Errorf("single collection did not read through the process globals: %+v", refs)
	}
}

// TestOpen_MultiCollectionMountsEachDirectory: with several, each gets
// its own filesystem over its own directory, honouring its own layout.
func TestOpen_MultiCollectionMountsEachDirectory(t *testing.T) {
	dirA := t.TempDir()
	writeTree(t, dirA, map[string]string{"wiki/a.md": "---\nid: a\ntitle: A\n---\nbody a\n"})
	dirB := t.TempDir()
	writeTree(t, dirB, map[string]string{"docs/b.md": "---\nid: b\ntitle: B\n---\nbody b\n"})

	reg, err := Open([]contentsource.ResolvedCollection{
		{Name: "a", Dir: dirA, Source: contentsource.Source{Type: "local", Layout: contentsource.MergeLayout(contentsource.Layout{})}},
		// A per-collection layout override must be honoured.
		{Name: "b", Dir: dirB, Source: contentsource.Source{Type: "local", Layout: contentsource.MergeLayout(contentsource.Layout{Wiki: "docs"})}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	refs, err := reg.Pages("")
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	var ids []string
	for _, r := range refs {
		ids = append(ids, r.QualifiedID())
	}
	if strings.Join(ids, ",") != "a:a,b:b" {
		t.Errorf("Pages = %v, want [a:a b:b]", ids)
	}
}

// TestOpen_EmbeddedCollectionAmongOthersIsRejectedGracefully: a resolved
// collection with no directory (the embedded fallback) alongside others
// reads through the globals; it must at least not panic or error.
func TestOpen_MultiWithEmbeddedEntry(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"wiki/a.md": "---\nid: a\ntitle: A\n---\nbody\n"})
	reg, err := Open([]contentsource.ResolvedCollection{
		{Name: "disk", Dir: dir, Source: contentsource.Source{Type: "local", Layout: contentsource.MergeLayout(contentsource.Layout{})}},
		{Name: "embedded", Provenance: contentsource.SourceEmbedded},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if _, err := reg.Pages(""); err != nil {
		t.Errorf("Pages: %v", err)
	}
}

func TestGlobal_IsOneDefaultCollection(t *testing.T) {
	reg := Global("embedded")
	if !reg.Single() || reg.Names()[0] != DefaultName {
		t.Errorf("Global() = %v, want one collection named %q", reg.Names(), DefaultName)
	}
	if reg.Provenance() != "embedded" {
		t.Errorf("Provenance() = %q", reg.Provenance())
	}
}
