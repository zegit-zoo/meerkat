package search

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/blevesearch/bleve/v2/search/searcher"
)

// Pre-parse query limits.
//
// Background: bleve's query-string lexer (search/query/query_string_lex.go
// in bleve v2.6.0) builds each token via repeated string concatenation
// (`l.buf += string(next)` in inStrState/inNumOrStrState/inBoostState/
// inTildeState) — quadratic in the length of a single unbroken token.
// Its grammar (search/query/query_string.y) has no grouping operator at
// all (no '(' / ')' token anywhere in the token/grammar declarations),
// so parentheses lex as ordinary "word" characters rather than being
// specially handled: a long run of them (nested or not) becomes ONE
// token built via that quadratic loop, and the cost is paid inside
// bleve.NewQueryStringQuery's eventual parse — synchronously, with no
// yield point — before bleve.SearchInContext's collector ever starts
// checking ctx.Done(). Threading a context (see Index.QueryContext)
// cannot bound this cost; only rejecting the input before it reaches
// bleve can.
//
// Measured against a 2-page fixture index (this repo, Apple M-series):
// 200,000 nested '(' characters took ~2.3s wall time at 300%+ CPU
// (multi-core, from GC pressure churning through the many intermediate
// string allocations); the query-string mini-language itself supports
// nothing resembling that input, and the corpus size is irrelevant —
// the cost is entirely in lexing/parsing the string once, before a
// single document is ever scored.
const (
	// maxQueryBytes caps the raw query length. Every real free-text
	// search against a KB of a few hundred/thousand pages is a short
	// phrase or a natural-language question — comfortably under 200
	// bytes even for a verbose one (see docs/SEARCH.md's examples: none
	// exceed a few words). 512 leaves 2-3x headroom for an unusually
	// long query while keeping the worst-case cost of the checks below,
	// and of bleve's own parse, negligible: extrapolating quadratically
	// from the 200,000-char measurement above, a 512-byte token costs on
	// the order of (512/200_000)^2 * 2.3s ≈ 15 microseconds.
	maxQueryBytes = 512

	// maxParenDepth caps the running open-paren nesting depth (a
	// '('-increments/')'-decrements counter, clamped at zero, tracking
	// its high-water mark as the query is scanned once left to right).
	// This directly targets the confirmed attack shape: an unbroken run
	// of nested '(' characters. Bleve's query-string language doesn't
	// use parens for grouping at all today (see above) — legitimate
	// queries never nest them — so 8 is already far more headroom than
	// any real query needs, in case a future bleve version adds real
	// grouping semantics.
	maxParenDepth = 8

	// maxQueryTerms caps the number of whitespace-separated terms, a
	// cheap proxy for the boolean-clause count the query-string parser
	// will build (query_string.y's searchParts rule is right-recursive:
	// one clause is added via AddShould/AddMust/AddMustNot per term). A
	// real search phrase is a handful of words; 64 is already an
	// extreme, non-organic query — docs/SEARCH.md's longest documented
	// example query is 2 terms.
	maxQueryTerms = 64

	// DefaultLimit is the result-count limit applied when a caller
	// passes limit <= 0. Matches the "default: 10" documented in
	// internal/http/openapi.go.
	DefaultLimit = 10

	// MaxLimit is the maximum number of results any query can request,
	// matching the "maximum: 100" contract documented in
	// internal/http/openapi.go. Enforced here (rather than only in the
	// HTTP handler) so every caller — CLI, HTTP, MCP — gets the same
	// cap, and the documented contract can't silently drift from
	// enforcement again.
	MaxLimit = 100

	// DefaultQueryTimeout is the recommended server-side ceiling on a
	// single query's execution. It is applied by callers that derive a
	// request-scoped context (see internal/http.Config.QueryTimeout and
	// internal/mcp's search handler), not by QueryContext itself, so a
	// caller with a specific reason for a different budget can still
	// supply its own context deadline — QueryContext just honours
	// whatever it's given.
	//
	// Chosen well above the slowest legitimate query we measured (a "*"
	// match-all against a sizeable corpus took 2.2-2.8s in the
	// originating vulnerability report) so it will not cut off real
	// traffic, while still bounding a single query to a fixed, small
	// multiple of that instead of letting it run unbounded.
	DefaultQueryTimeout = 10 * time.Second

	// maxMultiTermClauses caps the wildcard (`foo*`, `fo?`) and regexp
	// (`/fo+/`) clauses one query may contain. Each such clause is
	// expanded by bleve into one term searcher per matching dictionary
	// term, and that expansion is built in full before the search's
	// context is ever consulted, so its cost is paid per clause and
	// scales with the size of the collection's vocabulary rather than
	// with the length of the query. A real query uses one ("kube*"), now
	// and then two ("helm* flux*"); 2 keeps that working while bounding
	// the expansion work to a small constant multiple of
	// maxTermExpansion. Fuzzy terms (`foo~1`) are not counted: bleve
	// caps their edit distance at 2, which keeps each Levenshtein
	// automaton small, and their candidate lists are bounded by
	// maxTermExpansion like any other expansion.
	maxMultiTermClauses = 2

	// maxTermExpansion is the most dictionary terms one wildcard, regexp,
	// prefix or fuzzy clause may expand to, and is installed as bleve's
	// process-wide searcher.DisjunctionMaxClauseCount (see init below).
	// Bleve's default is 0, meaning unlimited: a single clause matching
	// every term in a large collection then allocates a searcher per
	// term. A clause over the limit fails the query with ErrInvalidQuery
	// ("make the pattern more specific") instead of silently truncating,
	// because a truncated expansion would rank an arbitrary subset of the
	// matching terms. 1024 is far above what a meaningful pattern
	// matches in a knowledge base, and the same limit bounds the
	// planner's prefix and fuzzy stages. Every other disjunction meerkat
	// builds has at most a few dozen clauses (maxQueryTerms × fields), so
	// the global setting never constrains them; nothing else in the
	// binary uses bleve.
	maxTermExpansion = 1024

	// maxConcurrentSearches is the number of bleve searches that may run
	// at once across every index in the process (see acquireSearchSlot).
	// The per-query limits above bound what ONE search can cost; this
	// bounds how many of those costs can be live at the same moment, so
	// the worst case is a fixed multiple of the per-search budget rather
	// than one that grows with the number of concurrent callers. A
	// constant, not a function of GOMAXPROCS, on purpose: it bounds
	// memory, which does not grow with core count. A legitimate search
	// takes milliseconds, so 8 slots queue nothing in practice.
	maxConcurrentSearches = 8
)

