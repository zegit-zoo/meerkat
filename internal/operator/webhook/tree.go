// Package webhook is the meerkat operator's validating admission
// webhook (meerkat-mob#10). It applies the `tree:` manifest loader's
// depth and cycle rules to KnowledgeBase objects at admission, so a
// tree the loader would refuse at startup is refused when it is
// written instead.
package webhook

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/operator/api/v1alpha1"
)

// MaxDepth is the deepest a KnowledgeBase may sit (root = 0). It is the
// manifest loader's hard cap, so a tree admitted here is one the
// server will load.
const MaxDepth = contentsource.MaxTreeDepth

// node is the part of a KnowledgeBase the tree rules read.
type node struct {
	parent     string
	depthLimit *int32
}

// checkTree validates candidate's place in the tree formed by existing
// (every KnowledgeBase in its namespace, candidate's old version
// included or not) with candidate in it. It returns warnings for what
// cannot be checked yet, and an error for a cycle or a depth that the
// manifest loader would refuse.
//
// Only rules the candidate can break are checked: its own ancestors
// cannot move, so a violation elsewhere in the namespace (one admitted
// while the webhook was down) does not block an unrelated edit.
func checkTree(candidate *v1alpha1.KnowledgeBase, existing []v1alpha1.KnowledgeBase) (warnings []string, err error) {
	name := candidate.Name
	nodes := make(map[string]node, len(existing)+1)
	for i := range existing {
		kb := &existing[i]
		nodes[kb.Name] = node{parent: kb.Spec.Parent, depthLimit: kb.Spec.DepthLimit}
	}
	nodes[name] = node{parent: candidate.Spec.Parent, depthLimit: candidate.Spec.DepthLimit}

	if candidate.Spec.Parent == name {
		return nil, fmt.Errorf("spec.parent: KnowledgeBase %q names itself as its parent", name)
	}

	// Walk up from the candidate. chain[0] is the candidate and the last
	// element the topmost KnowledgeBase this namespace holds.
	chain := []string{name}
	seen := map[string]bool{name: true}
	for cur := nodes[name].parent; cur != ""; {
		if seen[cur] {
			return nil, fmt.Errorf("spec.parent: the parent chain loops: %s -> %s", strings.Join(chain, " -> "), cur)
		}
		n, ok := nodes[cur]
		if !ok {
			warnings = append(warnings, fmt.Sprintf(
				"parent KnowledgeBase %q does not exist yet: the depth is checked only as far as %q, and again when %q is created",
				cur, chain[len(chain)-1], cur))
			break
		}
		seen[cur] = true
		chain = append(chain, cur)
		cur = n.parent
	}

	top := chain[len(chain)-1]
	// A top that names a parent of its own sits at depth 1 at least.
	topDepth := 0
	if nodes[top].parent != "" {
		topDepth = 1
	}

	children := make(map[string][]string, len(nodes))
	for n, nd := range nodes {
		if nd.parent != "" {
			children[nd.parent] = append(children[nd.parent], n)
		}
	}
	for _, c := range children {
		sort.Strings(c)
	}

	// Walk down from the top with the same depth and limit arithmetic as
	// contentsource's treeWalk: a depthLimit on a KnowledgeBase lowers
	// the limit for everything below it, never raises it.
	var walk func(n string, depth, limit int, inSubtree bool, path []string) error
	visited := map[string]bool{}
	walk = func(n string, depth, limit int, inSubtree bool, path []string) error {
		if visited[n] {
			// Reached twice: a loop below the candidate. The upward walk
			// catches every loop through the candidate, so this one
			// predates it; stop rather than recurse forever.
			return nil
		}
		visited[n] = true
		path = append(path, n)
		inSubtree = inSubtree || n == name
		if inSubtree {
			where := strings.Join(path, "/")
			if depth > MaxDepth {
				return fmt.Errorf("KnowledgeBase %q would be at depth %d (%s), over the hard cap of %d levels (root = 0): flatten the tree or move it under a shallower hub", n, depth, where, MaxDepth)
			}
			if depth > limit {
				return fmt.Errorf("KnowledgeBase %q would be at depth %d (%s), over the depthLimit %d its ancestor declared", n, depth, where, limit)
			}
		}
		childLimit := limit
		if dl := nodes[n].depthLimit; dl != nil && depth+int(*dl) < childLimit {
			childLimit = depth + int(*dl)
		}
		for _, c := range children[n] {
			if err := walk(c, depth+1, childLimit, inSubtree, path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(top, topDepth, MaxDepth, false, nil); err != nil {
		return warnings, err
	}
	return warnings, nil
}

// childrenOf lists the KnowledgeBases that name name as their parent.
func childrenOf(name string, existing []v1alpha1.KnowledgeBase) []string {
	var out []string
	for i := range existing {
		if existing[i].Spec.Parent == name && existing[i].Name != name {
			out = append(out, existing[i].Name)
		}
	}
	sort.Strings(out)
	return out
}
