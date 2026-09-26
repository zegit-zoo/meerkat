package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
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

// describedRegistry mounts three collections in the shape a real
// content-source.yaml produces: two with an operator `description:`, one
// with none (every configuration that predates the field). The described
// pair carries an `update:` block as well, so a test can pin that tool
// discovery renders the description and NOT the contribution repo.
func describedRegistry(t *testing.T) *collections.Registry {
	t.Helper()
	internal := collections.FromPages("internal", []kb.Page{
		testPage("systems/paging", "Paging", "who to page during an incident", "systems", "reviewed", "team-a"),
	})
	internal.Source.Description = "Swish systems and operations"
	internal.Source.Update = &contentsource.UpdateSpec{
		Method:       contentsource.UpdateMergeRequest,
		Repo:         "https://github.com/example-org/handbook.git",
		Host:         contentsource.UpdateHostGitHub,
		Instructions: "branch from main and open a merge request",
	}
	cra := collections.FromPages("cra", []kb.Page{
		testPage("cra/overview", "CRA Overview", "what the regulation requires", "policies", "reviewed", "team-b"),
	})
	cra.Source.Description = "EU Cyber Resilience Act guidance"
	scratch := collections.FromPages("scratch", []kb.Page{
		testPage("notes/idea", "Idea", "an unfinished thought", "notes", "placeholder", "team-c"),
	})
	reg, err := collections.New(internal, cra, scratch)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// TestTools_DescribeCollectionDescriptions is the acceptance test for
// #84: tools/list has to say what each collection CONTAINS, not only
// what it is called, because the model's first routing decision is made
// from the tool list alone — before mk_list_collections has been called.
//
// Both renderings are checked, because they are the reason the formatter
// is shared: the tool prose and the "collection" argument's description
// must carry the same list.
func TestTools_DescribeCollectionDescriptions(t *testing.T) {
	reg := describedRegistry(t)
	const (
		wantInternal = "internal — Swish systems and operations"
		wantCRA      = "cra — EU Cyber Resilience Act guidance"
	)
	for _, tool := range []struct {
		name string
		tool mcp.Tool
	}{
		{"mk_search", searchTool(reg)},
		{"mk_show", showTool(reg)},
		{"mk_list", listTool(reg)},
	} {
		t.Run(tool.name, func(t *testing.T) {
			arg := collectionArgDescription(t, tool.tool)
			for surface, text := range map[string]string{
				"tool description":    tool.tool.Description,
				"collection argument": arg,
			} {
				for _, want := range []string{wantInternal, wantCRA} {
					if !strings.Contains(text, want) {
						t.Errorf("%s should carry %q:\n%s", surface, want, text)
					}
				}
				// The ";"-separated form, so the pair reads as a list.
				if !strings.Contains(text, wantInternal+"; "+wantCRA) {
					t.Errorf("%s should separate the entries with '; ':\n%s", surface, text)
				}
				// mk_list_collections stays the detailed surface: the
				// contribution repo and its instructions are per-caller and
				// several fields long, and have no business in discovery.
				for _, leak := range []string{"github.com/example-org/handbook.git", "merge request", "merge-request"} {
					if strings.Contains(text, leak) {
						t.Errorf("%s leaks the update contract (%q):\n%s", surface, leak, text)
					}
				}
			}
		})
	}
}

// TestTools_CollectionWithoutADescriptionStaysNameOnly pins the
// back-compatible half: `description:` is optional, and a collection
// that has none is still named exactly as it was before #84 — no em
// dash, no empty gap, nothing invented on the operator's behalf.
func TestTools_CollectionWithoutADescriptionStaysNameOnly(t *testing.T) {
	reg := describedRegistry(t)
	for _, text := range []string{listTool(reg).Description, collectionArgDescription(t, listTool(reg))} {
		if !strings.Contains(text, "scratch") {
			t.Fatalf("the undescribed collection is missing entirely:\n%s", text)
		}
		if strings.Contains(text, "scratch —") {
			t.Errorf("an undescribed collection got a dangling em dash:\n%s", text)
		}
	}
	// And a registry in which nobody configured a description is
	// byte-identical to the pre-#84 rendering.
	if got, want := collectionList(multiRegistry(t)), "runbooks; architecture"; got != want {
		t.Errorf("collectionList without descriptions = %q, want %q", got, want)
	}
}

// TestTools_SingleCollectionDescriptionStaysAShortPhrase: a
// single-collection deployment gets the description too — it is the one
// thing that says what the server is FOR — but as a phrase inside the
// sentence it already had, never as a list header with routing rules
// that can never apply.
func TestTools_SingleCollectionDescriptionStaysAShortPhrase(t *testing.T) {
	only := collections.FromPages("handbook", []kb.Page{
		testPage("handbook/onboarding", "Onboarding", "how we onboard", "handbook", "reviewed", "team-a"),
	})
	only.Source.Description = "How Swish works, for people and agents"
	reg, err := collections.New(only)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	arg := collectionArgDescription(t, searchTool(reg))
	if want := "mounts a single collection (handbook — How Swish works, for people and agents)"; !strings.Contains(arg, want) {
		t.Errorf("collection argument = %q, want it to contain %q", arg, want)
	}
	if strings.Contains(arg, "Mounted collections:") || strings.Contains(arg, ";") {
		t.Errorf("single-collection wording became a list:\n%s", arg)
	}
	if s := collectionSuffix(reg); s != "" {
		t.Errorf("collectionSuffix on a single-collection server = %q, want empty", s)
	}
	if strings.Contains(searchTool(reg).Description, "collections (") {
		t.Errorf("single-collection tool prose carries multi-collection routing rules:\n%s", searchTool(reg).Description)
	}
}

// TestCollectionList_NormalisesConfiguredProse: a `description:` is
// usually a YAML block scalar, so it arrives with newlines and
// indentation, and operators end sentences with a full stop. Neither may
// reach the rendered list — one would break the tool description into
// ragged lines, the other would collide with our own punctuation.
func TestCollectionList_NormalisesConfiguredProse(t *testing.T) {
	col := collections.FromPages("kb", []kb.Page{
		testPage("a/b", "B", "body", "cat", "reviewed", "team-a"),
	})
	col.Source.Description = "  Two lines of\n  operator prose.  "
	reg, err := collections.New(col)
	if err != nil {
		t.Fatalf("collections.New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if got, want := collectionList(reg), "kb — Two lines of operator prose"; got != want {
		t.Errorf("collectionList = %q, want %q", got, want)
	}
}

// collectionArgDescription returns the description of a tool's
// "collection" argument — the other half of the discovery surface,
// which lives in the input schema rather than the prose.
func collectionArgDescription(t *testing.T, tool mcp.Tool) string {
	t.Helper()
	prop, ok := tool.InputSchema.Properties["collection"]
	if !ok {
		t.Fatalf("tool %q has no 'collection' argument (properties: %v)", tool.Name, tool.InputSchema.Properties)
	}
	spec, ok := prop.(map[string]any)
	if !ok {
		t.Fatalf("tool %q's 'collection' argument is %T, want a JSON-schema object", tool.Name, prop)
	}
	desc, _ := spec["description"].(string)
	if desc == "" {
		t.Fatalf("tool %q's 'collection' argument has no description: %v", tool.Name, spec)
	}
	return desc
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

	all, err := searchHandler(reg, stdioTransport())(context.Background(), callTool(map[string]any{"query": "incidents"}))
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

	one, err := searchHandler(reg, stdioTransport())(context.Background(), callTool(map[string]any{"query": "incidents", "collection": "runbooks"}))
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
	res, err := searchHandler(multiRegistry(t), stdioTransport())(context.Background(),
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
	res, err := listHandler(reg, stdioTransport())(context.Background(), callTool(map[string]any{"collection": "architecture"}))
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
	res, err := showHandler(reg, stdioTransport())(context.Background(), callTool(map[string]any{"id": "shared/overview"}))
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

	byQualified, err := showHandler(reg, stdioTransport())(context.Background(), callTool(map[string]any{"id": "architecture:shared/overview"}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := parsePage(t, resultText(t, byQualified)); got["collection"] != "architecture" || got["id"] != "shared/overview" {
		t.Errorf("qualified id resolved to %v", got)
	}

	byArg, err := showHandler(reg, stdioTransport())(context.Background(), callTool(map[string]any{"id": "shared/overview", "collection": "runbooks"}))
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
