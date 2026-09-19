package collections

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// links.go resolves `related:` links and pointer targets across the
// mounted set, and answers the two questions a page view asks: where
// does this page point (resolved or dangling?) and who points here?
//
// kb.ParseLink turns strings into typed links without knowing which
// collections exist; this file is where existence is decided, because
// only the registry knows the mounted set. Resolution is computed over
// EVERY page the registry holds (content and memory overlay alike),
// cached, and re-derived only when a collection's snapshot version or
// overlay generation moves — so a `mk_show` costs a map lookup, not a
// walk, and a reload or a memory save invalidates exactly once.
//
// Visibility: the graph is built unfiltered and filtered at read time.
// Backlinks shown to a viewer include only source pages that viewer
// can see, so a private memory page that links to a public page never
// reveals itself through the public page's `linked_from`. Resolution
// status is viewer-independent: whether `flux:drift` exists does not
// depend on who asks, and the ID was already in the source page's own
// frontmatter.

// ResolvedLink is a parsed link plus what the registry knows about it.
type ResolvedLink struct {
	kb.Link
	// Resolved is true when the target exists in the mounted set, and
	// always true for an external link (nothing to check).
	Resolved bool `json:"resolved"`
	// Reason says why Resolved is false ("collection \"x\" is not
	// mounted", "page not found"). Empty when resolved.
	Reason string `json:"reason,omitempty"`
}

// PageLinks is what a page view carries about links.
type PageLinks struct {
	// Links are this page's `related:` entries, resolved. Entries that
	// did not parse are listed in Invalid.
	Links []ResolvedLink `json:"links,omitempty"`
	// Invalid holds the `related:` entries that did not parse, with
	// the reason.
	Invalid []string `json:"invalid_links,omitempty"`
	// LinkedFrom lists the qualified IDs (`<collection>:<id>`) of pages
	// whose `related:` names this page, filtered by the viewer.
	LinkedFrom []string `json:"linked_from,omitempty"`
	// Pointer is set for a `type: pointer` page: its target, resolved.
	Pointer *ResolvedLink `json:"pointer,omitempty"`
	// PointerError says why a `type: pointer` page is not a valid
	// pointer (no target, no hint, local target). Empty otherwise.
	PointerError string `json:"pointer_error,omitempty"`
}

// Dangling is one unresolved link or invalid pointer, for
// Registry.LinkReport and `mk lint`.
type Dangling struct {
	Collection string `json:"collection"`
	PageID     string `json:"page"`
	// Link is the `related:` entry or pointer `target:` as written.
	Link   string `json:"link"`
	Reason string `json:"reason"`
	// Pointer is true when the problem is the page's pointer target
	// rather than a `related:` entry.
	Pointer bool `json:"pointer,omitempty"`
}

func (d Dangling) String() string {
	what := "related"
	if d.Pointer {
		what = "pointer target"
	}
	return fmt.Sprintf("%s:%s: %s %q: %s", d.Collection, d.PageID, what, d.Link, d.Reason)
}

// LinkReport is the registry-wide view of link health.
type LinkReport struct {
	// Pages counts pages examined.
	Pages int `json:"pages"`
	// Links counts parsed `related:` entries and pointer targets.
	Links int `json:"links"`
	// Dangling lists every unresolved or invalid entry, sorted by
	// collection, page, link.
	Dangling []Dangling `json:"dangling,omitempty"`
}

// OK reports whether nothing dangles.
func (r LinkReport) OK() bool { return len(r.Dangling) == 0 }

// linkGraph is the resolved state for one configuration of the mounted
// set, identified by key.
type linkGraph struct {
	key string
	// mounted is the set of collection names the graph was built over.
	mounted map[string]bool
	// exists answers "is there a page <collection>:<id>?" for every
	// page, unfiltered.
	exists map[string]kb.Page
	// backlinks maps a target's qualified ID to the qualified IDs of
	// pages whose `related:` names it (sorted).
	backlinks map[string][]string
	// pointers maps a pointer page's qualified ID to its resolved
	// target; pointerErr to why it is not a valid pointer.
	pointers   map[string]ResolvedLink
	pointerErr map[string]string
	report     LinkReport
}

// linkCache is shared by a root registry and every view derived from
// it (Restrict, ViewedBy copy the pointer), so a view never rebuilds
// what the root already has.
type linkCache struct {
	mu    sync.Mutex
	graph *linkGraph
}

