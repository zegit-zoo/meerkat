package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if vendors["mounted"] != false || vendors["source"] != "unmounted" || vendors["pages"] != float64(0) || vendors["path"] != "root/vendors" {
		t.Errorf("cold vendors = %v", vendors)
	}
	caps, _ := vendors["capabilities"].([]any)
	if len(caps) != 0 {
		t.Errorf("a cold entry must carry no capabilities: %v", caps)
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
	_, err = reg.Search(context.Background(), "vendors", "x", 10)
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("cold child: %v", err)
	}
}
