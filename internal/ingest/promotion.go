package ingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// promotion.go is the librarian's promotion pass (meerkat-mob issue
// #20). Every level deeper in the tree costs a cold retrieval, so a
// knowledge base that agents travel to very often but that sits two or
// more levels down deserves a shortcut from the root: a pointer page
// that lets a session reach it in one hop instead of walking the hubs.
//
// The evidence is the cache's flushed temperatures (issue E) and the
// traversal log's session entries (issue G), both keyed by hashed
// names; the pass hashes the registry's own names with the log's key to
// match them. It proposes, in temperature order, the hottest TopN
// collections at depth >= MinDepth that no pointer in the root already
// reaches, and lists the pages sessions confirmed most inside each so
// a human can judge whether the pages themselves belong higher up.
//
// Report-only by default. Apply files the root pointer when the root's
// contract is `direct` (a memory-store write under global/pointers/);
// a `merge-request` root gets instructions; page moves are never
// automated — the cache observes, it does not rewrite content.

// FindingPromotion is the finding kind and the metrics label.
const FindingPromotion = "promotion"

// Defaults for the promotion pass.
const (
	// DefaultPromotionTopN is how many hot deep collections a run
	// proposes at most.
	DefaultPromotionTopN = 5
	// DefaultPromotionMinDepth is the shallowest depth that counts as
	// "deep": a depth-1 collection is already one hop from the root.
	DefaultPromotionMinDepth = 2
	// promotionPagesListed caps the confirmed-page list in a proposal.
	promotionPagesListed = 5
)

// Promotion is one proposed shortcut: a pointer in Hub to Collection.
type Promotion struct {
	// Collection is the hot deep collection.
	Collection string `json:"collection"`
	// Path and Depth are its place in the tree.
	Path  string `json:"path"`
	Depth int    `json:"depth"`
	// Hub is where the pointer would go: the tree's root.
	Hub string `json:"hub"`
	// Method is Hub's contract method.
	Method string `json:"method"`
	// Temperature is the collection's traversal counter at last flush.
	Temperature int64 `json:"temperature"`
	// ExistingPointers lists qualified IDs of pointers that already
	// reach the collection from elsewhere in the tree (never the hub;
	// a hub pointer means no proposal).
	ExistingPointers []string `json:"existing_pointers,omitempty"`
	// HotPages lists the pages sessions confirmed most, with counts.
	HotPages []HotPage `json:"hot_pages,omitempty"`
	// Hint is the pointer hint Apply would write.
	Hint string `json:"hint"`
}

// HotPage is a confirmed page and how many sessions confirmed it.
type HotPage struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// PageID is the pointer page's ID relative to the hub's content path
// (a merge request adds `<path>/<PageID>.md`); a direct filing lands
// in the hub's memory store as `global/<PageID>.md`, which the overlay
// serves as `memory/global/<PageID>`.
func (p Promotion) PageID() string { return "pointers/" + strings.ReplaceAll(p.Collection, "/", "-") }

// promotions computes the pass. temps and entries come from the log;
// the registry supplies names and tree positions.
func promotions(ctx context.Context, reg *collections.Registry, log *traversal.Log, opts LibrarianOpts) ([]Promotion, error) {
	temps, err := log.ReadTemperatures(ctx, opts.Days)
	if err != nil {
		return nil, err
	}
	if len(temps) == 0 {
		return nil, nil
	}
	entries, err := log.ReadSessions(ctx, opts.Days)
	if err != nil {
		return nil, err
	}
	var cands []Promotion
	for _, c := range reg.All() {
		if c.Tree == nil || c.Tree.Depth < opts.PromotionMinDepth {
			continue
		}
		t, ok := temps[log.Hash(c.Name)]
		if !ok || t.Temperature <= 0 {
			continue
		}
		hub := rootOf(reg, c)
		if hub == "" {
			continue
		}
		p := Promotion{Collection: c.Name, Path: c.Tree.Path, Depth: c.Tree.Depth, Hub: hub, Temperature: t.Temperature, Method: contractMethodByName(reg, hub)}
		fromHub := false
		for _, from := range reg.PointersTo(c.Name) {
			if strings.HasPrefix(from, hub+":") {
				fromHub = true
				break
			}
			p.ExistingPointers = append(p.ExistingPointers, from)
		}
		if fromHub {
			continue
		}
		p.HotPages = hotPages(reg, log, c, entries)
		p.Hint = pointerHint(c)
		cands = append(cands, p)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Temperature != cands[j].Temperature {
			return cands[i].Temperature > cands[j].Temperature
		}
		return cands[i].Collection < cands[j].Collection
	})
	if len(cands) > opts.PromotionTopN {
		cands = cands[:opts.PromotionTopN]
	}
	return cands, nil
}

// rootOf walks the tree upwards to the depth-0 hub. It uses the
// collections' own tree nodes rather than Registry.Root so a registry
// assembled from parts (tests, embedded builds) works the same.
func rootOf(reg *collections.Registry, c *collections.Collection) string {
	for i := 0; i <= contentsource.MaxTreeDepth && c != nil && c.Tree != nil; i++ {
		if c.Tree.Depth == 0 || c.Tree.Parent == "" {
			return c.Name
		}
		parent, err := reg.Get(c.Tree.Parent)
		if err != nil {
			return ""
		}
		c = parent
	}
	return ""
}

