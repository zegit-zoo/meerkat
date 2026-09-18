package contentsource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// tree.go turns a flat list of collections into the tree the meerkat mob
// design describes (meerkat-mob issue D): a thin root that routes, hubs
// that route further, leaves that hold pages.
//
// A knowledge base is a content source whose root carries a
// `manifest.yaml`. The manifest names the KB, its tier, and its
// children — each child is itself a content source (an S3 prefix, a
// GCS bundle, a local directory) with its own manifest. `tree:` in
// content-source.yaml names the root; ResolveTree walks the manifests,
// resolves every eagerly mounted KB exactly as a `collections:` entry
// would be resolved, and returns the flat set of collections plus the
// tree they form.
//
// Two guards make the walk safe to point at operator-written manifests:
//
//   - Depth. A KB more than MaxTreeDepth levels below the root is a
//     load error (the root is depth 0, so at most six tiers exist).
//     The cap is the six-degrees argument: any page must be reachable
//     in five hops, and it bounds recursion, memory and the blast
//     radius of a mistyped child prefix.
//   - Cycles and duplicates. A child whose name or source location was
//     already seen on the walk is a load error. Names are collection
//     names and must be unique across the whole tree, because
//     `<collection>:<id>` addresses them.
//
// `mount: lazy` children are recorded in the tree but not resolved:
// they show up in mk_list_collections as declared and unmounted, and a
// search that names one fails with ErrColdCollection. Mounting them on
// first traversal, under a cache budget, is issue E; until then a
// deployment that needs a child served marks it `mount: eager` (the
// default).

// ManifestFile is the file a knowledge base carries at its root.
const ManifestFile = "manifest.yaml"

// ManifestKind is the only `kind:` a manifest may declare.
const ManifestKind = "KnowledgeBase"

// MaxTreeDepth is the hard cap on knowledge-base nesting: the root is
// depth 0, a KB at depth MaxTreeDepth is the deepest allowed (Q2,
// answered 2026-09-17: a hard limit of five levels).
const MaxTreeDepth = 5

// Mount modes for a child.
const (
	MountEager = "eager"
	MountLazy  = "lazy"
)

// Placement modes (Q4): shared runs the KB inside this process;
// dedicated asks the operator (issue J) for its own instance. This
// process honours both by mounting shared KBs and recording dedicated
// ones as unmounted with their placement, so the tree stays complete.
const (
	PlacementShared    = "shared"
	PlacementDedicated = "dedicated"
)

// Manifest is `manifest.yaml`.
type Manifest struct {
	Kind string `yaml:"kind"`
	// Name is the collection name this KB mounts as. Unique across the
	// tree; same rule as a collections: entry.
	Name string `yaml:"name"`
	// Tier is informational (0 = root hub). The loader validates it
	// against the actual depth when set.
	Tier *int `yaml:"tier,omitempty"`
	// Parent is informational and, when set, must equal the actual
	// parent's name.
	Parent string `yaml:"parent,omitempty"`
	// Children are the KBs below this one.
	Children []Child `yaml:"children,omitempty"`
	// SizeHintBytes is the author's estimate of the KB's markdown size,
	// for the cache budget (issue E) and the tier caps.
	SizeHintBytes int64 `yaml:"size_hint_bytes,omitempty"`
	// StaleAfter is an ISO date after which the KB should be reviewed.
	StaleAfter string `yaml:"stale_after,omitempty"`
	// Contract is the KB's update contract, same schema as a
	// collection's `update:`.
	Contract *UpdateSpec `yaml:"contract,omitempty"`
	// Placement is shared (default) or dedicated. See PlacementShared.
	Placement string `yaml:"placement,omitempty"`
	// Access restricts who may reach this KB and by which path. It is
	// carried for the authz layer; this loader validates shape only.
	Access *Access `yaml:"access,omitempty"`
	// Limits are the traversal restraints for retrieval sessions that
	// start at this KB (root only in practice). Carried for issues F
	// and G; validated here.
	Limits *Limits `yaml:"limits,omitempty"`
	// DepthLimit may lower MaxTreeDepth for this subtree, never raise it.
	DepthLimit *int `yaml:"depth_limit,omitempty"`
	// Description is shown by mk_list_collections.
	Description string `yaml:"description,omitempty"`
}

// Child is one entry of Manifest.Children.
type Child struct {
	Name string `yaml:"name"`
	// Source is where the child KB lives; any runtime-capable Source.
	Source Source `yaml:"source"`
	// Mount is eager (default) or lazy.
	Mount string `yaml:"mount,omitempty"`
}

