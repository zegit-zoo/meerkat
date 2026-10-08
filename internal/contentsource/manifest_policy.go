package contentsource

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zegit-zoo/meerkat/internal/memory"
)

// manifest_policy.go bounds what a manifest.yaml may add to a tree.
//
// A manifest is written by whoever maintains a knowledge base, not by
// the operator who wrote content-source.yaml, so a child it declares is
// a request, not an instruction. The operator decides which requests
// are honoured with the `manifest_children:` block:
//
//	manifest_children:
//	  types: [s3, url, local]          # source types a child may use
//	  buckets: [kb, kb-archive]        # gcs/s3 buckets a child may read
//	  endpoints: [https://s3.example.net]
//	  url_prefixes: [https://kb.example.net/bundles/]
//	  local_paths: [/srv/kb]           # directories a local child must sit under
//
// Absent, a child may only be an object-store child in the tree root's
// own bucket, through the root's own endpoint: the location the
// operator already pointed meerkat at. Nothing else is allowed — in
// particular no `local` and no `url` child — until the operator lists it.
//
// Some keys are refused in a manifest whatever the policy says (see
// refuseManifestOnlyKeys): memory:, update.method: direct, and
// refresh.remote_check / on_divergence. They make meerkat write
// somewhere or run git in a directory, which only the operator may ask
// for.

// ChildPolicy is the `manifest_children:` block of content-source.yaml.
type ChildPolicy struct {
	// Types lists the source types a manifest child may declare (local,
	// url, gcs, s3). Empty means: the tree root's type when it is an
	// object store, plus url when URLPrefixes is set and local when
	// LocalPaths is set.
	Types []string `yaml:"types,omitempty"`
	// Buckets lists the gcs/s3 buckets a child may read, in addition to
	// the tree root's own.
	Buckets []string `yaml:"buckets,omitempty"`
	// Endpoints lists the S3 endpoints a child may name, in addition to
	// the tree root's own. A child that names none uses the provider's
	// default endpoint, which is always allowed.
	Endpoints []string `yaml:"endpoints,omitempty"`
	// URLPrefixes lists the https:// prefixes a url child must start with.
	URLPrefixes []string `yaml:"url_prefixes,omitempty"`
	// LocalPaths lists absolute directories a local child must resolve
	// under (after symlinks are followed).
	LocalPaths []string `yaml:"local_paths,omitempty"`
}

// Validate checks the block's own shape.
func (p *ChildPolicy) Validate(label string) error {
	if p == nil {
		return nil
	}
	for _, t := range p.Types {
		switch t {
		case TypeLocal, TypeURL, TypeGCS, TypeS3:
		default:
			return fmt.Errorf("%s.types: %q is not a type a manifest child can use (local, url, gcs, s3)", label, t)
		}
	}
	for _, b := range p.Buckets {
		if b == "" || strings.Contains(b, "/") {
			return fmt.Errorf("%s.buckets: %q is not a bucket name", label, b)
		}
	}
	for _, e := range p.Endpoints {
		u, err := url.Parse(e)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("%s.endpoints: %q is not an endpoint URL", label, e)
		}
	}
	for _, pre := range p.URLPrefixes {
		if err := validURLPrefix(pre); err != nil {
			return fmt.Errorf("%s.url_prefixes: %q %w", label, pre, err)
		}
	}
	for _, lp := range p.LocalPaths {
		if !filepath.IsAbs(lp) {
			return fmt.Errorf("%s.local_paths: %q must be an absolute directory", label, lp)
		}
	}
	return nil
}

// validURLPrefix requires an https URL with a host, no user info, query
// or fragment, and a path ending in "/" — so a prefix always ends on a
// path-segment boundary and cannot be extended into another host.
func validURLPrefix(pre string) error {
	u, err := url.Parse(pre)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("must be an https:// URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not carry user info, a query or a fragment")
	}
	if !strings.HasSuffix(u.Path, "/") {
		return errors.New(`must end in "/" (a path prefix, e.g. https://kb.example.net/bundles/)`)
	}
	return nil
}

// childRules is the effective policy one tree walk applies: the
// operator's block (or its absence) combined with the root's location
// and the operator's writable stores.
type childRules struct {
	// unrestricted skips the allowlist (ResolveTree's programmatic
	// form). The unconditional refusals still apply.
	unrestricted bool
	types        map[string]bool
	buckets      map[string]bool
	endpoints    map[string]bool
	urlPrefixes  []string
	localPaths   []string
	// protected are the operator's writable stores (memory:, intake:):
	// no child may mount a location that overlaps one.
	protected []memory.Spec
}