// base returns the registry the graph is built from: the root, or r
// itself when r is one.
func (r *Registry) base() *Registry {
	if r.root != nil {
		return r.root
	}
	return r
}

// graphKey fingerprints the mounted set: every collection's name,
// snapshot version and overlay generation. Any reload, memory save or
// memory reconciliation moves it.
func (r *Registry) graphKey() string {
	var b strings.Builder
	for _, c := range r.base().list {
		fmt.Fprintf(&b, "%s\x00%s\x00%d\n", c.Name, c.currentVersion(), c.overlayGen.Load())
	}
	return b.String()
}

// graph returns the current link graph, building it if the mounted set
// changed since the last build.
func (r *Registry) graph() *linkGraph {
	root := r.base()
	if root.links == nil {
		// A registry assembled without New/Global (tests); no sharing,
		// no caching.
		return root.buildGraph(root.graphKey())
	}
	key := root.graphKey()
	root.links.mu.Lock()
	defer root.links.mu.Unlock()
	if root.links.graph != nil && root.links.graph.key == key {
		return root.links.graph
	}
	root.links.graph = root.buildGraph(key)
	return root.links.graph
}

func qualify(collection, id string) string { return collection + ":" + id }

// buildGraph walks every page of every collection once.
func (r *Registry) buildGraph(key string) *linkGraph {
	g := &linkGraph{
		key:        key,
		mounted:    make(map[string]bool, len(r.list)),
		exists:     make(map[string]kb.Page),
		backlinks:  make(map[string][]string),
		pointers:   make(map[string]ResolvedLink),
		pointerErr: make(map[string]string),
	}
	for _, c := range r.list {
		g.mounted[c.Name] = true
	}
	type located struct {
		collection string
		page       kb.Page
	}
	var all []located
	for _, c := range r.list {
		pages, err := c.PagesFor(kb.Unfiltered())
		if err != nil {
			// A collection that cannot list is reported by health(); the
			// graph simply does not know its pages, and links into it
			// resolve as dangling with that reason.
			continue
		}
		for _, p := range pages {
			g.exists[qualify(c.Name, p.ID)] = p
			all = append(all, located{c.Name, p})
		}
	}
	g.report.Pages = len(all)

	for _, l := range all {
		from := qualify(l.collection, l.page.ID)
		links, errs := l.page.Links()
		for _, err := range errs {
			g.report.Dangling = append(g.report.Dangling, Dangling{Collection: l.collection, PageID: l.page.ID, Link: linkRaw(err), Reason: err.Error()})
		}
		for _, link := range links {
			g.report.Links++
			res := g.resolve(l.collection, link)
			if !res.Resolved {
				g.report.Dangling = append(g.report.Dangling, Dangling{Collection: l.collection, PageID: l.page.ID, Link: link.Raw, Reason: res.Reason})
				continue
			}
			if target := res.targetKey(l.collection); target != "" {
				g.backlinks[target] = append(g.backlinks[target], from)
			}
		}
		if l.page.IsPointer() {
			target, err := l.page.Pointer()
			if err != nil {
				g.pointerErr[from] = err.Error()
				g.report.Dangling = append(g.report.Dangling, Dangling{Collection: l.collection, PageID: l.page.ID, Link: l.page.Front.Target, Reason: err.Error(), Pointer: true})
				continue
			}
			g.report.Links++
			res := g.resolve(l.collection, target)
			g.pointers[from] = res
			if !res.Resolved {
				g.report.Dangling = append(g.report.Dangling, Dangling{Collection: l.collection, PageID: l.page.ID, Link: target.Raw, Reason: res.Reason, Pointer: true})
			}
		}
	}
	for k := range g.backlinks {
		sort.Strings(g.backlinks[k])
	}
	sort.Slice(g.report.Dangling, func(i, j int) bool {
		a, b := g.report.Dangling[i], g.report.Dangling[j]
		if a.Collection != b.Collection {
			return a.Collection < b.Collection
		}
		if a.PageID != b.PageID {
			return a.PageID < b.PageID
		}
		return a.Link < b.Link
	})
	return g
}

// linkRaw extracts the quoted entry from a kb.ParseLink error for the
// report; the error text already says what was wrong.
func linkRaw(err error) string {
	s := err.Error()
	if i := strings.Index(s, `"`); i >= 0 {
		if j := strings.Index(s[i+1:], `"`); j >= 0 {
			return s[i+1 : i+1+j]
		}
	}
	return s
}