// hotPages counts, per page of c, the sessions that confirmed it
// (Entry.Pages are hashed qualified IDs) and returns the top few.
func hotPages(reg *collections.Registry, log *traversal.Log, c *collections.Collection, entries []traversal.Entry) []HotPage {
	refs, err := reg.Pages(c.Name)
	if err != nil {
		return nil
	}
	byHash := make(map[string]string, len(refs))
	for _, ref := range refs {
		byHash[log.Hash(c.Name+":"+ref.Page.ID)] = ref.Page.ID
	}
	counts := map[string]int{}
	for _, e := range entries {
		for _, h := range e.Pages {
			if id, ok := byHash[h]; ok {
				counts[id]++
			}
		}
	}
	out := make([]HotPage, 0, len(counts))
	for id, n := range counts {
		out = append(out, HotPage{ID: id, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > promotionPagesListed {
		out = out[:promotionPagesListed]
	}
	return out
}

// pointerHint is the hint a filed pointer carries: the collection's
// declared description, or its tree path when there is none. Kept
// under the pointer hint cap.
func pointerHint(c *collections.Collection) string {
	hint := strings.TrimSpace(c.Source.Description)
	if hint == "" && c.Tree != nil {
		hint = strings.TrimSpace(c.Tree.Description)
	}
	if hint == "" {
		hint = fmt.Sprintf("Knowledge base %s (%s).", c.Name, c.Tree.Path)
	}
	const cap = 300
	if len(hint) > cap {
		hint = hint[:cap-1] + "…"
	}
	return hint
}

func (p Promotion) detail() string {
	var b strings.Builder
	fmt.Fprintf(&b, "temperature %d at depth %d (%s); propose a pointer in %s via the %s contract", p.Temperature, p.Depth, p.Path, p.Hub, p.Method)
	if len(p.ExistingPointers) > 0 {
		fmt.Fprintf(&b, "; reachable today only from %s", strings.Join(p.ExistingPointers, ", "))
	}
	if len(p.HotPages) > 0 {
		parts := make([]string, 0, len(p.HotPages))
		for _, h := range p.HotPages {
			parts = append(parts, fmt.Sprintf("%s (%d)", h.ID, h.Count))
		}
		fmt.Fprintf(&b, "; pages sessions confirmed most: %s — consider moving them up", strings.Join(parts, ", "))
	}
	return b.String()
}

// pointerPage renders the pointer page Apply files.
func (p Promotion) pointerPage() []byte {
	page := kb.Page{ID: p.PageID(), Title: p.Collection, Front: kb.Frontmatter{
		Type:   kb.TypePointer,
		Target: "collection:" + p.Collection,
		Hint:   p.Hint,
	}}
	page.Body = fmt.Sprintf("# %s\n\nShortcut filed by the librarian: %s was traversed %d times at depth %d (%s).\n", p.Collection, p.Collection, p.Temperature, p.Depth, p.Path)
	return renderPage(page)
}

// applyPromotions files each proposal through the hub's contract.
func applyPromotions(ctx context.Context, reg *collections.Registry, rep *Report) ([]Applied, error) {
	var out []Applied
	for _, p := range rep.Promotions {
		a := Applied{IntakeID: "promote:" + p.Collection}
		hub, err := reg.Get(p.Hub)
		if err != nil {
			a.Action, a.Detail = "skipped", err.Error()
			out = append(out, a)
			continue
		}
		switch p.Method {
		case contentsource.UpdateDirect:
			st := hub.Memory()
			if st == nil {
				a.Action, a.Detail = "skipped", "contract is direct but the hub has no memory store"
				out = append(out, a)
				continue
			}
			key := "global/" + p.PageID() + ".md"
			// SaveMemory writes the store and publishes the page into the
			// overlay and index in one step, so the next report's link
			// graph already sees the pointer and proposes nothing.
			if _, _, err := hub.SaveMemory(ctx, key, p.pointerPage(), memory.CreateOnly()); err != nil && !errors.Is(err, memory.ErrConflict) {
				return out, fmt.Errorf("file pointer to %q into %q: %w", p.Collection, p.Hub, err)
			}
			a.Action, a.Detail = "filed", st.Location(key)
		case contentsource.UpdateMergeRequest:
			ec := hub.Contract()
			a.Action, a.Detail = "instructions", fmt.Sprintf("open a merge request on %s (%s, branch %s, path %s) adding pointer %s -> collection:%s with hint %q; %s", ec.Repo, ec.Host, ec.Branch, ec.Path, p.PageID(), p.Collection, p.Hint, strings.TrimSpace(ec.Instructions))
		default:
			a.Action, a.Detail = "instructions", "hub "+p.Hub+" declares no contribution path; a human must add pointer "+p.PageID()+" -> collection:"+p.Collection
		}
		out = append(out, a)
	}
	return out, nil
}