// Access is the manifest's `access:` block.
type Access struct {
	// Paths lists the tree paths (root/platform/flux) this KB may be
	// reached through; empty means any.
	Paths []string `yaml:"paths,omitempty"`
	// Identities lists the principals (authz subjects) that may reach
	// it; empty means anyone the collection's own rules admit.
	Identities []string `yaml:"identities,omitempty"`
}

// Limits are per-session traversal restraints (review decision,
// 2026-09-17): hops are collection crossings, steps are tool calls,
// attempts are searches without a show. Start high; tune from
// telemetry.
type Limits struct {
	MaxHops     int `yaml:"max_hops,omitempty"`
	MaxSteps    int `yaml:"max_steps,omitempty"`
	MaxAttempts int `yaml:"max_attempts,omitempty"`
}

// DefaultLimits are the placeholders the design names.
var DefaultLimits = Limits{MaxHops: 12, MaxSteps: 40, MaxAttempts: 20}

// TreeNode is what a resolved collection knows about its place in the
// tree. Attached to ResolvedCollection.Tree; nil for a flat
// deployment.
type TreeNode struct {
	// Name is the collection name.
	Name string `json:"name"`
	// Path is the slash-joined chain of names from the root
	// ("root/platform/flux"); the root's path is its name.
	Path string `json:"path"`
	// Depth is 0 for the root.
	Depth int `json:"tier"`
	// Parent is the parent's name; empty for the root.
	Parent string `json:"parent,omitempty"`
	// Children lists every declared child, mounted or not.
	Children []ChildRef `json:"children,omitempty"`
	// Mounted is false for a lazy or dedicated child that was declared
	// but not resolved into a collection by this process.
	Mounted bool `json:"mounted"`
	// Mount is the child's declared mount mode (eager for the root).
	Mount     string `json:"mount"`
	Placement string `json:"placement"`
	// SourceType is the child's declared source type, for an unmounted
	// child that has no collection to ask.
	SourceType    string  `json:"source_type,omitempty"`
	SizeHintBytes int64   `json:"size_hint_bytes,omitempty"`
	StaleAfter    string  `json:"stale_after,omitempty"`
	Access        *Access `json:"access,omitempty"`
	Limits        *Limits `json:"limits,omitempty"`
	Description   string  `json:"description,omitempty"`
}

// ChildRef is a child as its parent lists it.
type ChildRef struct {
	Name       string `json:"name"`
	Mount      string `json:"mount"`
	Mounted    bool   `json:"mounted"`
	SourceType string `json:"source_type"`
}

// Tree is the whole resolved tree: every node, mounted or not, keyed by
// name, plus the root's name and the deepest depth reached.
type Tree struct {
	Root  string
	Nodes map[string]*TreeNode
	// MaxDepth is the deepest node's depth.
	MaxDepth int
	// Limits are the root's traversal limits (or DefaultLimits).
	Limits Limits
}

// ByPath resolves a tree path ("root/platform/flux") to a node.
func (t *Tree) ByPath(path string) (*TreeNode, bool) {
	for _, n := range t.Nodes {
		if n.Path == path {
			return n, true
		}
	}
	return nil, false
}

// ErrColdCollection is returned when a request names a knowledge base
// that the tree declares but this process has not mounted (mount:
// lazy, or placement: dedicated).
var ErrColdCollection = errors.New("collection is declared in the tree but not mounted")

// LoadManifest reads and validates dir/manifest.yaml.
func LoadManifest(dir string) (Manifest, error) {
	body, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // G304: the resolved content dir is operator-chosen.
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(body)
}