// resolve decides whether one link's target exists.
func (g *linkGraph) resolve(from string, l kb.Link) ResolvedLink {
	switch l.Kind {
	case kb.LinkExternal:
		return ResolvedLink{Link: l, Resolved: true}
	case kb.LinkCollection:
		if !g.mounted[l.Collection] {
			return ResolvedLink{Link: l, Reason: fmt.Sprintf("collection %q is not mounted", l.Collection)}
		}
		return ResolvedLink{Link: l, Resolved: true}
	case kb.LinkQualified:
		if !g.mounted[l.Collection] {
			return ResolvedLink{Link: l, Reason: fmt.Sprintf("collection %q is not mounted", l.Collection)}
		}
		if _, ok := g.exists[qualify(l.Collection, l.ID)]; !ok {
			return ResolvedLink{Link: l, Reason: fmt.Sprintf("page %q not found in collection %q", l.ID, l.Collection)}
		}
		return ResolvedLink{Link: l, Resolved: true}
	default: // local
		if _, ok := g.exists[qualify(from, l.ID)]; !ok {
			return ResolvedLink{Link: l, Reason: fmt.Sprintf("page %q not found in collection %q", l.ID, from)}
		}
		return ResolvedLink{Link: l, Resolved: true}
	}
}

// targetKey is the qualified ID a resolved page link points at, or ""
// for links that name no single page.
func (l ResolvedLink) targetKey(from string) string {
	switch l.Kind {
	case kb.LinkLocal:
		return qualify(from, l.ID)
	case kb.LinkQualified:
		return qualify(l.Collection, l.ID)
	}
	return ""
}

// LinksOf returns what this registry view knows about ref's links:
// its `related:` entries resolved, its backlinks (viewer-filtered),
// and — for a pointer — its target.
func (r *Registry) LinksOf(ref PageRef) PageLinks {
	g := r.graph()
	v := r.viewerOf()
	from := qualify(ref.Collection, ref.Page.ID)
	var out PageLinks

	links, errs := ref.Page.Links()
	for _, err := range errs {
		out.Invalid = append(out.Invalid, err.Error())
	}
	for _, l := range links {
		out.Links = append(out.Links, g.resolve(ref.Collection, l))
	}
	for _, src := range g.backlinks[from] {
		coll, _, _ := strings.Cut(src, ":")
		c, ok := r.by[coll]
		if !ok {
			continue // outside this view
		}
		p, ok := g.exists[src]
		if !ok || !c.viewerFor(v).CanSee(p) {
			continue
		}
		out.LinkedFrom = append(out.LinkedFrom, src)
	}
	if ref.Page.IsPointer() {
		if msg, bad := g.pointerErr[from]; bad {
			out.PointerError = msg
		} else if res, ok := g.pointers[from]; ok {
			out.Pointer = &res
		}
	}
	return out
}

// PointerOf returns the resolved target of a pointer page in this view,
// or false when the page is not a (valid) pointer. It is the cheap form
// search hits use.
func (r *Registry) PointerOf(collection string, p kb.Page) (ResolvedLink, bool) {
	if !p.IsPointer() {
		return ResolvedLink{}, false
	}
	res, ok := r.graph().pointers[qualify(collection, p.ID)]
	return res, ok
}

// PointersTo lists the qualified IDs (`<collection>:<id>`) of every
// valid pointer page whose target is the named collection, sorted. It
// answers "is this collection reachable by a pointer, and from where?"
// — what the librarian's promotion pass needs to know before proposing
// another one (meerkat-mob issue #20).
func (r *Registry) PointersTo(collection string) []string {
	var out []string
	for from, res := range r.graph().pointers {
		if res.Kind == kb.LinkCollection && res.Collection == collection {
			out = append(out, from)
		}
	}
	sort.Strings(out)
	return out
}

// LinkReport returns the registry-wide link health. It is what
// Registry.Check folds into each collection's Health and what
// `mk lint` prints.
func (r *Registry) LinkReport() LinkReport { return r.graph().report }

// danglingFor returns the dangling entries of one collection.
func (r *Registry) danglingFor(name string) []Dangling {
	var out []Dangling
	for _, d := range r.graph().report.Dangling {
		if d.Collection == name {
			out = append(out, d)
		}
	}
	return out
}

