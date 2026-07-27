package search

import (
	"errors"
	"fmt"
	"strings"
	"time"
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
)

// ErrInvalidQuery is wrapped by the error QueryContext/Query return when
// the raw query string fails the pre-parse checks below. Callers (HTTP,
// MCP) test for it with errors.Is to answer with a clean 400 / tool
// error instead of a 500 — this is a rejected input, not an internal
// failure.
var ErrInvalidQuery = errors.New("invalid query")

// validateQuery applies cheap (O(len(q)), single-pass) limits to a raw
// query string, rejecting it before it is ever handed to
// bleve.NewQueryStringQuery. See the const block above for why bleve's
// parser needs this guard and how the thresholds were chosen.
func validateQuery(q string) error {
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
