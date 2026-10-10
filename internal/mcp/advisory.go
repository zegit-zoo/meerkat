package mcp

import (
	"container/list"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/collections"
)

// advisory.go is the freshness advisory of meerkat-mob#25 part C: when a
// collection a caller searched or read from is stale (behind-disk,
// behind-remote, dirty, diverged), the tool result carries one extra
// line saying so.
//
// Three rules, as agreed on meerkat-mob#25:
//
//   - It is a SECOND text item, after the result item, never inside page
//     text. A client that parses the first text item keeps working, and
//     no advisory can be mistaken for knowledge-base content.
//   - It is sent at most once per (session, collection, state). The
//     session is the key mk_report_outcome uses: an explicit session_id,
//     else the MCP session, else one bucket per principal — always scoped
//     to the caller's principal (advisoryKey, meerkat-mob#46). A new
//     state is a new advisory.
//   - It is bounded: one line, at most collections.MaxAdvisory bytes,
//     naming the collection and the state and nothing else.
//
// The seen-set is shared by every session, under one lock, so its cost
// per key is constant: keys sit in a list in the order they were last
// advised, which is also the order they expire in. Expired keys leave
// from the front as new ones arrive, and a full set drops its oldest,
// so no caller can make the lock expensive by varying session_id
// (#118 review S1).

const (
	advisoryCap = 10000            // (session, collection, state) keys remembered
	advisoryTTL = 30 * time.Minute // how long a key suppresses a repeat
	maxAdvised  = 5                // advisory items on one result
)

// advisories remembers which advisories each session has already seen.
type advisories struct {
	mu    sync.Mutex
	seen  map[string]*list.Element // key -> element holding a seenKey
	order *list.List               // oldest advised at the front
	now   func() time.Time
}

type seenKey struct {
	key string
	at  time.Time
}

func newAdvisories() *advisories {
	return &advisories{seen: make(map[string]*list.Element), order: list.New(), now: time.Now}
}

// record is one collection's name and freshness, as take reads them.
type record struct {
	name string
	f    collections.Freshness
}

// take returns the advisory lines for cols that session has not seen in
// their current state, and records them as seen.
func (a *advisories) take(session string, cols []*collections.Collection) []string {
	if a == nil {
		return nil
	}
	recs := make([]record, 0, len(cols))
	for _, c := range cols {
		if f, ok := c.Freshness(); ok {
			recs = append(recs, record{name: c.Name, f: f})
		}
	}
	return a.takeRecords(session, recs)
}

func (a *advisories) takeRecords(session string, recs []record) []string {
	var out []string
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for _, r := range recs {
		if len(out) == maxAdvised {
			break
		}
		if !r.f.Stale() {
			continue
		}
		key := session + "\x00" + r.name + "\x00" + r.f.State
		if el, seen := a.seen[key]; seen && now.Sub(el.Value.(seenKey).at) < advisoryTTL {
			continue
		}
		a.remember(key, now)
		out = append(out, r.f.Advisory(r.name))
	}
	return out
}

// remember records key as advised now. Keys that have expired leave
// from the front first; if the set is still full, the oldest goes. Both
// are O(1) per key, so the set never grows past advisoryCap and never
// costs a scan.
func (a *advisories) remember(key string, now time.Time) {
	if el, ok := a.seen[key]; ok {
		el.Value = seenKey{key: key, at: now}
		a.order.MoveToBack(el)
		return
	}
	for front := a.order.Front(); front != nil && now.Sub(front.Value.(seenKey).at) >= advisoryTTL; front = a.order.Front() {
		a.drop(front)
	}
	if len(a.seen) >= advisoryCap {
		a.drop(a.order.Front())
	}
	a.seen[key] = a.order.PushBack(seenKey{key: key, at: now})
}

func (a *advisories) drop(el *list.Element) {
	delete(a.seen, el.Value.(seenKey).key)
	a.order.Remove(el)
}

// withAdvisories appends each advisory line as its own text item AFTER
// the result's own content.
func withAdvisories(res *mcp.CallToolResult, lines []string) *mcp.CallToolResult {
	for _, l := range lines {
		res.Content = append(res.Content, mcp.NewTextContent(l))
	}
	return res
}

// searchedCollections is what a search looked in: the named collection,
// or every collection in view.
func searchedCollections(view *collections.Registry, name string) []*collections.Collection {
	if name == "" {
		return view.All()
	}
	c, err := view.Get(name)
	if err != nil {
		return nil
	}
	return []*collections.Collection{c}
}
