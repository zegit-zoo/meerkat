package mcp

import (
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
//     else the MCP session, else one shared bucket. A new state is a new
//     advisory.
//   - It is bounded: one line, at most collections.MaxAdvisory bytes,
//     naming the collection and the state and nothing else.

const (
	advisoryCap = 10000            // (session, collection, state) keys remembered
	advisoryTTL = 30 * time.Minute // how long a key suppresses a repeat
	maxAdvised  = 5                // advisory items on one result
)

// advisories remembers which advisories each session has already seen.
type advisories struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func newAdvisories() *advisories {
	return &advisories{seen: make(map[string]time.Time), now: time.Now}
}

// take returns the advisory lines for cols that session has not seen in
// their current state, and records them as seen.
func (a *advisories) take(session string, cols []*collections.Collection) []string {
	if a == nil {
		return nil
	}
	var out []string
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for _, c := range cols {
		if len(out) == maxAdvised {
			break
		}
		f, ok := c.Freshness()
		if !ok || !f.Stale() {
			continue
		}
		key := session + "\x00" + c.Name + "\x00" + f.State
		if at, seen := a.seen[key]; seen && now.Sub(at) < advisoryTTL {
			continue
		}
		a.remember(key, now)
		out = append(out, f.Advisory(c.Name))
	}
	return out
}

// remember records key, evicting expired keys first and then the oldest
// when the set is full, so it never grows past advisoryCap.
func (a *advisories) remember(key string, now time.Time) {
	if len(a.seen) >= advisoryCap {
		for k, at := range a.seen {
			if now.Sub(at) >= advisoryTTL {
				delete(a.seen, k)
			}
		}
	}
	if len(a.seen) >= advisoryCap {
		var oldest string
		var oldestAt time.Time
		for k, at := range a.seen {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = k, at
			}
		}
		delete(a.seen, oldest)
	}
	a.seen[key] = now
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