// init installs maxTermExpansion as bleve's clause limit. It has to be
// the package-level variable: bleve reads it at searcher construction,
// with no per-request or per-index option. Only this package uses bleve,
// so the setting cannot surprise another caller.
func init() {
	searcher.DisjunctionMaxClauseCount = maxTermExpansion
}

// searchSlots is the semaphore behind maxConcurrentSearches.
var searchSlots = make(chan struct{}, maxConcurrentSearches)

// acquireSearchSlot blocks until a search slot is free or ctx is done.
// A caller that gets nil must call releaseSearchSlot exactly once.
func acquireSearchSlot(ctx context.Context) error {
	select {
	case searchSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseSearchSlot() { <-searchSlots }

// ErrInvalidQuery is wrapped by the error QueryContext/Query return when
// the raw query string fails the pre-parse checks below. Callers (HTTP,
// MCP) test for it with errors.Is to answer with a clean 400 / tool
// error instead of a 500 — this is a rejected input, not an internal
// failure.
var ErrInvalidQuery = errors.New("invalid query")

// validateQuery applies cheap (O(len(q)), single-pass) limits to a raw
// query string, rejecting it before it is ever handed to
// bleve.NewQueryStringQuery, and then checks the parsed query's
// multi-term clauses (validateSyntax). See the const block above for why
// bleve's parser needs this guard and how the thresholds were chosen.
func validateQuery(q string) error {
	if err := validateShape(q); err != nil {
		return err
	}
	return validateSyntax(q)
}

// validateShape is the byte-level half of validateQuery: length, term
// count and paren depth, checked before anything parses q.
func validateShape(q string) error {
	if len(q) > maxQueryBytes {
		return fmt.Errorf("%w: query is %d bytes, which exceeds the %d byte limit",
			ErrInvalidQuery, len(q), maxQueryBytes)
	}
	if n := len(strings.Fields(q)); n > maxQueryTerms {
		return fmt.Errorf("%w: query has %d whitespace-separated terms, which exceeds the %d term limit",
			ErrInvalidQuery, n, maxQueryTerms)
	}
	depth := 0
	for _, r := range q {
		switch r {
		case '(':
			depth++
			if depth > maxParenDepth {
				return fmt.Errorf("%w: query nesting depth exceeds the %d limit",
					ErrInvalidQuery, maxParenDepth)
			}
		case ')':
			if depth > 0 {
				depth--
			}
		}
	}
	return nil
}

// validateSyntax parses q with bleve's own query-string parser — the
// parser the exact stage's query-string clauses use, so the check and
// the executed query cannot disagree about what q means — and bounds
// the clauses whose cost depends on the size of the index rather than
// the size of the query:
//
//   - every wildcard and regexp clause must start with at least one
//     literal character. A leading `*`, `?` or regexp metacharacter
//     would make bleve enumerate the whole term dictionary of the field;
//     a literal prefix lets it seek straight to the matching range.
//   - at most maxMultiTermClauses wildcard/regexp clauses per query.
//
// How far each remaining clause may expand is bounded at search time by
// maxTermExpansion. A query bleve cannot parse is rejected here too, as
// ErrInvalidQuery: it would fail at search time anyway, and it is the
// caller's input, not an internal failure.
func validateSyntax(q string) error {
	parsed, err := query.NewQueryStringQuery(q).Parse()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	multi := 0
	var walk func(query.Query) error
	walk = func(n query.Query) error {
		switch n := n.(type) {
		case *query.BooleanQuery:
			for _, c := range []query.Query{n.Must, n.Should, n.MustNot} {
				if c == nil {
					continue
				}
				if err := walk(c); err != nil {
					return err
				}
			}
		case *query.ConjunctionQuery:
			for _, c := range n.Conjuncts {
				if err := walk(c); err != nil {
					return err
				}
			}
		case *query.DisjunctionQuery:
			for _, c := range n.Disjuncts {
				if err := walk(c); err != nil {
					return err
				}
			}
		case *query.WildcardQuery:
			multi++
			if wildcardLiteralPrefix(n.Wildcard) == "" {
				return fmt.Errorf("%w: wildcard %q must start with at least one literal character",
					ErrInvalidQuery, n.Wildcard)
			}
		case *query.RegexpQuery:
			multi++
			re, err := regexp.Compile(n.Regexp)
			if err != nil {
				return fmt.Errorf("%w: regexp /%s/: %v", ErrInvalidQuery, n.Regexp, err)
			}
			if prefix, _ := re.LiteralPrefix(); prefix == "" {
				return fmt.Errorf("%w: regexp /%s/ must start with at least one literal character",
					ErrInvalidQuery, n.Regexp)
			}
		}
		if multi > maxMultiTermClauses {
			return fmt.Errorf("%w: query has more than %d wildcard or regexp clauses",
				ErrInvalidQuery, maxMultiTermClauses)
		}
		return nil
	}
	return walk(parsed)
}

// wildcardLiteralPrefix is the part of a bleve wildcard before its first
// `*` or `?`: the prefix bleve can seek the term dictionary to.
func wildcardLiteralPrefix(w string) string {
	if i := strings.IndexAny(w, "*?"); i >= 0 {
		return w[:i]
	}
	return w
}

// tooBroad reports bleve's "too many clauses" failure: a wildcard,
// regexp, prefix or fuzzy clause matched more than maxTermExpansion
// dictionary terms. bleve has no sentinel error for it, only this
// message (search/searcher/search_disjunction.go, tooManyClausesErr);
// TestQuery_TooBroadExpansionIsInvalidQuery pins the match, so a bleve
// upgrade that rewords it fails a test instead of turning a 400 into a
// 500.
func tooBroad(err error) bool {
	return err != nil && strings.Contains(err.Error(), "TooManyClauses")
}

// errTooBroad is the caller-facing form of a tooBroad failure.
func errTooBroad() error {
	return fmt.Errorf("%w: a wildcard, regexp, prefix or fuzzy term matches more than %d distinct index terms; make it more specific",
		ErrInvalidQuery, maxTermExpansion)
}

// clampLimit normalises a caller-supplied result limit: non-positive
// values fall back to DefaultLimit, and values above MaxLimit are
// capped there (see MaxLimit's doc comment).
func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}
