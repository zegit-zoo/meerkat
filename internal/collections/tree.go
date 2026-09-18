package collections

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
)

// tree.go is the registry's view of a `tree:` deployment (meerkat-mob
// issue D). contentsource.ResolveTree walks the manifests and hands
// Open a flat list of resolved collections, each carrying its
// TreeNode; this file rebuilds the tree from those nodes, including
// the children that were declared but not mounted, and makes three
// things tree-aware:
//
//   - a request that names a declared-but-unmounted child fails with
//     ErrColdCollection (which is also an ErrUnknownCollection, so every
//     surface that already maps unknown collections keeps working);
//   - a search that names no collection searches the ROOT hub only,
//     because the root's job is to route — its pointer pages are the
//     answer, and the leaves are reached by following them;
//   - `<path>:<id>` — `root/platform/flux:concepts/drift` — resolves
//     through the tree to the collection at that path.

// ErrColdCollection is returned when a request names a knowledge base
// the tree declares but this process has not mounted (mount: lazy, or
// placement: dedicated). It wraps ErrUnknownCollection.
var ErrColdCollection = fmt.Errorf("%w: declared in the tree but not mounted by this process", ErrUnknownCollection)

// TreeEntry is one knowledge base as the tree lists it — mounted or
// not. It is contentsource.TreeNode plus what only the registry knows.
type TreeEntry struct {
	contentsource.TreeNode
}

// tree is the registry-side index of a tree deployment.
type tree struct {
	root string
	// nodes holds every declared node, mounted or not, by name.
	nodes map[string]*contentsource.TreeNode
	// byPath maps a tree path to a collection name.
	byPath map[string]string
	depth  int
	limits contentsource.Limits
}

// buildTree indexes the tree nodes the resolved collections carry. It
// returns nil for a flat deployment.
func buildTree(resolved []contentsource.ResolvedCollection) *tree {
	t := &tree{nodes: map[string]*contentsource.TreeNode{}, byPath: map[string]string{}}
	any := false
	for _, rc := range resolved {
		if rc.Tree == nil {
			continue
		}
		any = true
		t.nodes[rc.Tree.Name] = rc.Tree
		t.byPath[rc.Tree.Path] = rc.Tree.Name
		if rc.Tree.Depth == 0 {
			t.root = rc.Tree.Name
			t.limits = contentsource.DefaultLimits
			if rc.Tree.Limits != nil {
				if rc.Tree.Limits.MaxHops > 0 {
					t.limits.MaxHops = rc.Tree.Limits.MaxHops
				}
				if rc.Tree.Limits.MaxSteps > 0 {
					t.limits.MaxSteps = rc.Tree.Limits.MaxSteps
				}
				if rc.Tree.Limits.MaxAttempts > 0 {
					t.limits.MaxAttempts = rc.Tree.Limits.MaxAttempts
				}
			}
		}
		if rc.Tree.Depth > t.depth {
			t.depth = rc.Tree.Depth
		}
		// Declared-but-unmounted children exist only as their parent's
		// references; give them a node so they can be listed and named.
		for _, c := range rc.Tree.Children {
			if c.Mounted {
				continue
			}
			if _, ok := t.nodes[c.Name]; ok {
				continue
			}
			n := &contentsource.TreeNode{
				Name: c.Name, Path: rc.Tree.Path + "/" + c.Name, Depth: rc.Tree.Depth + 1, Parent: rc.Tree.Name,
				Mounted: false, Mount: c.Mount, Placement: contentsource.PlacementShared, SourceType: c.SourceType,
			}
			t.nodes[c.Name] = n
			t.byPath[n.Path] = c.Name
			if n.Depth > t.depth {
				t.depth = n.Depth
			}
		}
	}
	if !any {
		return nil
	}
	return t
}

// IsTree reports whether this registry was opened from a `tree:`.
func (r *Registry) IsTree() bool { return r.base().tree != nil }

// Root returns the root hub's collection name in a tree deployment,
// or "" for a flat one.
func (r *Registry) Root() string {
	if t := r.base().tree; t != nil {
		return t.root
	}
	return ""
}

// TreeDepth returns the deepest declared knowledge base's depth (root
// = 0), or 0 for a flat deployment.
func (r *Registry) TreeDepth() int {
	if t := r.base().tree; t != nil {
		return t.depth
	}
	return 0
}

// TreeLimits returns the root's traversal limits (defaults when the
// manifest set none); zero values for a flat deployment.
func (r *Registry) TreeLimits() contentsource.Limits {
	if t := r.base().tree; t != nil {
		return t.limits
	}
	return contentsource.Limits{}
}

// TreeNode returns the tree node for a collection name, mounted or
// declared, and whether one exists.
func (r *Registry) TreeNode(name string) (*contentsource.TreeNode, bool) {
	t := r.base().tree
	if t == nil {
		return nil, false
	}
	n, ok := t.nodes[name]
	return n, ok
}

// treeMounted reports live residency for a name: a cold collection is
// declared but not resident.
func (r *Registry) treeMounted(name string) bool {
	if c, ok := r.base().by[name]; ok {
		return !c.IsCold()
	}
	return false
}

// TreeEntries lists every declared knowledge base in the tree — the
// mounted collections in this view plus the declared-but-unmounted
// children of those — ordered by path. Empty for a flat deployment.
func (r *Registry) TreeEntries() []TreeEntry {
	t := r.base().tree
	if t == nil {
		return nil
	}
	var out []TreeEntry
	for _, n := range t.nodes {
		if n.Mounted {
			if _, visible := r.by[n.Name]; !visible {
				continue // outside this view
			}
		} else if parent, ok := t.nodes[n.Parent]; ok {
			if _, visible := r.by[parent.Name]; !visible {
				continue // its parent is outside this view; so is it
			}
		}
		e := TreeEntry{TreeNode: *n}
		e.Mounted = r.treeMounted(n.Name)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// resolveTreeName maps a collection reference — a name or a tree path
// — to a collection name, and reports a declared-but-unmounted one.
func (r *Registry) resolveTreeName(ref string) (string, error) {
	t := r.base().tree
	if t == nil {
		return ref, nil
	}
	name := ref
	if strings.Contains(ref, "/") {
		n, ok := t.byPath[ref]
		if !ok {
			return "", fmt.Errorf("%w %q — no knowledge base at that tree path", ErrUnknownCollection, ref)
		}
		name = n
	}
	if n, ok := t.nodes[name]; ok && !n.Mounted {
		return "", fmt.Errorf("%w: %q (mount: %s, under %q) — search its parent hub, or mount it eagerly", ErrColdCollection, name, n.Mount, n.Parent)
	}
	return name, nil
}