// newChildRules builds the rules for a tree rooted at root.
func newChildRules(p *ChildPolicy, root Source, protected []memory.Spec) *childRules {
	r := &childRules{types: map[string]bool{}, buckets: map[string]bool{}, endpoints: map[string]bool{}, protected: protected}
	if root.IsObjectStore() {
		r.buckets[root.Bucket] = true
		if root.Endpoint != "" {
			r.endpoints[normEndpoint(root.Endpoint)] = true
		}
	}
	if p == nil {
		if root.IsObjectStore() {
			r.types[root.Type] = true
		}
		return r
	}
	if len(p.Types) > 0 {
		for _, t := range p.Types {
			r.types[t] = true
		}
	} else {
		if root.IsObjectStore() {
			r.types[root.Type] = true
		}
		if len(p.URLPrefixes) > 0 {
			r.types[TypeURL] = true
		}
		if len(p.LocalPaths) > 0 {
			r.types[TypeLocal] = true
		}
	}
	for _, b := range p.Buckets {
		r.buckets[b] = true
	}
	for _, e := range p.Endpoints {
		r.endpoints[normEndpoint(e)] = true
	}
	r.urlPrefixes = p.URLPrefixes
	for _, lp := range p.LocalPaths {
		r.localPaths = append(r.localPaths, realPath(filepath.Clean(lp)))
	}
	return r
}

func normEndpoint(e string) string { return strings.TrimRight(strings.ToLower(e), "/") }

const policyHint = "the operator may allow it under manifest_children: in content-source.yaml"

// admit checks one manifest child against the rules and returns the
// source to resolve: a local child's path made absolute (relative paths
// resolve against the directory the manifest sits in, never the
// operator's config directory) and the child marked as manifest-owned
// so resolution re-checks it. label names the child in errors.
func (r *childRules) admit(label string, src Source, manifestDir string) (Source, error) {
	if !r.unrestricted && !r.types[src.Type] {
		return src, fmt.Errorf("%s: type %q is not allowed for a manifest child (%s)", label, src.Type, policyHint)
	}
	switch src.Type {
	case TypeGCS, TypeS3:
		if !r.unrestricted {
			if !r.buckets[src.Bucket] {
				return src, fmt.Errorf("%s: bucket %q is not allowed for a manifest child (%s)", label, src.Bucket, policyHint)
			}
			if src.Endpoint != "" && !r.endpoints[normEndpoint(src.Endpoint)] {
				return src, fmt.Errorf("%s: endpoint %q is not allowed for a manifest child — only an endpoint the operator named may be used (%s)", label, src.Endpoint, policyHint)
			}
		}
		for _, m := range r.protected {
			if storeOverlapsSource(m, src) {
				return src, fmt.Errorf("%s: bucket %q at %q overlaps a memory or intake store this deployment writes — a manifest child may not mount it", label, src.Bucket, src.Prefix+src.Object)
			}
		}
	case TypeURL:
		if !r.unrestricted && !urlUnderPrefix(src.URL, r.urlPrefixes) {
			return src, fmt.Errorf("%s: url %q is not under an allowed prefix (%s)", label, src.URL, policyHint)
		}
	case TypeLocal:
		p := src.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(manifestDir, p)
		}
		p = filepath.Clean(p)
		if err := checkManifestLocal(p); err != nil {
			return src, fmt.Errorf("%s: %w", label, err)
		}
		real := realPath(p)
		if !r.unrestricted && !slices.ContainsFunc(r.localPaths, func(root string) bool { return within(real, root) }) {
			return src, fmt.Errorf("%s: local path %q is not under an allowed directory (%s)", label, src.Path, policyHint)
		}
		for _, m := range r.protected {
			if m.Type == memory.BackendLocal && filepath.IsAbs(m.Path) {
				mp := realPath(filepath.Clean(m.Path))
				if within(real, mp) || within(mp, real) {
					return src, fmt.Errorf("%s: local path %q overlaps a memory or intake store this deployment writes — a manifest child may not mount it", label, src.Path)
				}
			}
		}
		src.Path = p
		src.manifest = &manifestOrigin{localRoots: r.localPaths, unrestricted: r.unrestricted}
	}
	return src, nil
}

// manifestOrigin marks a Source declared by a manifest rather than by
// the operator. resolveSource re-checks a local one when it is resolved
// — including when a lazy child is mounted long after the walk — so a
// symlink swapped in between cannot move it somewhere the walk refused.
type manifestOrigin struct {
	localRoots   []string
	unrestricted bool
}

