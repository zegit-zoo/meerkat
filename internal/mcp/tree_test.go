package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
)

func treeRegistry(t *testing.T) *collections.Registry {
	t.Helper()
	base := t.TempDir()
	mk := func(name, manifest string) string {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "# " + name + "\nplatform flux gitops\n"
		if name == "root" {
			body = "---\ntype: pointer\ntarget: collection:platform\nhint: Platform hub.\n---\n# Platform\nplatform flux gitops\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "wiki", name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, contentsource.ManifestFile), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	flux := mk("flux", "kind: KnowledgeBase\nname: flux\n")
	platform := mk("platform", "kind: KnowledgeBase\nname: platform\nchildren:\n  - name: flux\n    source: {type: local, path: "+flux+"}\n")
	vendors := mk("vendors", "kind: KnowledgeBase\nname: vendors\n")
	root := mk("root", "kind: KnowledgeBase\nname: root\nchildren:\n  - name: platform\n    source: {type: local, path: "+platform+"}\n  - name: vendors\n    source: {type: local, path: "+vendors+"}\n    mount: lazy\n")
	resolved, _, err := contentsource.ResolveTree(context.Background(), contentsource.Source{Type: contentsource.TypeLocal, Path: root, Layout: contentsource.MergeLayout(contentsource.Layout{})}, filepath.Join(base, "content-source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := collections.Open(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func TestListCollectionsWire_RendersTheTree(t *testing.T) {
	reg := treeRegistry(t)
	body, err := listCollectionsJSON(context.Background(), reg)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, e := range out {
		byName[e["name"].(string)] = e
	}
	if len(out) != 4 {
		t.Fatalf("entries = %d, want root, platform, flux and the cold vendors: %s", len(out), body)
	}
	root := byName["root"]
	if root["path"] != "root" || root["tier"] != float64(0) || root["mounted"] != true {
		t.Errorf("root = %v", root)
	}
	children, _ := root["children"].([]any)
	if len(children) != 2 || children[1].(map[string]any)["mounted"] != false || children[1].(map[string]any)["mount"] != "lazy" {
		t.Errorf("root children = %v", root["children"])
	}
	flux := byName["flux"]
	if flux["path"] != "root/platform/flux" || flux["parent"] != "platform" || flux["tier"] != float64(2) || flux["pages"] != float64(1) {
		t.Errorf("flux = %v", flux)
	}
	vendors := byName["vendors"]
	if vendors["mounted"] != false || vendors["source"] != "cold" || vendors["pages"] != float64(0) || vendors["path"] != "root/vendors" || vendors["mount"] != "lazy" {
		t.Errorf("cold vendors = %v", vendors)
	}
	// Listing described the cold child without mounting it.
	if c, _ := reg.Get("vendors"); !c.IsCold() {
		t.Error("listing must never mount a cold collection")
	}
}

func TestSearchOnTree_RootOnlyAndColdChildRefused(t *testing.T) {
	reg := treeRegistry(t)
	hits, err := reg.Search(context.Background(), "", "platform", 10)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := searchResultsJSON(hits)
	if !strings.Contains(body, `"collection": "root"`) || strings.Contains(body, `"collection": "flux"`) {
		t.Errorf("unqualified search must stay on the root hub: %s", body)
	}
	if !strings.Contains(body, `"target": "collection:platform"`) {
		t.Errorf("root hit must be the routing pointer: %s", body)
	}
	// Blocking cold policy: naming the cold child mounts it and answers.
	hits, err = reg.Search(context.Background(), "vendors", "vendors", 10)
	if err != nil || len(hits) == 0 {
		t.Errorf("cold child under the blocking policy: %v %v", hits, err)
	}
	if c, _ := reg.Get("vendors"); c.IsCold() {
		t.Error("vendors must be warm after the search")
	}
}

func TestSearchHandler_AsyncColdPolicyAnswersCold(t *testing.T) {
	reg := treeRegistry(t)
	reg.SetCache(&contentsource.CacheSpec{ColdPolicy: contentsource.ColdAsync, RetryAfter: 30 * time.Millisecond}, nil)
	req := mcp.CallToolRequest{}
	req.Params.Name = toolSearch
	req.Params.Arguments = map[string]any{"query": "vendors", "collection": "vendors"}
	res, err := searchHandler(reg, transportOptions{})(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a cold answer is a result, not an error: %s", res.Content[0].(mcp.TextContent).Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "cold" || out["collection"] != "vendors" || out["retry_after_ms"] != float64(30) {
		t.Errorf("cold answer = %v", out)
	}
	// The mount proceeds in the background; a retry finds it warm.
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, _ := searchHandler(reg, transportOptions{})(context.Background(), req)
		text := res.Content[0].(mcp.TextContent).Text
		var hits []map[string]any
		if json.Unmarshal([]byte(text), &hits) == nil && len(hits) > 0 && hits[0]["collection"] == "vendors" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never warmed: %s", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
