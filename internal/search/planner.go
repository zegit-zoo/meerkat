package search

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// planner.go is the staged query planner meerkat-mob issue C asked for.
//
// meerkat is described as "fuzzy-searchable" and, until this file, was
// not: a typo in a vendor name ("datadgo") or a Swedish/English mix
// ("drift-detektering") returned nothing, which in a mob of knowledge
// bases means a wasted hop and a false "not found". The planner keeps
// the exact BM25 query as the first and usual answer and falls back, in
// order, to
//
//	fuzzy   — every term of five or more characters may differ by one
//	          edit (two from eight characters); shorter terms stay exact.
//	prefix  — every term of three or more characters matches as a prefix.
//
// A stage runs only when the previous one found nothing, so a query
// that hits exactly costs what it always cost. The stage that produced
// the hits is reported (Result.Stage, and meerkat.search.stage /
// meerkat_search_total{stage}) so the fallback rate is measurable and a
// content gap ("everyone types datadgo") can be fixed at the source.
//
// Advanced query syntax — field targeting (title:foo), quoted phrases,
// boolean operators, wildcards — is never rewritten: the author asked
// for precision, and a fuzzy rewrite of `title:"rate limiting"` would
// not be the query they wrote. Such queries get the exact stage only.

// Stage names the planner stage that produced a result set. They form
// a closed set (see telemetry.SearchStage).
type Stage string

const (
	StageExact  Stage = "exact"
	StageFuzzy  Stage = "fuzzy"
	StagePrefix Stage = "prefix"
)

// Fuzziness thresholds: one edit from fuzzyMinLen characters, two from
// fuzzyTwoLen. Below fuzzyMinLen a term is short enough that one edit
// reaches unrelated words ("helm" → "held"), so it stays exact.
const (
	fuzzyMinLen  = 5
	fuzzyTwoLen  = 8
	prefixMinLen = 3
)

// QueryStaged is QueryAs plus the stage that answered. QueryAs remains
// the plain form for callers that do not report stages.
func (i *Index) QueryStaged(ctx context.Context, v kb.Viewer, q string, limit int) ([]Result, Stage, error) {
	if q == "" {
		return nil, StageExact, nil
	}
	if err := validateQuery(q); err != nil {
		return nil, StageExact, err
	}
	limit = clampLimit(limit)

	out, err := i.run(ctx, v, i.exactQuery(q), limit)
	if err != nil || len(out) > 0 {
		return stamp(out, StageExact), StageExact, err
	}
	terms := fallbackTerms(q)
	if len(terms) == 0 {
		return nil, StageExact, nil
	}
	if fq := i.fuzzyQuery(terms); fq != nil {
		out, err = i.run(ctx, v, fq, limit)
		if err != nil || len(out) > 0 {
			return stamp(out, StageFuzzy), StageFuzzy, err
		}
	}
	if pq := i.prefixQuery(terms); pq != nil {
		out, err = i.run(ctx, v, pq, limit)
		if err != nil || len(out) > 0 {
			return stamp(out, StagePrefix), StagePrefix, err
		}
	}
	return nil, StagePrefix, nil
}

func stamp(out []Result, s Stage) []Result {
	for k := range out {
		out[k].Stage = s
	}
	return out
}

