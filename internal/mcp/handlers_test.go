package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

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

func callTool(args map[string]any) mcp.CallToolRequest {
	var req mcp.CallToolRequest
	req.Params.Arguments = args
	return req
}

func TestSearchHandler_ReturnsHits(t *testing.T) {
	idx, err := search.NewFromPages([]kb.Page{
		testPage("concepts/quorum", "Quorum", "A write needs a quorum of replicas.", "concepts", "reviewed", "team-a"),
		testPage("concepts/sharding", "Sharding", "Split data across shards.", "concepts", "reviewed", "team-a"),
	})
	if err != nil {
		t.Fatalf("NewFromPages: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	res, err := searchHandler(idx)(context.Background(), callTool(map[string]any{"query": "quorum"}))
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
	for _, k := range []string{"id", "title", "category", "status", "score", "snippet"} {
		if _, ok := hits[0][k]; !ok {
			t.Errorf("hit missing field %q", k)
		}
	}
}

func TestSearchHandler_MissingQueryIsToolError(t *testing.T) {
	idx, _ := search.NewFromPages(nil)
	t.Cleanup(func() { _ = idx.Close() })
	res, err := searchHandler(idx)(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("handler returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Errorf("expected a tool-level error result for a missing query")
	}
}

func TestSearchResultsJSON_Shape(t *testing.T) {
	results := []search.Result{
		{Page: testPage("a/b", "Title", "body", "concepts", "reviewed", "o"), Score: 1.5, Snippet: "a  snippet   here"},
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
	pages := []kb.Page{
		testPage("systems/backend/api", "API", "", "systems", "reviewed", "team-a"),
		testPage("systems/backend/db", "DB", "", "systems", "placeholder", "team-a"),
		testPage("concepts/quorum", "Quorum", "", "concepts", "reviewed", "team-b"),
	}
	cases := []struct {
		name                            string
		prefix, category, status, owner string
		wantIDs                         []string
	}{
		{"no filters", "", "", "", "", []string{"systems/backend/api", "systems/backend/db", "concepts/quorum"}},
		{"prefix", "systems/backend/", "", "", "", []string{"systems/backend/api", "systems/backend/db"}},
		{"category", "", "concepts", "", "", []string{"concepts/quorum"}},
		{"status", "", "", "reviewed", "", []string{"systems/backend/api", "concepts/quorum"}},
		{"owner", "", "", "", "team-b", []string{"concepts/quorum"}},
		{"prefix+status (AND)", "systems/", "", "reviewed", "", []string{"systems/backend/api"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterPages(pages, tc.prefix, tc.category, tc.status, tc.owner)
			var ids []string
			for _, p := range got {
				ids = append(ids, p.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("filterPages = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

func TestListPagesJSON_Shape(t *testing.T) {
	pages := []kb.Page{testPage("concepts/x", "X", "", "concepts", "reviewed", "owner-1")}
	out, err := listPagesJSON(pages)
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