// recheck re-applies the local-path rules at resolution time.
func (o *manifestOrigin) recheck(dir string) error {
	if err := checkManifestLocal(dir); err != nil {
		return err
	}
	if o.unrestricted {
		return nil
	}
	real := realPath(dir)
	if !slices.ContainsFunc(o.localRoots, func(root string) bool { return within(real, root) }) {
		return fmt.Errorf("manifest child path %q is no longer under an allowed directory", dir)
	}
	return nil
}

// checkManifestLocal refuses a local child that resolves into meerkat's
// own cache directory: that is where fetched archives and object-store
// trees are extracted, so a path there would mount content some other
// source fetched, under a name and policy the manifest chose.
func checkManifestLocal(p string) error {
	base, err := os.UserCacheDir()
	if err != nil {
		return fmt.Errorf("cannot place local path %q relative to the content cache (%w); refusing it", p, err)
	}
	cache := realPath(filepath.Join(base, "meerkat"))
	if within(realPath(p), cache) {
		return fmt.Errorf("local path %q resolves inside meerkat's cache directory; a manifest child may not mount it", p)
	}
	return nil
}

// realPath resolves symlinks in p as far as p exists: the longest
// existing ancestor is resolved and the rest re-appended, so a path that
// does not exist yet still compares correctly against one that does
// (macOS's /var -> /private/var, for one).
func realPath(p string) string {
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether p is root or a descendant of it. Both must be
// clean absolute paths.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// urlUnderPrefix reports whether raw starts with one of prefixes (each
// validated by validURLPrefix: https, a host, a path ending in "/").
// Scheme and host must match exactly; a path carrying a dot segment or
// an encoded dot or slash is never under a prefix, since the server may
// normalise it somewhere else.
func urlUnderPrefix(raw string, prefixes []string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return false
	}
	ep := strings.ToLower(u.EscapedPath())
	if strings.Contains(ep, "%2e") || strings.Contains(ep, "%2f") || strings.Contains(ep, "%5c") {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	for _, pre := range prefixes {
		pu, err := url.Parse(pre)
		if err != nil {
			continue
		}
		if strings.EqualFold(u.Scheme, pu.Scheme) && strings.EqualFold(u.Host, pu.Host) && strings.HasPrefix(u.Path, pu.Path) {
			return true
		}
	}
	return false
}

// storeOverlapsSource reports whether an object-store content source
// reads any object a writable store holds: same backend type, bucket and
// endpoint, and one location is a prefix of the other.
func storeOverlapsSource(m memory.Spec, s Source) bool {
	if m.Type != s.Type || m.Bucket != s.Bucket || normEndpoint(m.Endpoint) != normEndpoint(s.Endpoint) {
		return false
	}
	loc := s.Prefix
	if s.Object != "" {
		loc = s.Object
	}
	return prefixesNest(m.Prefix, loc)
}

// prefixesNest reports whether one object-key prefix contains the other
// (an empty prefix is the whole bucket and contains everything).
func prefixesNest(a, b string) bool {
	a, b = path.Clean("/"+a), path.Clean("/"+b)
	return a == b || a == "/" || b == "/" || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// refuseManifestOnlyKeys refuses the source keys a manifest may never
// set, whatever manifest_children allows: each makes meerkat write to a
// store or run git in a directory, which only the operator may ask for.
func refuseManifestOnlyKeys(p string, s Source) error {
	if s.Memory != nil {
		return fmt.Errorf("%s.memory is refused in %s: a memory store receives users' saved memories, so only the operator's content-source.yaml may declare one", p, ManifestFile)
	}
	if s.Refresh.HasLocalOnlyKeys() {
		return fmt.Errorf("%s.refresh: remote_check and on_divergence are refused in %s: they run git against the directory, so only the operator's content-source.yaml may set them", p, ManifestFile)
	}
	if s.Update.DeclaredMethod() == UpdateDirect {
		return fmt.Errorf("%s.update: method: %s is refused in %s: it tells agents to write into the source, so only the operator's content-source.yaml may declare it", p, UpdateDirect, ManifestFile)
	}
	return nil
}

// childRules returns the rules a content-source.yaml tree: walk applies:
// the operator's manifest_children: block, the tree root's location,
// and the writable stores no child may overlap.
func (c Config) childRules() *childRules {
	var protected []memory.Spec
	if c.Tree != nil && c.Tree.Memory != nil {
		protected = append(protected, *c.Tree.Memory)
	}
	if c.Intake != nil {
		protected = append(protected, *c.Intake)
	}
	var root Source
	if c.Tree != nil {
		root = *c.Tree
	}
	return newChildRules(c.ManifestChildren, root, protected)
}
