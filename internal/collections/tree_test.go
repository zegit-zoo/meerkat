package collections

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

func treeKB(t *testing.T, base, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(dir, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\ntype: pointer\ntarget: collection:platform\nhint: Platform things.\n---\n# Platform\nThe platform hub: flux, gitops, kubernetes.\n"
	if name != "root" {
		body = "# " + name + " page about " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "wiki", name+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, contentsource.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openTree(t *testing.T) *Registry {
	t.Helper()
	base := t.TempDir()
	flux := treeKB(t, base, "flux", "kind: KnowledgeBase\nname: flux\n")
	platform := treeKB(t, base, "platform", "kind: KnowledgeBase\nname: platform\nchildren:\n  - name: flux\n    source: {type: local, path: "+flux+"}\n")
	vendors := treeKB(t, base, "vendors", "kind: KnowledgeBase\nname: vendors\n")
	root := treeKB(t, base, "root", "kind: KnowledgeBase\nname: root\nlimits: {max_hops: 3}\nchildren:\n  - name: platform\n    source: {type: local, path: "+platform+"}\n  - name: vendors\n    source: {type: local, path: "+vendors+"}\n    mount: lazy\n")
	resolved, _, err := contentsource.ResolveTree(context.Background(), contentsource.Source{Type: contentsource.TypeLocal, Path: root, Layout: contentsource.MergeLayout(contentsource.Layout{})}, filepath.Join(base, "content-source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func TestTree_RegistryKnowsTheTree(t *testing.T) {
	reg := openTree(t)
	if !reg.IsTree() || reg.Root() != "root" || reg.TreeDepth() != 2 || reg.TreeLimits().MaxHops != 3 || reg.TreeLimits().MaxSteps != contentsource.DefaultLimits.MaxSteps {
		t.Fatalf("tree = root %q depth %d limits %+v", reg.Root(), reg.TreeDepth(), reg.TreeLimits())
	}
	if strings.Join(reg.Names(), ",") != "root,platform,flux" {
		t.Errorf("mounted = %v", reg.Names())
	}
	var paths []string
	for _, e := range reg.TreeEntries() {
		paths = append(paths, e.Path+"="+map[bool]string{true: "mounted", false: "cold"}[e.Mounted])
	}
	if strings.Join(paths, ",") != "root=mounted,root/platform=mounted,root/platform/flux=mounted,root/vendors=cold" {
		t.Errorf("entries = %v", paths)
	}
	c, _ := reg.Get("flux")
	if c.Tree == nil || c.Tree.Depth != 2 || c.Tree.Parent != "platform" {
		t.Errorf("flux collection tree = %+v", c.Tree)
	}
}

func TestTree_ColdChildIsRefusedNotUnknown(t *testing.T) {
	reg := openTree(t)
	_, err := reg.Get("vendors")
	if !errors.Is(err, ErrColdCollection) || !errors.Is(err, ErrUnknownCollection) {
		t.Fatalf("Get(vendors) = %v, want ErrColdCollection wrapping ErrUnknownCollection", err)
	}
	if !strings.Contains(err.Error(), "lazy") {
		t.Errorf("message should name the mount mode: %v", err)
	}
	if _, err := reg.Search(context.Background(), "vendors", "anything", 5); !errors.Is(err, ErrColdCollection) {
		t.Errorf("Search(vendors) = %v", err)
	}
	if _, err := reg.Get("nope"); errors.Is(err, ErrColdCollection) || !errors.Is(err, ErrUnknownCollection) {
		t.Errorf("Get(nope) = %v, want plain unknown", err)
	}
}

func TestTree_UnqualifiedSearchAsksTheRootOnly(t *testing.T) {
	reg := openTree(t)
	hits, err := reg.Search(context.Background(), "", "platform", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Collection != "root" {
			t.Fatalf("unqualified search reached %q; a tree search asks the root hub only: %v", h.Collection, hits)
		}
	}
	if len(hits) == 0 || hits[0].Kind != KindPointer || hits[0].Target != "collection:platform" {
		t.Errorf("root search must return the routing pointer: %+v", hits)
	}
	// Naming the leaf, by name or by path, reaches it.
	for _, ref := range []string{"flux", "root/platform/flux"} {
		hits, err := reg.Search(context.Background(), ref, "flux", 10)
		if err != nil || len(hits) == 0 || hits[0].Collection != "flux" {
			t.Errorf("Search(%q) = %v %v", ref, hits, err)
		}
	}
	if _, err := reg.Search(context.Background(), "root/nowhere", "x", 10); !errors.Is(err, ErrUnknownCollection) {
		t.Errorf("bad path = %v", err)
	}
}

func TestTree_PathAliasInQualifiedIDs(t *testing.T) {
	reg := openTree(t)
	coll, id := reg.SplitQualified("root/platform/flux:flux")
	if coll != "flux" || id != "flux" {
		t.Errorf("SplitQualified(path) = %q %q", coll, id)
	}
	ref, err := reg.Show("", "root/platform/flux:flux")
	if err != nil || ref.Collection != "flux" {
		t.Errorf("Show(path alias) = %+v %v", ref, err)
	}
	if coll, id := reg.SplitQualified("root/unknown:x"); coll != "" || id != "root/unknown:x" {
		t.Errorf("unknown path must not split: %q %q", coll, id)
	}
	// Flat registries are untouched.
	flat, _ := New(FromPages("a", []kb.Page{{ID: "p", Title: "P"}}))
	if flat.IsTree() || flat.Root() != "" || flat.TreeEntries() != nil {
		t.Error("a flat registry must report no tree")
	}
	if coll, _ := flat.SplitQualified("a/b:p"); coll != "" {
		t.Errorf("flat registry split a path alias: %q", coll)
	}
}

func TestTree_ViewsHideSubtreesTheyCannotSee(t *testing.T) {
	reg := openTree(t)
	only := reg.Restrict(func(name string) bool { return name == "platform" || name == "flux" })
	var paths []string
	for _, e := range only.TreeEntries() {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != "root/platform,root/platform/flux" {
		t.Errorf("restricted entries = %v (root and its cold child must be absent)", paths)
	}
	hits, err := only.Search(context.Background(), "", "flux", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("a view without the root falls back to searching what it can see")
	}
}