// fallbackTerms tokenises a plain query for the fuzzy and prefix
// stages, or returns nil when the query uses bleve syntax the planner
// must not rewrite.
func fallbackTerms(q string) []string {
	if strings.ContainsAny(q, `:"*?~^()[]{}\`) || hasOperatorPrefix(q) {
		return nil
	}
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := make(map[string]bool, len(fields))
	out := fields[:0]
	for _, f := range fields {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// hasOperatorPrefix reports a bleve boolean operator: a "+" or "-" at
// the start of a term ("+datadog -flux"). A hyphen inside a term
// ("drift-detektering", "helm-release") is a word joiner, not syntax,
// and such mixed-language compounds are exactly what the fuzzy stage
// exists for.
func hasOperatorPrefix(q string) bool {
	for _, f := range strings.Fields(q) {
		if len(f) > 1 && (f[0] == '+' || f[0] == '-') {
			return true
		}
	}
	return false
}

// fuzzyQuery builds the fuzzy stage: per term, a disjunction over
// title/id/body with the same boosts the exact stage uses; terms too
// short to fuzz match exactly. Nil when no term can be fuzzed — the
// stage would then be the exact stage again.
func (i *Index) fuzzyQuery(terms []string) query.Query {
	fuzzable := false
	perTerm := make([]query.Query, 0, len(terms))
	for _, t := range terms {
		f := 0
		switch {
		case len(t) >= fuzzyTwoLen:
			f = 2
		case len(t) >= fuzzyMinLen:
			f = 1
		}
		if f > 0 {
			fuzzable = true
		}
		perTerm = append(perTerm, i.termClauses(t, func(field string, boost float64) query.Query {
			if f == 0 {
				m := bleve.NewMatchQuery(t)
				m.SetField(field)
				m.SetBoost(boost)
				return m
			}
			fq := bleve.NewFuzzyQuery(t)
			fq.SetField(field)
			fq.SetFuzziness(f)
			fq.SetBoost(boost)
			return fq
		}))
	}
	if !fuzzable {
		return nil
	}
	return bleve.NewDisjunctionQuery(perTerm...)
}

// prefixQuery builds the prefix stage: per term of prefixMinLen or
// more characters, a prefix match over title/id/body.
func (i *Index) prefixQuery(terms []string) query.Query {
	perTerm := make([]query.Query, 0, len(terms))
	for _, t := range terms {
		if len(t) < prefixMinLen {
			continue
		}
		perTerm = append(perTerm, i.termClauses(t, func(field string, boost float64) query.Query {
			p := bleve.NewPrefixQuery(t)
			p.SetField(field)
			p.SetBoost(boost)
			return p
		}))
	}
	if len(perTerm) == 0 {
		return nil
	}
	return bleve.NewDisjunctionQuery(perTerm...)
}

// termClauses applies build to the three content fields with the exact
// stage's relative boosts and returns their disjunction.
func (i *Index) termClauses(term string, build func(field string, boost float64) query.Query) query.Query {
	_ = term
	return bleve.NewDisjunctionQuery(
		build("title", 5.0),
		build("id", 3.0),
		build("body", 1.0),
	)
}

// --- keyword-field matching (mk_list filters as query clauses) --------

// Filters is a conjunction of exact keyword matches over the indexed
// frontmatter fields. Every set field must match; an empty Filters
// matches every page the viewer can see.
type Filters struct {
	Prefix      string
	Category    string
	Subcategory string
	Status      string
	Type        string
	Owner       string
	// Tags must all be present on the page.
	Tags []string
}

// Match returns the pages matching f, in ID order, using the keyword
// fields the index carries for category, subcategory, status, type,
// tags and owner instead of a post-list filter over every page. Prefix
// is applied to the page ID after the index narrows the set.
func (i *Index) Match(ctx context.Context, v kb.Viewer, f Filters) ([]kb.Page, error) {
	var clauses []query.Query
	term := func(field, value string) {
		if value == "" {
			return
		}
		t := bleve.NewTermQuery(value)
		t.SetField(field)
		clauses = append(clauses, t)
	}
	term("category", f.Category)
	term(subcategoryField, f.Subcategory)
	term(statusField, f.Status)
	term(typeField, f.Type)
	for _, tag := range f.Tags {
		term(tagsField, tag)
	}
	if f.Owner != "" {
		term("front.owner", f.Owner)
	}
	var q query.Query
	switch len(clauses) {
	case 0:
		q = bleve.NewMatchAllQuery()
	default:
		q = bleve.NewConjunctionQuery(clauses...)
	}
	if vis := visibilityClause(v); vis != nil {
		q = bleve.NewConjunctionQuery(vis, q)
	}
	i.mu.RLock()
	size := len(i.pages)
	i.mu.RUnlock()
	req := bleve.NewSearchRequestOptions(q, max(size, 1), 0, false)
	res, err := i.bleve.SearchInContext(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]kb.Page, 0, len(res.Hits))
	for _, hit := range res.Hits {
		p, ok := i.page(hit.ID)
		if !ok || !v.CanSee(p) {
			continue
		}
		if f.Prefix != "" && !strings.HasPrefix(p.ID, f.Prefix) {
			continue
		}
		if f.Owner != "" && p.Front.Owner != f.Owner {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}
