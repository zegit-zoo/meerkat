package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

func testPage(id, title, body, category, status, owner string) kb.Page {
	return kb.Page{
		ID:    id,
		Title: title,
		Body:  body,
		Front: kb.Frontmatter{ID: id, Title: title, Category: category, Status: status, Owner: owner},
	}
}

// testPageWithType extends testPage with a Type value for the ByType /
// --type filter tests, without changing testPage's signature (used
// above by many pre-existing tests).
func testPageWithType(id, title, body, category, status, owner, typ string) kb.Page {
	p := testPage(id, title, body, category, status, owner)
	p.Front.Type = typ
	return p
}

func callTool(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

// testRegistry mounts pages as a single collection named "default" —
// the one-collection shape every pre-collections deployment resolves
// to, so these tests keep asserting the same behaviour they always did.
func testRegistry(t *testing.T, pages ...kb.Page) *collections.Registry {
	t.Helper()
	reg, err := collections.New(collections.FromPages(collections.DefaultName, pages))
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// testGlobalRegistry mounts the process-global kb filesystem (whatever
// kb.UseFS currently points at) as the single "default" collection.
func testGlobalRegistry() *collections.Registry {
	return collections.Global("test")
}

func TestSearchHandler_ReturnsHits(t *testing.T) {
	reg := testRegistry(t,
		testPage("concepts/quorum", "Quorum", "A write needs a quorum of replicas.", "concepts", "reviewed", "team-a"),
		testPage("concepts/sharding", "Sharding", "Split data across shards.", "concepts", "reviewed", "team-a"),
	)

	res, err := searchHandler(reg)(context.Background(), callTool(map[string]any{"query": "quorum"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := resultText(t, res)
	if !strings.Contains(text, "concepts/quorum") {
		t.Errorf("expected quorum page in results, got: %s", text)
	}
	// Result must be valid JSON with the documented fields.
	var hits []map[string]any
	if err := json.Unmarshal([]byte(text), &hits); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	for _, k := range []string{"id", "collection", "title", "category", "status", "score", "snippet"} {
		if _, ok := hits[0][k]; !ok {
			t.Errorf("hit missing field %q", k)
		}
	}
}

// TestSearchHandler_QueryTooLongIsToolError proves an oversized query
// comes back as a normal tool-level error (IsError, err == nil), not a
// transport-level Go error, matching TestSearchHandler_MissingQueryIsToolError's
// existing contract for the missing-query case.
func TestSearchHandler_QueryTooLongIsToolError(t *testing.T) {
	reg := testRegistry(t)

	huge := strings.Repeat("x", 5000)
	res, err := searchHandler(reg)(context.Background(), callTool(map[string]any{"query": huge}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected a tool-level error result for an oversized query")
	}
}

// TestSearchHandler_PathologicalNestingIsToolError mirrors the
// confirmed attack shape (a long run of nested parens) at a size that's
// cheap to run in a test.
func TestSearchHandler_PathologicalNestingIsToolError(t *testing.T) {
	reg := testRegistry(t)

	nested := strings.Repeat("(", 100)
	res, err := searchHandler(reg)(context.Background(), callTool(map[string]any{"query": nested}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected a tool-level error result for a pathologically nested query")
	}
}

// TestSearchHandler_ContextCancelledIsToolError proves the handler
// threads its ctx into the search and surfaces cancellation as a clean
// tool error rather than hanging or returning a transport error.
func TestSearchHandler_ContextCancelledIsToolError(t *testing.T) {
	reg := testRegistry(t,
		testPage("concepts/quorum", "Quorum", "A write needs a quorum of replicas.", "concepts", "reviewed", "team-a"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := searchHandler(reg)(ctx, callTool(map[string]any{"query": "quorum"}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected a tool-level error result for a cancelled context")
	}
}

// TestSearchHandler_LimitClampedToMax proves the mk_search tool applies
// the same MaxLimit cap as the HTTP /search endpoint: a client-supplied
// limit far above MaxLimit must still come back capped, not passed
// straight through to bleve.
func TestSearchHandler_LimitClampedToMax(t *testing.T) {
	pages := make([]kb.Page, search.MaxLimit+50)
	for i := range pages {
		pages[i] = testPage(
			fmt.Sprintf("fixture/%d", i), fmt.Sprintf("Widget %d", i),
			"widget widget shared searchable term", "concepts", "reviewed", "team-a",
		)
	}
	reg := testRegistry(t, pages...)

	res, err := searchHandler(reg)(context.Background(), callTool(map[string]any{"query": "widget", "limit": float64(100000)}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &hits); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if len(hits) != search.MaxLimit {
		t.Errorf("got %d hits for limit=100000 against %d matching pages, want exactly MaxLimit=%d",
			len(hits), len(pages), search.MaxLimit)
	}
}

func TestSearchHandler_MissingQueryIsToolError(t *testing.T) {
	reg := testRegistry(t)
	res, err := searchHandler(reg)(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected a tool-level error result for a missing query")
	}
}

func TestSearchResultsJSON_Shape(t *testing.T) {
	results := []collections.Hit{
		{Collection: "default", Result: search.Result{Page: testPage("a/b", "Title", "body", "concepts", "reviewed", "o"), Score: 1.5, Snippet: "a  snippet   here"}},
	}
	out, err := searchResultsJSON(results)
	if err != nil {
		t.Fatalf("searchResultsJSON: %v", err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed[0]["snippet"] != "a snippet here" {
		t.Errorf("snippet should be collapsed to one line, got %q", parsed[0]["snippet"])
	}
}

func TestFilterPages_ComposesAND(t *testing.T) {
	refs := []collections.PageRef{
		{Collection: "default", Page: testPageWithType("systems/backend/api", "API", "", "systems", "reviewed", "team-a", "API Endpoint")},
		{Collection: "default", Page: testPageWithType("systems/backend/db", "DB", "", "systems", "placeholder", "team-a", "")},
		{Collection: "default", Page: testPageWithType("concepts/quorum", "Quorum", "", "concepts", "reviewed", "team-b", "")},
		{Collection: "default", Page: testPageWithType("tables/orders", "Orders", "", "tables", "stable", "team-b", "BigQuery Table")},
	}
	cases := []struct {
		name                                 string
		prefix, category, status, owner, typ string
		wantIDs                              []string
	}{
		{"no filters", "", "", "", "", "", []string{"systems/backend/api", "systems/backend/db", "concepts/quorum", "tables/orders"}},
		{"prefix", "systems/backend/", "", "", "", "", []string{"systems/backend/api", "systems/backend/db"}},
		{"category", "", "concepts", "", "", "", []string{"concepts/quorum"}},
		{"status", "", "", "reviewed", "", "", []string{"systems/backend/api", "concepts/quorum"}},
		{"owner", "", "", "", "team-b", "", []string{"concepts/quorum", "tables/orders"}},
		{"type", "", "", "", "", "BigQuery Table", []string{"tables/orders"}},
		{"prefix+status (AND)", "systems/", "", "reviewed", "", "", []string{"systems/backend/api"}},
		{"owner+type (AND)", "", "", "", "team-b", "BigQuery Table", []string{"tables/orders"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterPages(refs, tc.prefix, tc.category, tc.status, tc.owner, tc.typ)
			var ids []string
			for _, r := range got {
				ids = append(ids, r.Page.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("filterPages = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

func TestListPagesJSON_Shape(t *testing.T) {
	refs := []collections.PageRef{{Collection: "default", Page: testPageWithType("concepts/x", "X", "", "concepts", "reviewed", "owner-1", "Metric")}}
	out, err := listPagesJSON(refs)
	if err != nil {
		t.Fatalf("listPagesJSON: %v", err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed[0]["id"] != "concepts/x" || parsed[0]["owner"] != "owner-1" {
		t.Errorf("unexpected list shape: %v", parsed[0])
	}
	if parsed[0]["collection"] != "default" {
		t.Errorf("collection = %v, want default", parsed[0]["collection"])
	}
	if parsed[0]["type"] != "Metric" {
		t.Errorf("type = %v, want Metric", parsed[0]["type"])
	}
}

// TestListHandler_TypeFilter drives the full mk_list handler (not just
// filterPages) to prove the "type" tool argument is wired end to end,
// against injected KB content rather than testPage fixtures.
func TestListHandler_TypeFilter(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md":    {Data: []byte("---\nid: tables/orders\ntitle: Orders\ntype: BigQuery Table\n---\nbody\n")},
		"content/playbooks/oncall.md": {Data: []byte("---\nid: playbooks/oncall\ntitle: Oncall\ntype: Playbook\n---\nbody\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })

	res, err := listHandler(testGlobalRegistry())(context.Background(), callTool(map[string]any{"type": "BigQuery Table"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(parsed) != 1 || parsed[0]["id"] != "tables/orders" {
		t.Errorf("mk_list type=BigQuery Table = %v, want exactly [tables/orders]", parsed)
	}
}

func TestListPagesJSON_EmptyIsArrayNotNull(t *testing.T) {
	out, err := listPagesJSON(nil)
	if err != nil {
		t.Fatalf("listPagesJSON: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty list should render as [], got %q", out)
	}
}

// TestShowPageJSON_Shape proves the mk_show wire shape carries the
// page's own fields (id, front.type, front.description, ...) alongside
// the two OKF-derived advisory signals (trust_tier, stale) that aren't
// stored fields on kb.Page.
func TestShowPageJSON_Shape(t *testing.T) {
	page := kb.Page{
		ID:    "tables/orders",
		Title: "Orders",
		Body:  "body",
		Front: kb.Frontmatter{
			ID:          "tables/orders",
			Type:        "BigQuery Table",
			Description: "One row per order.",
			StaleAfter:  "2020-01-01",
			Verified:    kb.VerifiedList{{By: "human:ahormati"}},
		},
	}
	out, err := showPageJSON(collections.PageRef{Collection: "default", Page: page})
	if err != nil {
		t.Fatalf("showPageJSON: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if parsed["id"] != "tables/orders" {
		t.Errorf("id = %v", parsed["id"])
	}
	if parsed["trust_tier"] != kb.TrustHumanReviewed {
		t.Errorf("trust_tier = %v, want %q", parsed["trust_tier"], kb.TrustHumanReviewed)
	}
	if parsed["stale"] != true {
		t.Errorf("stale = %v, want true", parsed["stale"])
	}
	front, ok := parsed["front"].(map[string]any)
	if !ok {
		t.Fatalf("front is not an object: %v", parsed["front"])
	}
	if front["type"] != "BigQuery Table" {
		t.Errorf("front.type = %v", front["type"])
	}
	if front["description"] != "One row per order." {
		t.Errorf("front.description = %v", front["description"])
	}
}

// TestShowHandler_ReturnsHitWithTrustTier drives the full mk_show
// handler end to end against injected KB content.
func TestShowHandler_ReturnsHitWithTrustTier(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md": {Data: []byte(`---
id: tables/orders
type: BigQuery Table
verified: { by: process:finance-nightly, at: 2026-06-26T02:00:00Z }
---
body
`)},
	})
	t.Cleanup(func() { kb.UseFS(nil) })

	res, err := showHandler(testGlobalRegistry())(context.Background(), callTool(map[string]any{"id": "tables/orders"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["trust_tier"] != kb.TrustMachineConfirmed {
		t.Errorf("trust_tier = %v, want %q", parsed["trust_tier"], kb.TrustMachineConfirmed)
	}
}

// TestShowHandler_NotFoundIsToolError proves a missing page still comes
// back as a clean tool-level error, unaffected by the trust_tier/stale
// augmentation.
func TestShowHandler_NotFoundIsToolError(t *testing.T) {
	kb.UseFS(fstest.MapFS{})
	t.Cleanup(func() { kb.UseFS(nil) })

	res, err := showHandler(testGlobalRegistry())(context.Background(), callTool(map[string]any{"id": "does-not-exist"}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected a tool-level error result for a missing page")
	}
}

// TestListCollectionsHandler_Shape drives the full mk_list_collections
// handler against a multi-collection registry with no grants installed
// (the stdio / unauthenticated-hosted shape), proving the documented
// wire shape: {name, type, source, pages, capabilities}.
func TestListCollectionsHandler_Shape(t *testing.T) {
	reg, err := collections.New(
		collections.FromPages("runbooks", []kb.Page{
			testPage("incidents/paging", "Paging", "body", "concepts", "reviewed", "team-a"),
		}),
		collections.FromPages("architecture", []kb.Page{
			testPage("adr/0001", "ADR 1", "body", "adr", "reviewed", "team-b"),
			testPage("adr/0002", "ADR 2", "body", "adr", "reviewed", "team-b"),
		}),
	)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	res, err := listCollectionsHandler(reg)(context.Background(), callTool(nil))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &entries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, field := range []string{"name", "type", "source", "pages", "capabilities"} {
		if _, ok := entries[0][field]; !ok {
			t.Errorf("entry missing field %q: %v", field, entries[0])
		}
	}
	if entries[0]["name"] != "runbooks" || entries[1]["name"] != "architecture" {
		t.Errorf("order = %v/%v, want runbooks then architecture (configuration order)", entries[0]["name"], entries[1]["name"])
	}
	// FromPages sets no contentsource.Source, so Type() falls back to
	// "none" — the same default `mk list --collections` and GET
	// /collections use.
	if entries[0]["type"] != "none" {
		t.Errorf("type = %v, want %q", entries[0]["type"], "none")
	}
	if n, _ := entries[1]["pages"].(float64); int(n) != 2 {
		t.Errorf("pages = %v, want 2", entries[1]["pages"])
	}
	// No grants in context (stdio, or an unauthenticated hosted server):
	// authz.Grants.Capabilities' nil receiver reports the full capability
	// set — there is no identity to restrict against.
	wantCaps := []string{"read", "personal-write", "team-write", "global-write", "admin"}
	for _, e := range entries {
		gotRaw, _ := e["capabilities"].([]any)
		got := make([]string, len(gotRaw))
		for i, c := range gotRaw {
			got[i] = c.(string)
		}
		if strings.Join(got, ",") != strings.Join(wantCaps, ",") {
			t.Errorf("%v: capabilities = %v, want the full set %v", e["name"], got, wantCaps)
		}
	}
}

// TestListCollectionsJSON_EmptyViewIsArrayNotNull mirrors
// TestListPagesJSON_EmptyIsArrayNotNull: an empty registry view (every
// collection restricted away) must render as [], not null.
func TestListCollectionsJSON_EmptyViewIsArrayNotNull(t *testing.T) {
	reg := testRegistry(t, testPage("a/b", "A", "body", "concepts", "reviewed", "team-a"))
	empty := reg.Restrict(func(string) bool { return false })
	out, err := listCollectionsJSON(context.Background(), empty)
	if err != nil {
		t.Fatalf("listCollectionsJSON: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty view should render as [], got %q", out)
	}
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatal("nil result")
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}
