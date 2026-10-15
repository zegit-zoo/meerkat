package collections

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

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

	// A restricted view shares the root's graph but resolves as it sees:
	// a link into a collection outside it reads "not mounted", and it
	// only lists backlinks it can name.
	only := reg.Restrict(func(name string) bool { return name == "hub" })
	ref, _ = only.Show("hub", "flux")
	pl = only.LinksOf(ref)
	if pl.Links[1].Resolved || pl.Links[1].Reason != `collection "flux" is not mounted` {
		t.Errorf("a link out of the view must read as not mounted: %+v", pl.Links[1])
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

// TestLinks_HiddenTargetsReadAsMissing pins meerkat-mob#40: link status
// in a view — `related:` entries, a pointer's target, and a search hit's
// target_resolved — says nothing about collections outside the view or
// pages its viewer may not see. Each probe is compared, with the probed
// name normalised, against a target that does not exist at all.
func TestLinks_HiddenTargetsReadAsMissing(t *testing.T) {
	const alice = "memory/personal/alice/"
	related := []string{
		"secret:plan", "ghost:plan", // hidden collection, real page vs. no collection
		"secret:nothing", "ghost:nothing", // hidden collection, no page vs. no collection
		"pub:" + alice + "note", "pub:" + alice + "none", // another principal's private page vs. no page
		alice + "note", alice + "none", // the same, as local links
	}
	pub := FromPages("pub", []kb.Page{
		{ID: "page", Title: "Page", Front: kb.Frontmatter{Related: related}},
		{ID: "to-secret", Title: "Zanzibar secret", Body: "zanzibar", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:secret", Hint: "h"}},
		{ID: "to-ghost", Title: "Zanzibar ghost", Body: "zanzibar", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:ghost", Hint: "h"}},
		{ID: "to-plan", Title: "Zanzibar plan", Body: "zanzibar", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "secret:plan", Hint: "h"}},
		{ID: "to-nothing", Title: "Zanzibar nothing", Body: "zanzibar", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "ghost:plan", Hint: "h"}},
		{ID: alice + "note", Title: "Alice's note"},
	})
	secret := FromPages("secret", []kb.Page{{ID: "plan", Title: "Plan", Front: kb.Frontmatter{Related: []string{"pub:page"}}}})
	reg, err := New(pub, secret)
	if err != nil {
		t.Fatal(err)
	}
	bob := reg.Restrict(func(name string) bool { return name == "pub" }).ViewedBy(kb.AsOwner("bob"))
	norm := func(l ResolvedLink, from, to string) string {
		return fmt.Sprintf("%v %s", l.Resolved, strings.ReplaceAll(l.Reason, from, to))
	}

	ref, err := bob.Show("pub", "page")
	if err != nil {
		t.Fatal(err)
	}
	pl := bob.LinksOf(ref)
	if len(pl.Links) != len(related) {
		t.Fatalf("links = %+v", pl.Links)
	}
	for i := 0; i < len(related); i += 2 {
		hidden, missing := pl.Links[i], pl.Links[i+1]
		if hidden.Resolved {
			t.Errorf("%s resolved in a view that cannot see it", related[i])
		}
		a := norm(hidden, "secret", "<c>")
		a = strings.ReplaceAll(strings.ReplaceAll(a, `"note"`, `"<p>"`), alice+"note", alice+"<p>")
		b := norm(missing, "ghost", "<c>")
		b = strings.ReplaceAll(strings.ReplaceAll(b, `"none"`, `"<p>"`), alice+"none", alice+"<p>")
		if a != b {
			t.Errorf("%s reads %q but %s reads %q", related[i], a, related[i+1], b)
		}
	}
	if len(pl.LinkedFrom) != 0 {
		t.Errorf("a backlink from a hidden collection was listed: %v", pl.LinkedFrom)
	}

	// Pointer targets: LinksOf, PointerOf and search hits agree.
	pointer := func(id string) ResolvedLink {
		ref, err := bob.Show("pub", id)
		if err != nil {
			t.Fatal(err)
		}
		pl := bob.LinksOf(ref)
		if pl.Pointer == nil {
			t.Fatalf("%s: no pointer", id)
		}
		viaOf, _ := bob.PointerOf("pub", ref.Page)
		if viaOf.Resolved != pl.Pointer.Resolved || viaOf.Reason != pl.Pointer.Reason {
			t.Errorf("%s: PointerOf %+v disagrees with LinksOf %+v", id, viaOf, pl.Pointer)
		}
		return *pl.Pointer
	}
	for _, pair := range [][2]string{{"to-secret", "to-ghost"}, {"to-plan", "to-nothing"}} {
		a, b := norm(pointer(pair[0]), "secret", "<c>"), norm(pointer(pair[1]), "ghost", "<c>")
		if a != b {
			t.Errorf("pointer %s reads %q but %s reads %q", pair[0], a, pair[1], b)
		}
	}
	hits, err := bob.Search(context.Background(), "", "zanzibar", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("hits = %+v", hits)
	}
	for _, h := range hits {
		if h.TargetResolved {
			t.Errorf("search hit %s: target_resolved = true for a target outside the view", h.Page.ID)
		}
	}
	if from := bob.PointersTo("secret"); len(from) != 0 {
		t.Errorf("PointersTo(hidden) = %v", from)
	}

	// The unrestricted, unfiltered registry still resolves everything.
	ref, _ = reg.Show("pub", "page")
	for i, l := range reg.LinksOf(ref).Links {
		if want := i%2 == 0 && i != 2; l.Resolved != want {
			t.Errorf("root view: %s resolved = %v, want %v", related[i], l.Resolved, want)
		}
	}
	if res, _ := reg.PointerOf("pub", kb.Page{ID: "to-plan", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "secret:plan", Hint: "h"}}); !res.Resolved {
		t.Errorf("root view: pointer to secret:plan = %+v", res)
	}
	// Alice sees her own note through the same links.
	aliceView := reg.Restrict(func(name string) bool { return name == "pub" }).ViewedBy(kb.AsOwner("alice"))
	ref, _ = aliceView.Show("pub", "page")
	if l := aliceView.LinksOf(ref).Links[4]; !l.Resolved {
		t.Errorf("alice's own note must resolve for her: %+v", l)
	}
}

