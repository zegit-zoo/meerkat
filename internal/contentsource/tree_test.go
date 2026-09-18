package contentsource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kbDir writes a knowledge base directory: a manifest plus one wiki page,
// and returns its path. children are manifest child entries as YAML.
func kbDir(t *testing.T, base, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wiki", name+".md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func child(name, dir, mount string) string {
	s := "  - name: " + name + "\n    source: {type: local, path: " + dir + "}\n"
	if mount != "" {
		s += "    mount: " + mount + "\n"
	}
	return s
}

// threeTier builds root -> platform -> flux (eager) with a lazy sibling
// "vendors" under root, and returns the root dir.
func threeTier(t *testing.T, base string) string {
	t.Helper()
	flux := kbDir(t, base, "flux", "kind: KnowledgeBase\nname: flux\ntier: 2\nparent: platform\nstale_after: 2026-12-31\n")
	platform := kbDir(t, base, "platform", "kind: KnowledgeBase\nname: platform\ntier: 1\nparent: root\ndescription: Platform hub\nchildren:\n"+child("flux", flux, ""))
	vendors := kbDir(t, base, "vendors", "kind: KnowledgeBase\nname: vendors\n")
	root := kbDir(t, base, "root", "kind: KnowledgeBase\nname: root\ntier: 0\nlimits: {max_hops: 6}\nchildren:\n"+child("platform", platform, "eager")+child("vendors", vendors, "lazy"))
	return root
}

func TestResolveTree_ThreeTierLoads(t *testing.T) {
	base := t.TempDir()
	root := threeTier(t, base)
	cols, tree, err := ResolveTree(context.Background(), Source{Type: TypeLocal, Path: root, Layout: defaultLayout()}, filepath.Join(base, ConfigFile))
	if err != nil {
		t.Fatalf("ResolveTree: %v", err)
	}
	var names []string
	for _, c := range cols {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "root,platform,flux" {
		t.Errorf("mounted = %v, want root,platform,flux (depth-first, manifest order)", names)
	}
	if tree.Root != "root" || tree.MaxDepth != 2 || tree.Limits.MaxHops != 6 || tree.Limits.MaxSteps != DefaultLimits.MaxSteps {
		t.Errorf("tree = root %q depth %d limits %+v", tree.Root, tree.MaxDepth, tree.Limits)
	}
	flux := tree.Nodes["flux"]
	if flux == nil || flux.Path != "root/platform/flux" || flux.Depth != 2 || flux.Parent != "platform" || !flux.Mounted || flux.StaleAfter != "2026-12-31" {
		t.Errorf("flux node = %+v", flux)
	}
	vendors := tree.Nodes["vendors"]
	if vendors == nil || vendors.Mounted || vendors.Mount != MountLazy || vendors.Depth != 1 || vendors.Path != "root/vendors" {
		t.Errorf("lazy vendors node = %+v", vendors)
	}
	rootNode := tree.Nodes["root"]
	if len(rootNode.Children) != 2 || !rootNode.Children[0].Mounted || rootNode.Children[1].Mounted || rootNode.Children[1].Mount != MountLazy {
		t.Errorf("root children = %+v", rootNode.Children)
	}
	if n, ok := tree.ByPath("root/platform"); !ok || n.Name != "platform" || n.Description != "Platform hub" {
		t.Errorf("ByPath = %+v %v", n, ok)
	}
	if cols[1].Tree != tree.Nodes["platform"] || cols[1].Source.Description != "Platform hub" {
		t.Errorf("resolved platform carries its node and description: %+v", cols[1].Source.Description)
	}
}

func TestResolveTree_DepthGuard(t *testing.T) {
	base := t.TempDir()
	// Six tiers: d5 is at depth 5 (allowed), d6 at depth 6 (refused).
	prev := kbDir(t, base, "d6", "kind: KnowledgeBase\nname: d6\n")
	for i := 5; i >= 1; i-- {
		name := "d" + string(rune('0'+i))
		prev = kbDir(t, base, name, "kind: KnowledgeBase\nname: "+name+"\nchildren:\n"+child("d"+string(rune('0'+i+1)), prev, ""))
	}
	root := kbDir(t, base, "root", "kind: KnowledgeBase\nname: root\nchildren:\n"+child("d1", prev, ""))
	_, _, err := ResolveTree(context.Background(), Source{Type: TypeLocal, Path: root, Layout: defaultLayout()}, filepath.Join(base, ConfigFile))
	if err == nil || !strings.Contains(err.Error(), "over the hard cap of 5") {
		t.Fatalf("six tiers: err = %v, want the depth cap", err)
	}
	// Five tiers under the root (root=0 … d5=5) is the deepest allowed.
	os.WriteFile(filepath.Join(base, "d5", ManifestFile), []byte("kind: KnowledgeBase\nname: d5\n"), 0o644)
	_, tree, err := ResolveTree(context.Background(), Source{Type: TypeLocal, Path: root, Layout: defaultLayout()}, filepath.Join(base, ConfigFile))
	if err != nil || tree.MaxDepth != 5 {
		t.Fatalf("five tiers: err=%v depth=%v", err, tree)
	}
	// depth_limit on an ancestor lowers the cap for its subtree.
	os.WriteFile(filepath.Join(base, "d3", ManifestFile), []byte("kind: KnowledgeBase\nname: d3\ndepth_limit: 1\nchildren:\n"+child("d4", filepath.Join(base, "d4"), "")), 0o644)
	_, _, err = ResolveTree(context.Background(), Source{Type: TypeLocal, Path: root, Layout: defaultLayout()}, filepath.Join(base, ConfigFile))
	if err == nil || !strings.Contains(err.Error(), "depth_limit") {
		t.Errorf("depth_limit: err = %v", err)
	}
}

func TestResolveTree_CyclesDuplicatesAndMismatches(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	cases := []struct {
		name string
		root string
		want string
	}{
		{"cycle", "kind: KnowledgeBase\nname: root\nchildren:\n" + child("again", rootDir, ""), "cycle or a duplicate"},
		{"duplicate name", "kind: KnowledgeBase\nname: root\nchildren:\n" + child("root", kbDir(t, base, "other", "kind: KnowledgeBase\nname: root\n"), ""), "declared twice"},
		{"child name disagrees with manifest", "kind: KnowledgeBase\nname: root\nchildren:\n" + child("alias", kbDir(t, base, "real", "kind: KnowledgeBase\nname: real\n"), ""), "must agree"},
		{"parent mismatch", "kind: KnowledgeBase\nname: root\nchildren:\n" + child("orphan", kbDir(t, base, "orphan", "kind: KnowledgeBase\nname: orphan\nparent: elsewhere\n"), ""), "parent:"},
		{"tier mismatch", "kind: KnowledgeBase\nname: root\nchildren:\n" + child("deep", kbDir(t, base, "deep", "kind: KnowledgeBase\nname: deep\ntier: 4\n"), ""), "tier: 4"},
		{"missing manifest", "kind: KnowledgeBase\nname: root\nchildren:\n" + child("bare", kbDir(t, base, "bare", ""), ""), "has no manifest.yaml"},
		{"bad kind", "kind: Something\nname: root\n", "kind must be"},
		{"bad child mount", "kind: KnowledgeBase\nname: root\nchildren:\n  - name: x\n    source: {type: local, path: /tmp/x}\n    mount: sometimes\n", "mount must be"},
		{"unknown field", "kind: KnowledgeBase\nname: root\ncolour: blue\n", "field colour not found"},
		{"bad placement", "kind: KnowledgeBase\nname: root\nplacement: floating\n", "placement must be"},
		{"bad date", "kind: KnowledgeBase\nname: root\nstale_after: tomorrow\n", "ISO date"},
	}
	for _, c := range cases {
		kbDir(t, base, "root", c.root)
		_, _, err := ResolveTree(context.Background(), Source{Type: TypeLocal, Path: rootDir, Layout: defaultLayout()}, filepath.Join(base, ConfigFile))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestParseConfig_TreeIsExclusive(t *testing.T) {
	if _, err := parseConfig([]byte("tree: {type: local, path: kb}\ncollections:\n  - name: a\n    type: local\n    path: x\n"), "t.yaml"); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Errorf("tree + collections: %v", err)
	}
	if _, err := parseConfig([]byte("tree: {type: local, path: kb}\ncontent: {type: local, path: x}\n"), "t.yaml"); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Errorf("tree + content: %v", err)
	}
	if _, err := parseConfig([]byte("tree: {path: kb}\n"), "t.yaml"); err == nil || !strings.Contains(err.Error(), "tree.type is required") {
		t.Errorf("tree without type: %v", err)
	}
	cfg, err := parseConfig([]byte("tree: {type: s3, bucket: kb, prefix: kb/root/, endpoint: https://s3.example.net, region: garage, path_style: true}\n"), "t.yaml")
	if err != nil || cfg.Tree == nil || cfg.Tree.Layout.Wiki != "wiki" {
		t.Errorf("tree config = %+v %v", cfg.Tree, err)
	}
}

func TestResolveRuntimeCollections_Tree(t *testing.T) {
	base := t.TempDir()
	root := threeTier(t, base)
	cfgPath := filepath.Join(base, ConfigFile)
	if err := os.WriteFile(cfgPath, []byte("tree:\n  type: local\n  path: "+root+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cols, err := ResolveRuntimeCollections(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("ResolveRuntimeCollections: %v", err)
	}
	if len(cols) != 3 || cols[0].Name != "root" || cols[0].Tree == nil || cols[0].Tree.Depth != 0 || cols[2].Tree.Path != "root/platform/flux" {
		t.Errorf("cols = %d, root tree=%+v", len(cols), cols[0].Tree)
	}
}
