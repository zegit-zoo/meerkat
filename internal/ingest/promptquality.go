package ingest

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// promptquality.go is the librarian's prompt-quality analysis
// (meerkat-mob issue #21): how well do clients ask, and how well does
// meerkat turn a weak question into the right page? The evidence is
// the traversal log's session entries — the initial query verbatim,
// the outcome, the attempted path (hashed), the planner stages that
// answered and the wrong-turn count (issue F's session summary). The
// pass matches hashed collection names against the registry and
// produces three kinds of rewrite target:
//
//   - hint: sessions gave up in a collection whose pointer hints
//     mention none of the query's terms. The agent chose the pointer
//     (or was sent there) and found nothing; the hint promised the
//     wrong thing or too little. Rewrite the hint.
//   - description: sessions found their page only after the exact
//     stage missed and the fuzzy or prefix stage answered. The pages
//     are right but the words agents use are not on them. Add the
//     terms to page descriptions (or aliases).
//   - tool: sessions gave up having touched only the root, or without
//     navigating at all. The agent never left tier 0, so the tool
//     description did not tell it where to go. Rewrite the tool text.
//   - route: sessions starting in a hub took a wrong turn first. The
//     hub's pointer hints did not separate its children; rewrite them
//     so the right child is chosen first.
//
// Report-only: the findings quote the queries so a human (or the
// rewrite stage, a later slice) can act. Nothing here edits a page.

// FindingPromptQuality is the finding kind and the metrics label.
const FindingPromptQuality = "prompt_quality"

// Rewrite targets.
const (
	TargetHint        = "hint"
	TargetDescription = "description"
	TargetTool        = "tool"
	TargetRoute       = "route"
)

// Defaults for the analysis.
const (
	// DefaultPromptMinSessions is how many sessions must share a target
	// before it is reported.
	DefaultPromptMinSessions = 2
	// promptQueriesListed caps the queries quoted per finding.
	promptQueriesListed = 5
)

// PromptFinding is one rewrite target with its evidence.
type PromptFinding struct {
	// Target is hint | description | tool | route.
	Target string `json:"target"`
	// Collection is the collection the rewrite concerns: the one
	// sessions gave up in (hint), found in (description), or started
	// in (route); the root or "" for a tool finding.
	Collection string `json:"collection,omitempty"`
	// Pages lists the pointer pages (qualified IDs) whose hints are the
	// rewrite target, for hint and route findings.
	Pages []string `json:"pages,omitempty"`
	// Queries are the initial queries, deduplicated, most frequent first.
	Queries []string `json:"queries"`
	// Sessions is how many sessions support the finding.
	Sessions int `json:"sessions"`
	// Terms are the query words no hint mentions (hint findings).
	Terms []string `json:"terms,omitempty"`
}