// ParseManifest parses and validates a manifest document.
func ParseManifest(body []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", ManifestFile, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks a manifest's own shape; tree-level rules (depth,
// uniqueness, parent agreement) are checked by the walk.
func (m Manifest) Validate() error {
	if m.Kind != ManifestKind {
		return fmt.Errorf("%s: kind must be %q, got %q", ManifestFile, ManifestKind, m.Kind)
	}
	if !validCollectionName(m.Name) {
		return fmt.Errorf("%s: name %q is not a valid collection name (letters/digits, optionally - or _ after the first character, max %d)", ManifestFile, m.Name, maxCollectionNameLen)
	}
	if m.Tier != nil && *m.Tier < 0 {
		return fmt.Errorf("%s: tier must be >= 0, got %d", ManifestFile, *m.Tier)
	}
	if m.DepthLimit != nil && (*m.DepthLimit < 0 || *m.DepthLimit > MaxTreeDepth) {
		return fmt.Errorf("%s: depth_limit must be between 0 and %d (the hard cap), got %d", ManifestFile, MaxTreeDepth, *m.DepthLimit)
	}
	switch m.Placement {
	case "", PlacementShared, PlacementDedicated:
	default:
		return fmt.Errorf("%s: placement must be %s or %s, got %q", ManifestFile, PlacementShared, PlacementDedicated, m.Placement)
	}
	if m.StaleAfter != "" {
		if _, err := time.Parse("2006-01-02", m.StaleAfter); err != nil {
			return fmt.Errorf("%s: stale_after must be an ISO date (2026-12-31), got %q", ManifestFile, m.StaleAfter)
		}
	}
	if m.SizeHintBytes < 0 {
		return fmt.Errorf("%s: size_hint_bytes must be >= 0", ManifestFile)
	}
	if m.Limits != nil {
		if m.Limits.MaxHops < 0 || m.Limits.MaxSteps < 0 || m.Limits.MaxAttempts < 0 {
			return fmt.Errorf("%s: limits must be >= 0 (0 means the default)", ManifestFile)
		}
	}
	if m.Contract != nil {
		m.Contract.Normalize()
		if err := m.Contract.Validate("manifest.contract"); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(m.Children))
	for i, c := range m.Children {
		p := fmt.Sprintf("%s: children[%d]", ManifestFile, i)
		if !validCollectionName(c.Name) {
			return fmt.Errorf("%s.name %q is not a valid collection name", p, c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("%s: duplicate child name %q", p, c.Name)
		}
		seen[c.Name] = true
		switch c.Mount {
		case "", MountEager, MountLazy:
		default:
			return fmt.Errorf("%s.mount must be %s or %s, got %q", p, MountEager, MountLazy, c.Mount)
		}
		src := c.Source
		if src.Type == "" || src.Type == TypeNone {
			return fmt.Errorf("%s.source.type is required", p)
		}
		src.Layout = MergeLayout(src.Layout)
		src.Update.Normalize()
		if err := src.validate(p + ".source"); err != nil {
			return err
		}
	}
	return nil
}

// effectiveLimits fills zero fields from DefaultLimits.
func (m Manifest) effectiveLimits() Limits {
	out := DefaultLimits
	if m.Limits != nil {
		if m.Limits.MaxHops > 0 {
			out.MaxHops = m.Limits.MaxHops
		}
		if m.Limits.MaxSteps > 0 {
			out.MaxSteps = m.Limits.MaxSteps
		}
		if m.Limits.MaxAttempts > 0 {
			out.MaxAttempts = m.Limits.MaxAttempts
		}
	}
	return out
}

// sourceKey identifies a source location for cycle detection: two
// children that name the same bucket/prefix/path are the same KB even
// under different names.
func sourceKey(s Source, cfgPath string) string {
	switch s.Type {
	case TypeLocal:
		p := s.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(cfgPath), p)
		}
		return "local:" + filepath.Clean(p)
	case TypeURL:
		return "url:" + s.URL
	case TypeGCS, TypeS3:
		return s.Type + ":" + s.Endpoint + "/" + s.Bucket + "/" + s.Object + s.Prefix
	}
	return s.Type + ":" + s.Path + s.Repo
}

// ResolveTree resolves `tree:` — the root source — into the flat list of
// mounted collections (root first, then children in manifest order,
// depth-first) and the tree they form.
func ResolveTree(ctx context.Context, root Source, cfgPath string) ([]ResolvedCollection, *Tree, error) {
	t := &Tree{Nodes: map[string]*TreeNode{}}
	seenSource := map[string]string{}
	var out []ResolvedCollection
	err := resolveTreeNode(ctx, root, cfgPath, "", "", 0, MaxTreeDepth, MountEager, t, seenSource, &out)
	if err != nil {
		return nil, nil, err
	}
	return out, t, nil
}

