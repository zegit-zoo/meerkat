package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// collections_test.go covers the MCP surface of multi-collection
// routing: the optional "collection" tool argument, the discovery
// affordance in the tool descriptions, and the error shapes a client
// can act on.

func multiRegistry(t *testing.T) *collections.Registry {
	t.Helper()
	reg, err := collections.New(
		collections.FromPages("runbooks", []kb.Page{
			testPage("incidents/paging", "Paging", "who to page during an incident", "runbooks", "reviewed", "team-a"),
			testPage("shared/overview", "Runbook Overview", "shared overview text", "runbooks", "reviewed", "team-a"),
		}),
		collections.FromPages("architecture", []kb.Page{
			testPage("adr/0001", "ADR 1", "we chose object storage for incidents", "adr", "reviewed", "team-b"),
			testPage("shared/overview", "Architecture Overview", "shared overview text", "adr", "reviewed", "team-b"),
		}),
	)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// TestTools_DescribeMountedCollections proves the tool list doubles as
// the collection listing — a client learns the names from the schema it
// already fetches, with no extra tool to call.
func TestTools_DescribeMountedCollections(t *testing.T) {
	reg := multiRegistry(t)
	for _, tool := range []struct {
		name string
		desc string
		args map[string]any
	}{
		{"mk_search", searchTool(reg).Description, searchTool(reg).InputSchema.Properties},
		{"mk_show", showTool(reg).Description, showTool(reg).InputSchema.Properties},
		{"mk_list", listTool(reg).Description, listTool(reg).InputSchema.Properties},
	} {
		t.Run(tool.name, func(t *testing.T) {
			for _, want := range []string{"runbooks", "architecture"} {
				if !strings.Contains(tool.desc, want) {
					t.Errorf("description should name the mounted collection %q:\n%s", want, tool.desc)
				}
			}
			if _, ok := tool.args["collection"]; !ok {
				t.Errorf("tool has no 'collection' argument (properties: %v)", tool.args)
			}
		})
	}
}

// TestTools_SingleCollectionDescriptionsStayQuiet: a single-collection
// deployment (every pre-collections one) must not have its tool
// descriptions cluttered with routing rules that can never apply.
func TestTools_SingleCollectionDescriptionsStayQuiet(t *testing.T) {
	reg := collections.Global("test")
	for _, desc := range []string{searchTool(reg).Description, showTool(reg).Description, listTool(reg).Description} {
		if strings.Contains(desc, "collections (") {
			t.Errorf("single-collection description carries multi-collection prose:\n%s", desc)
		}
	}
}

func TestSearchHandler_CollectionArgumentNarrows(t *testing.T) {
	reg := multiRegistry(t)

	all, err := searchHandler(reg)(context.Background(), callTool(map[string]any{"query": "incidents"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	seen := map[string]bool{}
	for _, hit := range parseHits(t, resultText(t, all)) {
		seen[hit["collection"].(string)] = true
	}
	if !seen["runbooks"] || !seen["architecture"] {
		t.Errorf("omitting collection should span all, reached %v", seen)
	}

	one, err := searchHandler(reg)(context.Background(), callTool(map[string]any{"query": "incidents", "collection": "runbooks"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	for _, hit := range parseHits(t, resultText(t, one)) {
		if hit["collection"] != "runbooks" {
			t.Errorf("collection=runbooks leaked a %v hit", hit["collection"])
		}
	}
}

func TestSearchHandler_UnknownCollectionIsToolError(t *testing.T) {
	res, err := searchHandler(multiRegistry(t))(context.Background(),
		callTool(map[string]any{"query": "x", "collection": "nope"}))
	if err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected a tool-level error for an unknown collection")
	}
	if !strings.Contains(resultText(t, res), "runbooks") {
		t.Errorf("error should list the available collections: %s", resultText(t, res))
	}
}

func TestListHandler_CollectionArgumentNarrows(t *testing.T) {
	reg := multiRegistry(t)
	res, err := listHandler(reg)(context.Background(), callTool(map[string]any{"collection": "architecture"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	entries := parseHits(t, resultText(t, res))
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want architecture's 2", len(entries))
	}
	for _, e := range entries {
		if e["collection"] != "architecture" {
			t.Errorf("leaked a %v entry", e["collection"])
		}
	}
}

// TestShowHandler_AmbiguousIsAnActionableToolError: mk_show must hand
// the model something it can retry with, not a transport failure.
func TestShowHandler_AmbiguousIsAnActionableToolError(t *testing.T) {
	reg := multiRegistry(t)
	res, err := showHandler(reg)(context.Background(), callTool(map[string]any{"id": "shared/overview"}))
	if err != nil {
		t.Fatalf("handler returned a transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected a tool-level error for an ambiguous page id")
	}
	text := resultText(t, res)
	for _, want := range []string{"runbooks:shared/overview", "architecture:shared/overview"} {
		if !strings.Contains(text, want) {
			t.Errorf("error should offer %q: %s", want, text)
		}
	}
}

func TestShowHandler_QualifiedIDAndCollectionArgumentBothResolve(t *testing.T) {
	reg := multiRegistry(t)

	byQualified, err := showHandler(reg)(context.Background(), callTool(map[string]any{"id": "architecture:shared/overview"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := parsePage(t, resultText(t, byQualified)); got["collection"] != "architecture" || got["id"] != "shared/overview" {
		t.Errorf("qualified id resolved to %v", got)
	}

	byArg, err := showHandler(reg)(context.Background(), callTool(map[string]any{"id": "shared/overview", "collection": "runbooks"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := parsePage(t, resultText(t, byArg)); got["collection"] != "runbooks" {
		t.Errorf("collection argument resolved to %v", got)
	}
}

func parseHits(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	return out
}

func parsePage(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, body)
	}
	return out
}