func (f PromptFinding) detail() string {
	q := f.Queries
	if len(q) > promptQueriesListed {
		q = q[:promptQueriesListed]
	}
	quoted := make([]string, len(q))
	for i, s := range q {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	queries := strings.Join(quoted, ", ")
	switch f.Target {
	case TargetHint:
		return fmt.Sprintf("%d sessions gave up here; the pointer hint(s) %s mention none of their terms (%s); rewrite the hint to say what is here: %s",
			f.Sessions, strings.Join(f.Pages, ", "), strings.Join(f.Terms, ", "), queries)
	case TargetDescription:
		return fmt.Sprintf("%d sessions found their page only after the exact stage missed (fuzzy/prefix answered); add their words to the page descriptions: %s",
			f.Sessions, queries)
	case TargetTool:
		return fmt.Sprintf("%d sessions gave up without leaving tier 0; the mk_search tool description did not send them anywhere: %s",
			f.Sessions, queries)
	default:
		return fmt.Sprintf("%d sessions starting here took a wrong turn first; the hub's pointer hints (%s) do not separate its children: %s",
			f.Sessions, strings.Join(f.Pages, ", "), queries)
	}
}

// promptQuality runs the analysis over the last days of the log.
func promptQuality(ctx context.Context, reg *collections.Registry, log *traversal.Log, opts LibrarianOpts) ([]PromptFinding, error) {
	entries, err := log.ReadSessions(ctx, opts.Days)
	if err != nil {
		return nil, err
	}
	byHash := map[string]string{}
	for _, name := range reg.Names() {
		byHash[log.Hash(name)] = name
	}
	root := ""
	for _, c := range reg.All() {
		if c.Tree != nil && c.Tree.Depth == 0 {
			root = c.Name
			break
		}
	}
	hints := pointerHints(reg)

	type bucket struct {
		f       PromptFinding
		queries map[string]int
		terms   map[string]bool
	}
	buckets := map[string]*bucket{}
	add := func(target, collection string, pages []string, query string, terms []string) {
		k := target + "\x00" + collection
		b, ok := buckets[k]
		if !ok {
			b = &bucket{f: PromptFinding{Target: target, Collection: collection, Pages: pages}, queries: map[string]int{}, terms: map[string]bool{}}
			buckets[k] = b
		}
		b.f.Sessions++
		b.queries[query]++
		for _, t := range terms {
			b.terms[t] = true
		}
	}

	for _, e := range entries {
		query := strings.TrimSpace(e.InitialQuery)
		if query == "" {
			continue
		}
		attempted := make([]string, 0, len(e.Attempted))
		for _, h := range e.Attempted {
			if name, ok := byHash[h]; ok {
				attempted = append(attempted, name)
			}
		}
		gaveUp := e.Outcome == "gave_up" || e.Outcome == "not_found"
		terms := queryTerms(query)

		switch {
		case gaveUp && (len(attempted) == 0 || (len(attempted) == 1 && attempted[0] == root)):
			add(TargetTool, root, nil, query, nil)
		case gaveUp:
			last := attempted[len(attempted)-1]
			ptrs := hints[last]
			if len(ptrs) == 0 {
				continue
			}
			missing := termsNotMentioned(terms, ptrs)
			// A hint problem when the hints carry fewer than half the
			// query's words: "rotate the datadog api key" against a hint
			// that only says "Dashboards" (1 of 4) is one; "drift
			// remediation" against "…drift." (1 of 2) is not — the hint
			// routed correctly and the page is what is missing.
			if len(missing) == 0 || (len(terms)-len(missing))*2 >= len(terms) {
				continue
			}
			add(TargetHint, last, pointerIDs(ptrs), query, missing)
		case e.Outcome == "found" && (e.Stages["fuzzy"] > 0 || e.Stages["prefix"] > 0):
			where := ""
			if len(attempted) > 0 {
				where = attempted[len(attempted)-1]
			}
			add(TargetDescription, where, nil, query, nil)
		}
		if e.WrongTurns > 0 && len(attempted) >= 2 {
			start := attempted[0]
			if ptrs := pointerHintsIn(reg, start); len(ptrs) > 0 {
				add(TargetRoute, start, ptrs, query, nil)
			}
		}
	}

	var out []PromptFinding
	for _, b := range buckets {
		if b.f.Sessions < opts.PromptMinSessions {
			continue
		}
		type qc struct {
			q string
			n int
		}
		qs := make([]qc, 0, len(b.queries))
		for q, n := range b.queries {
			qs = append(qs, qc{q, n})
		}
		sort.Slice(qs, func(i, j int) bool {
			if qs[i].n != qs[j].n {
				return qs[i].n > qs[j].n
			}
			return qs[i].q < qs[j].q
		})
		for _, x := range qs {
			b.f.Queries = append(b.f.Queries, x.q)
		}
		for t := range b.terms {
			b.f.Terms = append(b.f.Terms, t)
		}
		sort.Strings(b.f.Terms)
		out = append(out, b.f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Collection < out[j].Collection
	})
	return out, nil
}

// pointerHint is one pointer page's text, for term matching.
type pointerText struct {
	id   string // qualified
	text string // lower-cased title + hint + description
}

// pointerHints maps every collection to the pointers that reach it,
// with their text.
func pointerHints(reg *collections.Registry) map[string][]pointerText {
	out := map[string][]pointerText{}
	for _, c := range reg.All() {
		for _, from := range reg.PointersTo(c.Name) {
			coll, id, ok := strings.Cut(from, ":")
			if !ok {
				continue
			}
			ref, err := reg.Show(coll, id)
			if err != nil {
				continue
			}
			p := ref.Page
			out[c.Name] = append(out[c.Name], pointerText{id: from, text: strings.ToLower(p.Title + " " + p.Front.Hint + " " + p.Front.Description + " " + p.ID)})
		}
	}
	return out
}

// pointerHintsIn lists the qualified IDs of the pointer pages a hub
// holds (its routing surface).
func pointerHintsIn(reg *collections.Registry, hub string) []string {
	refs, err := reg.Pages(hub)
	if err != nil {
		return nil
	}
	var out []string
	for _, ref := range refs {
		if ref.Page.IsPointer() {
			out = append(out, hub+":"+ref.Page.ID)
		}
	}
	sort.Strings(out)
	return out
}

func pointerIDs(ptrs []pointerText) []string {
	out := make([]string, len(ptrs))
	for i, p := range ptrs {
		out[i] = p.id
	}
	sort.Strings(out)
	return out
}

// termsNotMentioned returns the query terms that no pointer text
// contains, as a substring, so "datadgo" is not found in "datadog"
// (that is the point) while "alert" is found in "alerting".
func termsNotMentioned(terms []string, ptrs []pointerText) []string {
	var out []string
	for _, t := range terms {
		found := false
		for _, p := range ptrs {
			if strings.Contains(p.text, t) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, t)
		}
	}
	return out
}

// queryTerms splits a query into lower-cased words of three letters or
// more, dropping bleve operator prefixes, quotes and field syntax, and
// a few function words that carry no topic.
func queryTerms(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(strings.ToLower(q)) {
		f = strings.Trim(f, "+-\"'()*~^:?!,.;")
		if i := strings.IndexByte(f, ':'); i > 0 {
			f = f[i+1:]
		}
		if len(f) < 3 || stopword[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

var stopword = map[string]bool{
	"the": true, "and": true, "for": true, "how": true, "what": true, "where": true, "when": true, "why": true,
	"with": true, "from": true, "that": true, "this": true, "are": true, "does": true, "can": true, "not": true,
	"about": true, "into": true, "our": true, "you": true, "your": true, "which": true, "who": true,
}