func resolveTreeNode(ctx context.Context, src Source, cfgPath, parent, parentPath string, depth, limit int, mount string, t *Tree, seenSource map[string]string, out *[]ResolvedCollection) error {
	if depth > MaxTreeDepth {
		return fmt.Errorf("tree: knowledge base under %q would be at depth %d, over the hard cap of %d levels (root = 0) — flatten the tree or move it under a shallower hub", parentPath, depth, MaxTreeDepth)
	}
	if depth > limit {
		return fmt.Errorf("tree: knowledge base under %q would be at depth %d, over the depth_limit %d its ancestor declared", parentPath, depth, limit)
	}
	key := sourceKey(src, cfgPath)
	if prev, dup := seenSource[key]; dup {
		return fmt.Errorf("tree: knowledge base under %q names the same source as %q — a cycle or a duplicate mount", parentPath, prev)
	}
	seenSource[key] = parentPath + "/?"

	rc, err := resolveSource(ctx, src, cfgPath)
	if err != nil {
		return fmt.Errorf("tree: knowledge base under %q: %w", parentPath, err)
	}
	if rc.Dir == "" {
		return fmt.Errorf("tree: knowledge base under %q resolves to no directory (type %q cannot carry a manifest)", parentPath, src.Type)
	}
	m, err := LoadManifest(rc.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("tree: knowledge base under %q has no %s at its root — every KB in a tree carries one", parentPath, ManifestFile)
		}
		return fmt.Errorf("tree: knowledge base under %q: %w", parentPath, err)
	}
	if _, dup := t.Nodes[m.Name]; dup {
		return fmt.Errorf("tree: knowledge base name %q is declared twice (under %q and earlier) — names address collections and must be unique across the tree", m.Name, parentPath)
	}
	seenSource[key] = m.Name
	if m.Parent != "" && m.Parent != parent {
		return fmt.Errorf("tree: %s of %q says parent: %q but it is mounted under %q", ManifestFile, m.Name, m.Parent, parent)
	}
	if m.Tier != nil && *m.Tier != depth {
		return fmt.Errorf("tree: %s of %q says tier: %d but it sits at depth %d", ManifestFile, m.Name, *m.Tier, depth)
	}
	path := m.Name
	if parentPath != "" {
		path = parentPath + "/" + m.Name
	}
	placement := m.Placement
	if placement == "" {
		placement = PlacementShared
	}
	node := &TreeNode{
		Name: m.Name, Path: path, Depth: depth, Parent: parent, Mounted: true, Mount: mount,
		Placement: placement, SourceType: src.Type, SizeHintBytes: m.SizeHintBytes, StaleAfter: m.StaleAfter,
		Access: m.Access, Limits: m.Limits, Description: m.Description,
	}
	t.Nodes[m.Name] = node
	if depth == 0 {
		t.Root = m.Name
		t.Limits = m.effectiveLimits()
	}
	if depth > t.MaxDepth {
		t.MaxDepth = depth
	}

	rc.Name = m.Name
	if m.Description != "" && rc.Source.Description == "" {
		rc.Source.Description = m.Description
	}
	if m.Contract != nil && rc.Source.Update == nil {
		rc.Source.Update = m.Contract
	}
	rc.Tree = node
	*out = append(*out, rc)

	childLimit := limit
	if m.DepthLimit != nil && depth+*m.DepthLimit < childLimit {
		childLimit = depth + *m.DepthLimit
	}
	for _, c := range m.Children {
		cm := c.Mount
		if cm == "" {
			cm = MountEager
		}
		csrc := c.Source
		csrc.Layout = MergeLayout(csrc.Layout)
		csrc.Update.Normalize()
		ref := ChildRef{Name: c.Name, Mount: cm, SourceType: c.Source.Type}
		if cm == MountLazy {
			// Declared, not mounted: recorded so the tree is complete and
			// a request naming it gets ErrColdCollection, not "unknown".
			if _, dup := t.Nodes[c.Name]; dup {
				return fmt.Errorf("tree: knowledge base name %q is declared twice", c.Name)
			}
			cpath := path + "/" + c.Name
			if depth+1 > MaxTreeDepth || depth+1 > childLimit {
				return fmt.Errorf("tree: knowledge base %q would be at depth %d, over the cap", cpath, depth+1)
			}
			t.Nodes[c.Name] = &TreeNode{Name: c.Name, Path: cpath, Depth: depth + 1, Parent: m.Name, Mounted: false, Mount: cm, Placement: PlacementShared, SourceType: c.Source.Type}
			if depth+1 > t.MaxDepth {
				t.MaxDepth = depth + 1
			}
			node.Children = append(node.Children, ref)
			continue
		}
		before := len(*out)
		if err := resolveTreeNode(ctx, csrc, cfgPath, m.Name, path, depth+1, childLimit, cm, t, seenSource, out); err != nil {
			return err
		}
		mounted := (*out)[before]
		if mounted.Name != c.Name {
			return fmt.Errorf("tree: child declared as %q under %q has a %s naming it %q — the two must agree", c.Name, m.Name, ManifestFile, mounted.Name)
		}
		ref.Mounted = t.Nodes[c.Name].Mounted
		node.Children = append(node.Children, ref)
	}
	return nil
}
