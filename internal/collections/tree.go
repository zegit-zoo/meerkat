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
// declared, as THIS view may see it, and whether one exists.
//
// A name outside the view answers exactly as a name nobody declared
// (nil, false). For a visible node the result is a copy with every
// reference to a knowledge base outside the view removed: Children
// lists only visible children, Parent is blank when the parent is
// hidden, and Path keeps only the segments below the deepest hidden
// ancestor (so a caller who can read `flux` alone sees `flux`, not
// `root/platform/flux`). Mounted, and each child's Mounted, report live
// residency rather than the declaration-time flag. The copy carries no
// lazy source; it is for describing the node, not mounting it.
func (r *Registry) TreeNode(name string) (*contentsource.TreeNode, bool) {
	t := r.base().tree
	if t == nil || !r.sees(name) {
		return nil, false
	}
	n, ok := t.nodes[name]
	if !ok {
		return nil, false
	}
	// A collection's own node is the richer one (a lazy child's carries
	// its declared description, limits and children); the tree index
	// holds only a stub for it.
	if c, ok := r.base().by[name]; ok && c.Tree != nil {
		n = c.Tree
	}
	out := r.viewNode(n)
	return &out, true
}

// viewNode copies n with every name outside this view removed (see
// TreeNode). It reads no field a mount writes, so it is safe against a
// concurrent lazy mount.
func (r *Registry) viewNode(n *contentsource.TreeNode) contentsource.TreeNode {
	out := contentsource.TreeNode{
		Name:          n.Name,
		Path:          r.visiblePath(n.Path),
		Depth:         n.Depth,
		Mounted:       r.treeMounted(n.Name),
		Mount:         n.Mount,
		Placement:     n.Placement,
		SourceType:    n.SourceType,
		SizeHintBytes: n.SizeHintBytes,
		StaleAfter:    n.StaleAfter,
		Access:        n.Access,
		Limits:        n.Limits,
		Description:   n.Description,
	}
	if n.Parent != "" && r.sees(n.Parent) {
		out.Parent = n.Parent
	}
	for _, ch := range n.Children {
		if !r.sees(ch.Name) {
			continue
		}
		ch.Mounted = r.treeMounted(ch.Name)
		out.Children = append(out.Children, ch)
	}
	return out
}

// visiblePath drops the segments of a tree path down to and including
// the deepest one this view cannot see.
func (r *Registry) visiblePath(path string) string {
	segs := strings.Split(path, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if !r.sees(segs[i]) {
			return strings.Join(segs[i+1:], "/")
		}
	}
	return path
}

// sees reports whether this view may know that a knowledge base called
// name exists. A collection is visible exactly when the view holds it
// (r.by, which Restrict filtered). A node the tree declares but that has
// no collection in this process at all (placement: dedicated) is
// visible when the view's Restrict predicate accepts its name; on an
// unrestricted registry, always.
//
// Every tree-aware answer — path aliases, the cold-child error, tree
// metadata — goes through this, so a hidden knowledge base answers
// exactly as one that was never declared.
func (r *Registry) sees(name string) bool {
	if _, ok := r.by[name]; ok {
		return true
	}
	if _, ok := r.base().by[name]; ok {
		return false // a collection, filtered out of this view
	}
	t := r.base().tree
	if t == nil {
		return false
	}
	if _, declared := t.nodes[name]; !declared {
		return false
	}
	return r.allow == nil || r.allow(name)
}

// treePath resolves a tree path to a collection name when, and only
// when, every knowledge base along it is visible to this view. A path
// through a hidden hub would otherwise confirm that hub's name.
func (r *Registry) treePath(path string) (string, bool) {
	t := r.base().tree
	if t == nil {
		return "", false
	}
	name, ok := t.byPath[path]
	if !ok {
		return "", false
	}
	for _, seg := range strings.Split(path, "/") {
		if !r.sees(seg) {
			return "", false
		}
	}
	return name, true
}

// treeMounted reports live residency for a name: a cold collection is
// declared but not resident.
func (r *Registry) treeMounted(name string) bool {
	if c, ok := r.base().by[name]; ok {
		return !c.IsCold()
	}
	return false
}

// TreeEntries lists every declared knowledge base in the tree that this
// view can see — its collections, mounted or cold, plus declared nodes
// with no collection in this process that its Restrict predicate
// accepts — ordered by path, each described as TreeNode describes it.
// Empty for a flat deployment.
func (r *Registry) TreeEntries() []TreeEntry {
	t := r.base().tree
	if t == nil {
		return nil
	}
	var out []TreeEntry
	for name := range t.nodes {
		n, ok := r.TreeNode(name)
		if !ok {
			continue // outside this view
		}
		out = append(out, TreeEntry{TreeNode: *n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// resolveTreeName maps a collection reference — a name or a tree path
// — to a collection name, and reports a declared node this process has
// no collection for (ErrColdCollection).
//
// A reference this view cannot see is returned unchanged with no error,
// so the caller (Get) produces the same unknown-collection error it
// gives for a name nobody declared: a hidden name, a path through a
// hidden hub and a path that does not exist are indistinguishable.
func (r *Registry) resolveTreeName(ref string) (string, error) {
	t := r.base().tree
	if t == nil {
		return ref, nil
	}
	name := ref
	if strings.Contains(ref, "/") {
		n, ok := r.treePath(ref)
		if !ok {
			return ref, nil
		}
		name = n
	}
	if !r.sees(name) {
		return ref, nil
	}
	if _, ok := r.by[name]; ok {
		return name, nil // a collection in view; Get mounts it if cold
	}
	if n, ok := t.nodes[name]; ok {
		where := ""
		if n.Parent != "" && r.sees(n.Parent) {
			where = fmt.Sprintf(", under %q", n.Parent)
		}
		return "", fmt.Errorf("%w: %q (mount: %s%s) — search its parent hub, or mount it eagerly", ErrColdCollection, name, n.Mount, where)
	}
	return name, nil
}
