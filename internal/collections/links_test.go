package collections

import (
	"context"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

func linkFixture(t *testing.T) *Registry {
	t.Helper()
	hub := FromPages("hub", []kb.Page{
		{ID: "flux", Title: "Flux CD", Body: "flux helmrelease drift", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux", Hint: "GitOps, HelmRelease drift.", Related: []string{"skills/flux-diff", "flux:concepts/drift"}}},
		{ID: "datadog", Title: "Datadog", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "mcp://datadog", Hint: "Datadog's own MCP.", Tools: []string{"search_datadog_docs"}}},
		{ID: "broken", Title: "Broken", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:nope", Hint: "Nowhere."}},
		{ID: "hintless", Title: "Hintless", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux"}},
		{ID: "skills/flux-diff", Title: "Run flux diff", Front: kb.Frontmatter{Type: kb.TypeSkill, Related: []string{"flux", "missing/page", "flux:not-there", "bad link"}}},
		{ID: "memory/personal/alice/note", Title: "Alice's note", Front: kb.Frontmatter{Related: []string{"flux"}}},
	})
	flux := FromPages("flux", []kb.Page{
		{ID: "concepts/drift", Title: "Drift", Body: "flux helmrelease drift explained", Front: kb.Frontmatter{Related: []string{"hub:flux", "ext:mcp:pagerduty"}}},
	})
	reg, err := New(hub, flux)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestLinks_ResolveAcrossCollections(t *testing.T) {
	reg := linkFixture(t)
	ref, err := reg.Show("hub", "flux")
	if err != nil {
		t.Fatal(err)
	}
	pl := reg.LinksOf(ref)
	if len(pl.Links) != 2 || !pl.Links[0].Resolved || !pl.Links[1].Resolved {
		t.Errorf("links = %+v", pl.Links)
	}
	if pl.Pointer == nil || !pl.Pointer.Resolved || pl.Pointer.Kind != kb.LinkCollection || pl.Pointer.Collection != "flux" {
		t.Errorf("pointer = %+v", pl.Pointer)
	}
	// Backlinks: the skill, the drift page (qualified) and — unfiltered — Alice's private note.
	want := []string{"flux:concepts/drift", "hub:memory/personal/alice/note", "hub:skills/flux-diff"}
	if strings.Join(pl.LinkedFrom, ",") != strings.Join(want, ",") {
		t.Errorf("linked_from = %v, want %v", pl.LinkedFrom, want)
	}
}

func TestLinks_DanglingAndInvalidAreReportedNotFatal(t *testing.T) {
	reg := linkFixture(t)
	ref, _ := reg.Show("hub", "skills/flux-diff")
	pl := reg.LinksOf(ref)
	if len(pl.Links) != 3 || !pl.Links[0].Resolved || pl.Links[1].Resolved || pl.Links[2].Resolved {
		t.Errorf("links = %+v", pl.Links)
	}
	if !strings.Contains(pl.Links[1].Reason, "not found") || !strings.Contains(pl.Links[2].Reason, "not found in collection \"flux\"") {
		t.Errorf("reasons = %q / %q", pl.Links[1].Reason, pl.Links[2].Reason)
	}
	if len(pl.Invalid) != 1 || !strings.Contains(pl.Invalid[0], "whitespace") {
		t.Errorf("invalid = %v", pl.Invalid)
	}

	report := reg.LinkReport()
	if report.OK() || report.Pages != 7 {
		t.Errorf("report = %+v", report)
	}
	var got []string
	for _, d := range report.Dangling {
		got = append(got, d.String())
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		`hub:broken: pointer target "collection:nope": collection "nope" is not mounted`,
		`hub:hintless: pointer target "collection:flux": pointer "hintless" has no hint`,
		`hub:skills/flux-diff: related "bad link"`,
		`hub:skills/flux-diff: related "flux:not-there"`,
		`hub:skills/flux-diff: related "missing/page"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("report lacks %q:\n%s", want, joined)
		}
	}
	if len(report.Dangling) != 5 {
		t.Errorf("dangling = %d, want 5:\n%s", len(report.Dangling), joined)
	}

	// Health carries the count and the first few, and stays ready.
	ready, health := reg.Ready()
	if !ready {
		t.Error("dangling links must not make the registry unready")
	}
	for _, h := range health {
		switch h.Name {
		case "hub":
			if h.DanglingLinks != 5 || len(h.Warnings) != 5 || !h.Ready {
				t.Errorf("hub health = %+v", h)
			}
		case "flux":
			if h.DanglingLinks != 0 || h.Warnings != nil {
				t.Errorf("flux health = %+v", h)
			}
		}
	}
	if n, w := linkWarnings(make([]Dangling, maxLinkWarnings+3)); n != maxLinkWarnings+3 || len(w) != maxLinkWarnings+1 || !strings.Contains(w[maxLinkWarnings], "3 more") {
		t.Errorf("linkWarnings truncation = %d %v", n, w)
	}

	broken, _ := reg.Show("hub", "broken")
	if pl := reg.LinksOf(broken); pl.Pointer == nil || pl.Pointer.Resolved || pl.PointerError != "" {
		t.Errorf("broken pointer = %+v", pl)
	}
	hintless, _ := reg.Show("hub", "hintless")
	if pl := reg.LinksOf(hintless); pl.Pointer != nil || !strings.Contains(pl.PointerError, "no hint") {
		t.Errorf("hintless pointer = %+v", pl)
	}
}

func TestLinks_BacklinksRespectTheViewer(t *testing.T) {
	reg := linkFixture(t)
	bob := reg.ViewedBy(kb.AsOwner("bob"))
	ref, err := bob.Show("hub", "flux")
	if err != nil {
		t.Fatal(err)
	}
	pl := bob.LinksOf(ref)
	for _, src := range pl.LinkedFrom {
		if strings.Contains(src, "alice") {
			t.Errorf("Alice's private note leaked into Bob's linked_from: %v", pl.LinkedFrom)
		}
	}
	alice := reg.ViewedBy(kb.AsOwner("alice"))
	ref, _ = alice.Show("hub", "flux")
	if pl := alice.LinksOf(ref); !strings.Contains(strings.Join(pl.LinkedFrom, ","), "alice") {
		t.Errorf("Alice must see her own note among the backlinks: %v", pl.LinkedFrom)
	}

	// A restricted view still resolves against the full mounted set
	// (the graph is the root's) but only lists backlinks it can name.
	only := reg.Restrict(func(name string) bool { return name == "hub" })
	ref, _ = only.Show("hub", "flux")
	pl = only.LinksOf(ref)
	if !pl.Links[1].Resolved {
		t.Errorf("qualified link must still resolve in a restricted view: %+v", pl.Links[1])
	}
	for _, src := range pl.LinkedFrom {
		if strings.HasPrefix(src, "flux:") {
			t.Errorf("a backlink from outside the view was listed: %v", pl.LinkedFrom)
		}
	}
}

func TestLinks_GraphIsCachedUntilTheSetMoves(t *testing.T) {
	reg := linkFixture(t)
	g1 := reg.graph()
	if reg.graph() != g1 || reg.ViewedBy(kb.AsOwner("x")).graph() != g1 {
		t.Error("the graph must be reused while nothing moved")
	}
	// A memory publish bumps the overlay generation and invalidates.
	hub, _ := reg.Get("hub")
	if err := hub.publishMemory(kb.Page{ID: "memory/team/new", Title: "New", Front: kb.Frontmatter{Related: []string{"flux"}}}); err != nil {
		t.Fatal(err)
	}
	g2 := reg.graph()
	if g2 == g1 {
		t.Fatal("a memory publish must invalidate the graph")
	}
	ref, _ := reg.Show("hub", "flux")
	if pl := reg.LinksOf(ref); !strings.Contains(strings.Join(pl.LinkedFrom, ","), "memory/team/new") {
		t.Errorf("the new memory page's link is missing from backlinks: %v", pl.LinkedFrom)
	}
}

func TestPointerOf(t *testing.T) {
	reg := linkFixture(t)
	hub, _ := reg.Get("hub")
	pages, _ := hub.Pages()
	var seen int
	for _, p := range pages {
		res, ok := reg.PointerOf("hub", p)
		switch p.ID {
		case "flux", "datadog", "broken":
			if !ok {
				t.Errorf("%s: PointerOf = false", p.ID)
			}
			seen++
			if p.ID == "broken" && res.Resolved {
				t.Error("broken must be unresolved")
			}
		default:
			if ok {
				t.Errorf("%s: PointerOf = true", p.ID)
			}
		}
	}
	if seen != 3 {
		t.Errorf("seen %d pointers", seen)
	}
}

func TestBundles_GroupByPointerTarget(t *testing.T) {
	reg := linkFixture(t)
	hits, err := reg.Search(context.Background(), "", "flux", 10)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, h := range hits {
		kinds = append(kinds, h.Collection+":"+h.Page.ID+"="+h.Kind)
		if h.Page.ID == "flux" && h.Collection == "hub" && (h.Target != "collection:flux" || !h.TargetResolved || h.Hint == "") {
			t.Errorf("pointer hit lacks routing: %+v", h)
		}
	}
	if hits[0].Kind != KindPointer {
		t.Errorf("a pointer must be the top hit on a hub: %v", kinds)
	}
	bundles := reg.Bundles(hits)
	if len(bundles) == 0 || bundles[0].Target != "collection:flux" || bundles[0].Hint == "" {
		t.Fatalf("bundles = %+v", bundles)
	}
	b := bundles[0]
	if len(b.Pointers) != 1 || len(b.Skills) != 1 || b.Skills[0].Page.ID != "skills/flux-diff" {
		t.Errorf("flux bundle = pointers %d skills %d docs %d", len(b.Pointers), len(b.Skills), len(b.Docs))
	}
	// The drift page is linked from the pointer (qualified) and lands in the same bundle.
	var docs []string
	for _, d := range b.Docs {
		docs = append(docs, d.Collection+":"+d.Page.ID)
	}
	if !strings.Contains(strings.Join(docs, ","), "flux:concepts/drift") {
		t.Errorf("docs = %v, want the qualified related page", docs)
	}
	// Nothing is dropped: every hit is in exactly one bundle.
	var total int
	for _, b := range bundles {
		total += len(b.Pointers) + len(b.Skills) + len(b.Examples) + len(b.Docs)
	}
	if total != len(hits) {
		t.Errorf("bundled %d of %d hits", total, len(hits))
	}
	if reg.Bundles(nil) != nil {
		t.Error("no hits, no bundles")
	}
}
