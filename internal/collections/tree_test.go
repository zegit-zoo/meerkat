package collections

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if strings.Join(reg.Names(), ",") != "root,platform,flux,vendors" {
		t.Errorf("declared = %v (vendors is declared and cold)", reg.Names())
	}
	if v, _ := reg.Get("vendors"); v == nil || !v.IsCold() || !v.Lazy() {
		t.Errorf("vendors must be a cold, lazy collection: %+v", v)
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

func TestTree_ColdChildMountsOnFirstRequest(t *testing.T) {
	reg := openTree(t)
	v, _ := reg.Get("vendors")
	if !v.IsCold() {
		t.Fatal("vendors must start cold")
	}
	// Blocking policy (default): the first request mounts it and answers.
	hits, err := reg.Search(context.Background(), "vendors", "vendors", 5)
	if err != nil || len(hits) == 0 || hits[0].Collection != "vendors" {
		t.Fatalf("Search(vendors) = %v %v, want a mounted answer", hits, err)
	}
	if v.IsCold() || !v.Tree.Mounted {
		t.Error("vendors must be warm after the request")
	}
	if _, n := reg.Resident(); n != 1 {
		t.Errorf("resident lazy collections = %d, want 1", n)
	}
	if _, err := reg.Get("nope"); errors.Is(err, ErrColdCollection) || !errors.Is(err, ErrUnknownCollection) {
		t.Errorf("Get(nope) = %v, want plain unknown", err)
	}
}

func TestTree_AsyncColdPolicyAnswersColdThenMounts(t *testing.T) {
	reg := openTree(t)
	reg.SetCache(&contentsource.CacheSpec{ColdPolicy: contentsource.ColdAsync, RetryAfter: 50 * time.Millisecond}, nil)
	_, err := reg.Search(context.Background(), "vendors", "vendors", 5)
	var cold *ColdError
	if !errors.As(err, &cold) || !errors.Is(err, ErrColdCollection) || !errors.Is(err, ErrUnknownCollection) || cold.Name != "vendors" {
		t.Fatalf("async first request = %v, want *ColdError", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		hits, err := reg.Search(context.Background(), "vendors", "vendors", 5)
		if err == nil && len(hits) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mount never completed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
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
	// root is hidden, so the paths start below it.
	if strings.Join(paths, ",") != "platform,platform/flux" {
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

// answer renders whatever an operation returned as one string, with the
// probed reference replaced by a placeholder, so a hidden reference and
// a nonexistent one can be compared byte for byte.
func answer(ref string, v any, err error) string {
	body, _ := json.Marshal(v)
	out := string(body)
	if err != nil {
		out += " error: " + err.Error()
	}
	return strings.ReplaceAll(out, ref, "<ref>")
}

// TestTree_HiddenAnswersAsNonexistent pins meerkat-mob#39: in a tree,
// every tree-aware answer a restricted view gives about a knowledge base
// it cannot see — by name, by tree path, through a path alias, or as a
// cold child — is byte-identical to the answer for one nobody declared.
func TestTree_HiddenAnswersAsNonexistent(t *testing.T) {
	reg := openTree(t)
	only := func(names ...string) *Registry {
		return reg.Restrict(func(name string) bool {
			for _, n := range names {
				if n == name {
					return true
				}
			}
			return false
		})
	}
	ctx := context.Background()
	cases := []struct {
		view             []string
		hidden, nonexist string
	}{
		{[]string{"flux"}, "platform", "nosuchkb"},
		{[]string{"flux"}, "vendors", "nosuchkb"},                          // a cold lazy child
		{[]string{"flux"}, "root/platform", "nope/nowhere"},                // a hidden hub by path
		{[]string{"flux"}, "root/vendors", "nope/nowhere"},                 // a cold child by path
		{[]string{"flux"}, "root/platform/flux", "nope/nowhere/flux"},      // a visible leaf through hidden hubs
		{[]string{"root"}, "root/platform", "root/nowhere"},                // a hidden child of a visible root
		{[]string{"root", "flux"}, "root/platform/flux", "root/nope/flux"}, // a hidden middle hub
		{[]string{"root"}, "vendors", "nosuchkb"},
	}
	for _, tc := range cases {
		view := only(tc.view...)
		name := strings.Join(tc.view, "+") + "/" + tc.hidden
		t.Run(name, func(t *testing.T) {
			probe := func(ref string) map[string]string {
				out := map[string]string{}
				c, err := view.Get(ref)
				var got string
				if c != nil {
					got = c.Name
				}
				out["Get"] = answer(ref, got, err)
				hits, err := view.Search(ctx, ref, "flux platform vendors", 5)
				out["Search"] = answer(ref, hits, err)
				coll, id := view.SplitQualified(ref + ":flux")
				out["SplitQualified"] = answer(ref, []string{coll, id}, nil)
				page, err := view.Show("", ref+":flux")
				out["Show"] = answer(ref, page, err)
				page, err = view.Show(ref, "flux")
				out["Show(collection)"] = answer(ref, page, err)
				n, ok := view.TreeNode(ref)
				out["TreeNode"] = answer(ref, []any{n, ok}, nil)
				return out
			}
			hidden, nonexist := probe(tc.hidden), probe(tc.nonexist)
			for op := range hidden {
				if hidden[op] != nonexist[op] {
					t.Errorf("%s: hidden %q answers\n  %s\nbut nonexistent %q answers\n  %s", op, tc.hidden, hidden[op], tc.nonexist, nonexist[op])
				}
			}
			// Nothing the view lists names a knowledge base outside it.
			entries, _ := json.Marshal(view.TreeEntries())
			for _, n := range []string{"root", "platform", "flux", "vendors"} {
				if visible := view.sees(n); !visible && strings.Contains(string(entries), `"`+n) {
					t.Errorf("TreeEntries names hidden %q: %s", n, entries)
				}
				if !view.sees(n) && strings.Contains(string(entries), "/"+n) {
					t.Errorf("TreeEntries names hidden %q in a path: %s", n, entries)
				}
			}
		})
	}
	// Lookups through hidden hubs did not mount the hidden cold child.
	if v, _ := reg.Get("vendors"); !v.IsCold() {
		t.Error("a restricted lookup mounted a collection outside its view")
	}
}

func TestTree_ViewNodeFiltersMetadata(t *testing.T) {
	reg := openTree(t)
	rootOnly := reg.Restrict(func(name string) bool { return name == "root" })
	n, ok := rootOnly.TreeNode("root")
	if !ok || n.Path != "root" || len(n.Children) != 0 {
		t.Errorf("root-only view of root = %+v %v, want no children", n, ok)
	}
	fluxOnly := reg.Restrict(func(name string) bool { return name == "flux" })
	n, ok = fluxOnly.TreeNode("flux")
	if !ok || n.Path != "flux" || n.Parent != "" || n.Depth != 2 {
		t.Errorf("flux-only view of flux = %+v %v, want path flux, no parent, tier kept", n, ok)
	}
	rootFlux := reg.Restrict(func(name string) bool { return name == "root" || name == "flux" })
	if n, _ := rootFlux.TreeNode("flux"); n.Path != "flux" || n.Parent != "" {
		t.Errorf("root+flux view of flux = %+v: the hidden hub must not appear", n)
	}
	// The unrestricted registry is unchanged, and child residency is live.
	n, _ = reg.TreeNode("root")
	if n.Path != "root" || len(n.Children) != 2 || n.Children[1].Name != "vendors" || n.Children[1].Mounted {
		t.Fatalf("root = %+v", n)
	}
	if _, err := reg.Search(context.Background(), "root/vendors", "vendors", 5); err != nil {
		t.Fatalf("a visible lazy child mounts through its path alias: %v", err)
	}
	if n, _ := reg.TreeNode("root"); !n.Children[1].Mounted {
		t.Errorf("children must report live residency after the mount: %+v", n.Children)
	}
	// The copy is a copy: filtering never writes through to the tree.
	if c, _ := reg.Get("root"); len(c.Tree.Children) != 2 {
		t.Errorf("view filtering mutated the shared node: %+v", c.Tree.Children)
	}
}

// TestTree_DeclaredOnlyNodeNeedsTheViewsGrant covers a node the tree
// declares that this process has no collection for (placement:
// dedicated): it is listed and named as cold only for a view whose
// Restrict predicate accepts it, never merely because its parent is
// visible.
func TestTree_DeclaredOnlyNodeNeedsTheViewsGrant(t *testing.T) {
	reg := openTree(t)
	reg.tree.nodes["edge"] = &contentsource.TreeNode{Name: "edge", Path: "root/edge", Depth: 1, Parent: "root", Mount: contentsource.MountEager, Placement: contentsource.PlacementDedicated}
	reg.tree.byPath["root/edge"] = "edge"

	if _, err := reg.Get("edge"); !errors.Is(err, ErrColdCollection) {
		t.Errorf("unrestricted Get(edge) = %v, want the cold-collection error", err)
	}
	rootOnly := reg.Restrict(func(name string) bool { return name == "root" })
	_, hidden := rootOnly.Get("edge")
	_, never := rootOnly.Get("nosuchkb")
	if errors.Is(hidden, ErrColdCollection) || answer("edge", nil, hidden) != answer("nosuchkb", nil, never) {
		t.Errorf("hidden declared node = %v, want the same answer as %v", hidden, never)
	}
	for _, e := range rootOnly.TreeEntries() {
		if e.Name == "edge" {
			t.Error("a declared node outside the view is listed because its parent is visible")
		}
	}
	withEdge := reg.Restrict(func(name string) bool { return name == "root" || name == "edge" })
	if _, err := withEdge.Get("root/edge"); !errors.Is(err, ErrColdCollection) || !strings.Contains(err.Error(), `under "root"`) {
		t.Errorf("granted declared node = %v, want cold under root", err)
	}
	edgeOnly := reg.Restrict(func(name string) bool { return name == "edge" })
	if _, err := edgeOnly.Get("edge"); err == nil || strings.Contains(err.Error(), "root") {
		t.Errorf("a cold error must not name a hidden parent: %v", err)
	}
	if e := withEdge.TreeEntries(); len(e) != 2 || e[1].Name != "edge" || e[1].Mounted {
		t.Errorf("entries = %+v", e)
	}
}