// --- search hits and capability bundles --------------------------------

// Routing kinds a search hit can carry. "doc" is every page that is not
// one of the three routing types.
const (
	KindPointer = kb.TypePointer
	KindSkill   = kb.TypeSkill
	KindExample = kb.TypeExample
	KindDoc     = "doc"
)

// KindOf returns the routing kind of a page: pointer | skill | example
// | doc.
func KindOf(p kb.Page) string {
	switch p.Front.Type {
	case kb.TypePointer, kb.TypeSkill, kb.TypeExample:
		return p.Front.Type
	}
	return KindDoc
}

// routing fills the routing fields of a hit from the page's type and,
// for a pointer, its resolved target.
func (r *Registry) routing(h *Hit) {
	h.Kind = KindOf(h.Page)
	if h.Kind == KindPointer {
		if res, ok := r.PointerOf(h.Collection, h.Page); ok {
			h.Target = res.String()
			h.TargetResolved = res.Resolved
		}
		h.Hint = h.Page.Front.Hint
	}
}

// Bundle is a capability bundle: everything a hub knows about one
// destination, assembled from its pointer pages and whatever their
// `related:` links pull in. It is the tier-0 answer shape the mob
// design asks for ("I have to handle security signals" → the pointer
// to the vendor's MCP, the skills that use it, the worked examples, the
// docs — in one hit), grouped by where the agent would go next.
type Bundle struct {
	// Target is the destination every pointer in this bundle names
	// (`collection:flux`, `mcp://datadog`), or "" for hits that no
	// pointer claims.
	Target string `json:"target"`
	// Hint is the first pointer's hint.
	Hint     string `json:"hint,omitempty"`
	Pointers []Hit  `json:"pointers,omitempty"`
	Skills   []Hit  `json:"skills,omitempty"`
	Examples []Hit  `json:"examples,omitempty"`
	Docs     []Hit  `json:"docs,omitempty"`
	// Score is the best score in the bundle; bundles are ordered by it.
	Score float64 `json:"score"`
}

// Bundles groups search hits by pointer target.
//
// A pointer hit opens (or joins) the bundle for its target. Every other
// hit joins the bundle of the first pointer whose `related:` names it
// — a hub's pointer page lists the skills, examples and docs that go
// with the hop — and otherwise the unrouted bundle (Target ""), so
// nothing a search found is dropped. Bundles are ordered by their best
// hit's score; within a bundle the original order is kept.
func (r *Registry) Bundles(hits []Hit) []Bundle {
	if len(hits) == 0 {
		return nil
	}
	byTarget := map[string]*Bundle{}
	var order []string
	bundleFor := func(target string) *Bundle {
		b, ok := byTarget[target]
		if !ok {
			b = &Bundle{Target: target}
			byTarget[target] = b
			order = append(order, target)
		}
		return b
	}
	// claimed maps a qualified page ID to the target of the first
	// pointer whose related: names it.
	claimed := map[string]string{}
	for _, h := range hits {
		if h.Kind != KindPointer || h.Target == "" {
			continue
		}
		b := bundleFor(h.Target)
		if b.Hint == "" {
			b.Hint = h.Hint
		}
		b.Pointers = append(b.Pointers, h)
		links, _ := h.Page.Links()
		for _, l := range links {
			key := ResolvedLink{Link: l}.targetKey(h.Collection)
			if key == "" {
				continue
			}
			if _, taken := claimed[key]; !taken {
				claimed[key] = h.Target
			}
		}
	}
	for _, h := range hits {
		if h.Kind == KindPointer && h.Target != "" {
			continue
		}
		target := claimed[qualify(h.Collection, h.Page.ID)]
		b := bundleFor(target)
		switch h.Kind {
		case KindSkill:
			b.Skills = append(b.Skills, h)
		case KindExample:
			b.Examples = append(b.Examples, h)
		default:
			b.Docs = append(b.Docs, h)
		}
	}
	out := make([]Bundle, 0, len(order))
	for _, t := range order {
		b := byTarget[t]
		for _, group := range [][]Hit{b.Pointers, b.Skills, b.Examples, b.Docs} {
			for _, h := range group {
				if h.Score > b.Score {
					b.Score = h.Score
				}
			}
		}
		out = append(out, *b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