// countingFS counts Open calls: every page a listing reads is an Open.
type countingFS struct {
	fs.FS
	opens atomic.Int64
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.opens.Add(1)
	return c.FS.Open(name)
}

// TestLinks_MemorySaveRebuildsWithoutRelistingContent pins item 1 of
// meerkat-mob#63: a memory save invalidates the link graph, but the
// rebuild it triggers re-reads no content root — content pages are
// cached per snapshot — while a snapshot swap still re-lists.
func TestLinks_MemorySaveRebuildsWithoutRelistingContent(t *testing.T) {
	cfs := &countingFS{FS: fstest.MapFS{
		"content/a.md": {Data: []byte("---\nid: a\ntitle: A\nrelated: [b]\n---\nbody\n")},
		"content/b.md": {Data: []byte("---\nid: b\ntitle: B\n---\nbody\n")},
	}}
	docs := newCollection("docs", "disk:x", "v1", cfs)
	other := FromPages("other", []kb.Page{{ID: "o", Title: "O"}})
	reg, err := New(docs, other)
	if err != nil {
		t.Fatal(err)
	}
	// Build the search index first: a memory save writes into it, and
	// building it is a listing of its own that is not the graph's.
	if _, err := docs.Index(); err != nil {
		t.Fatal(err)
	}
	before := cfs.opens.Load()
	g1 := reg.graph()
	if _, ok := g1.exists["docs:a"]; !ok {
		t.Fatalf("content not in the graph: %v", g1.exists)
	}
	listed := cfs.opens.Load()
	if listed == before {
		t.Fatal("the first build must list the content root")
	}

	for i := range 3 {
		if err := docs.publishMemory(kb.Page{ID: fmt.Sprintf("memory/team/m%d", i), Title: "M", Front: kb.Frontmatter{Related: []string{"b"}}}); err != nil {
			t.Fatal(err)
		}
		if reg.graph() == g1 {
			t.Fatal("a memory save must still invalidate the graph")
		}
	}
	if got := cfs.opens.Load(); got != listed {
		t.Errorf("memory saves re-read the content root: %d opens, want %d", got, listed)
	}
	ref, _ := reg.Show("docs", "b")
	if pl := reg.LinksOf(ref); len(pl.LinkedFrom) != 4 {
		t.Errorf("backlinks after the saves = %v, want docs:a and the three memories", pl.LinkedFrom)
	}

	// A new snapshot is new content: it is listed again.
	docs.install(&snapshot{fsys: cfs, provenance: "disk:x", version: "v2"})
	reg.graph()
	if cfs.opens.Load() == listed {
		t.Error("a snapshot swap must re-list the content root")
	}
}

// TestLinks_ContentCacheForgetsDroppedAndEvictedCollections pins the
// pruning of the per-collection content cache: a name no longer in the
// registry, and a lazy collection the resident budget returned to cold,
// keep no cached page list after the next rebuild. It also pins that a
// lazy mount and an eviction move the graph key at all.
func TestLinks_ContentCacheForgetsDroppedAndEvictedCollections(t *testing.T) {
	t.Run("dropped", func(t *testing.T) {
		reg, err := New(FromPages("docs", []kb.Page{{ID: "a", Title: "A"}}))
		if err != nil {
			t.Fatal(err)
		}
		reg.graph()
		reg.links.mu.Lock()
		reg.links.content["gone"] = contentPages{gen: 1, pages: []kb.Page{{ID: "x"}}}
		reg.links.graph = reg.buildGraph(reg.graphKey(), reg.links)
		_, kept := reg.links.content["docs"]
		_, stale := reg.links.content["gone"]
		reg.links.mu.Unlock()
		if !kept || stale {
			t.Errorf("content cache after rebuild: docs kept=%v, gone kept=%v; want true, false", kept, stale)
		}
	})
	t.Run("evicted", func(t *testing.T) {
		reg := openTree(t)
		v, _ := reg.Get("vendors")
		reg.graph() // built while vendors was cold
		if _, err := reg.ShowContext(context.Background(), "vendors", "vendors"); err != nil {
			t.Fatal(err)
		}
		// The mount moves the graph key even though a local source
		// without a refresh block carries no version token.
		if _, ok := reg.graph().exists["vendors:vendors"]; !ok {
			t.Fatal("the graph must see a lazy collection once it mounts")
		}
		cached := func() bool {
			reg.links.mu.Lock()
			defer reg.links.mu.Unlock()
			_, ok := reg.links.content["vendors"]
			return ok
		}
		if !cached() {
			t.Fatal("a mounted collection's content must be cached")
		}
		reg.unmount(context.Background(), v, "test")
		if !v.IsCold() {
			t.Fatal("unmount must leave vendors cold")
		}
		if _, ok := reg.graph().exists["vendors:vendors"]; ok {
			t.Error("the graph must forget an evicted collection's pages")
		}
		if cached() {
			t.Error("an evicted collection must not keep its cached page list")
		}
	})
}
