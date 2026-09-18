package kb

import (
	"fmt"
	"strings"
)

// links.go makes `related:` and pointer pages real.
//
// A knowledge base is a tree of collections whose hub pages must be
// able to say "the answer lives over there" and an agent must be able
// to follow that in one hop. Two things carry that:
//
//   - `related:` entries on any page, which name another page — in
//     this collection (`<id>`), in another (`<collection>:<id>`), or
//     outside meerkat altogether (`ext:<scheme>:<target>`, or an
//     `mcp://<server>` URL, the MCP protocol's own vocabulary).
//   - Pages of `type: pointer`, whose frontmatter `target:` names a
//     whole collection, one page, or an external MCP server, and whose
//     `hint:` is the one sentence an agent reads before deciding to
//     go. Beside them, `type: skill` and `type: example` pages are the
//     other two members of a capability bundle (see
//     docs/design/links.md).
//
// This file is the parser: it turns strings into typed Links and says
// what shape a pointer must have. It knows nothing about which
// collections exist — resolution (does the target exist? who links
// here?) is internal/collections' job, because only the registry knows
// the mounted set.

// Page types with routing semantics. Any other `type:` value is an OKF
// concept kind and means nothing to the router.
const (
	// TypePointer routes: `target:` + `hint:`, body optional.
	TypePointer = "pointer"
	// TypeSkill is a how-to bundled with a pointer through `related:`.
	TypeSkill = "skill"
	// TypeExample is a worked example bundled the same way.
	TypeExample = "example"
)

// Kind classifies a resolved-or-not link target.
type LinkKind string

const (
	// LinkLocal names a page in the same collection: `<id>`.
	LinkLocal LinkKind = "local"
	// LinkQualified names a page in a named collection: `<collection>:<id>`.
	LinkQualified LinkKind = "qualified"
	// LinkCollection names a whole collection (pointer targets only):
	// `collection:<name>`.
	LinkCollection LinkKind = "collection"
	// LinkExternal names something outside meerkat: `ext:<scheme>:<target>`,
	// `external:<name>` or `mcp://<server>`. Never resolved, never dangling.
	LinkExternal LinkKind = "external"
)

// Link is one parsed `related:` entry or pointer `target:`.
type Link struct {
	// Raw is the string as written.
	Raw  string   `json:"raw"`
	Kind LinkKind `json:"kind"`
	// Collection is set for LinkQualified and LinkCollection. Empty for
	// a local link means "the page's own collection".
	Collection string `json:"collection,omitempty"`
	// ID is the page ID for LinkLocal and LinkQualified.
	ID string `json:"id,omitempty"`
	// Target is the external target for LinkExternal ("mcp://datadog",
	// "https://…").
	Target string `json:"target,omitempty"`
}

// String renders the canonical form: `<id>`, `<collection>:<id>`,
// `collection:<name>` or the external target.
func (l Link) String() string {
	switch l.Kind {
	case LinkLocal:
		return l.ID
	case LinkQualified:
		return l.Collection + ":" + l.ID
	case LinkCollection:
		return "collection:" + l.Collection
	default:
		return l.Target
	}
}

// ParseLink parses one `related:` entry.
//
// Grammar, first match wins:
//
//	ext:<scheme>:<target>   external (issue A/B spelling)
//	external:<name>         external (pointer-target spelling)
//	<scheme>://<rest>       external (mcp://, https://, …)
//	collection:<name>       a whole collection (valid as a pointer target)
//	page:<collection>:<id>  a page, explicit form
//	<collection>:<id>       a page in a named collection
//	<id>                    a page in this collection
//
// A collection name obeys the same rule contentsource enforces
// ([A-Za-z0-9][A-Za-z0-9_-]*), which is what makes `<collection>:<id>`
// unambiguous: a page ID may contain "/" and "." but a collection name
// may not, and neither may contain ":". An entry that parses as
// qualified but names an unmounted collection is reported dangling by
// the resolver, not rejected here.
func ParseLink(raw string) (Link, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Link{}, fmt.Errorf("empty link")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return Link{}, fmt.Errorf("link %q contains whitespace", raw)
	}
	switch {
	case strings.HasPrefix(s, "ext:"):
		rest := strings.TrimPrefix(s, "ext:")
		if !strings.Contains(rest, ":") || strings.HasPrefix(rest, ":") {
			return Link{}, fmt.Errorf("external link %q must be ext:<scheme>:<target>", raw)
		}
		return Link{Raw: raw, Kind: LinkExternal, Target: rest}, nil
	case strings.HasPrefix(s, "external:"):
		name := strings.TrimPrefix(s, "external:")
		if name == "" {
			return Link{}, fmt.Errorf("external link %q names nothing", raw)
		}
		return Link{Raw: raw, Kind: LinkExternal, Target: name}, nil
	case strings.Contains(s, "://"):
		return Link{Raw: raw, Kind: LinkExternal, Target: s}, nil
	case strings.HasPrefix(s, "collection:"):
		name := strings.TrimPrefix(s, "collection:")
		if !validCollectionName(name) {
			return Link{}, fmt.Errorf("link %q: %q is not a valid collection name", raw, name)
		}
		return Link{Raw: raw, Kind: LinkCollection, Collection: name}, nil
	case strings.HasPrefix(s, "page:"):
		rest := strings.TrimPrefix(s, "page:")
		coll, id, ok := strings.Cut(rest, ":")
		if !ok || !validCollectionName(coll) || !validLinkID(id) {
			return Link{}, fmt.Errorf("link %q must be page:<collection>:<id>", raw)
		}
		return Link{Raw: raw, Kind: LinkQualified, Collection: coll, ID: normaliseLinkID(id)}, nil
	}
	if coll, id, ok := strings.Cut(s, ":"); ok {
		if !validCollectionName(coll) || !validLinkID(id) {
			return Link{}, fmt.Errorf("link %q: expected <collection>:<id>", raw)
		}
		return Link{Raw: raw, Kind: LinkQualified, Collection: coll, ID: normaliseLinkID(id)}, nil
	}
	if !validLinkID(s) {
		return Link{}, fmt.Errorf("link %q is not a page id", raw)
	}
	return Link{Raw: raw, Kind: LinkLocal, ID: normaliseLinkID(s)}, nil
}

// ParseRelated parses every `related:` entry of a page. Entries that do
// not parse are returned as errors alongside the ones that do, so a
// single typo neither hides the good links nor fails the page.
func ParseRelated(related []string) (links []Link, errs []error) {
	for _, raw := range related {
		l, err := ParseLink(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		links = append(links, l)
	}
	return links, errs
}

// Links parses this page's `related:` entries. See ParseRelated.
func (p Page) Links() ([]Link, []error) { return ParseRelated(p.Front.Related) }

// IsPointer reports whether this is a routing page.
func (p Page) IsPointer() bool { return p.Front.Type == TypePointer }

// Pointer returns the parsed target of a `type: pointer` page, or an
// error describing why the page is not a valid pointer: it must have a
// `target:` that is a collection, a page, or an external reference (a
// bare local `<id>` is refused — a pointer to a page in the same
// collection is just a `related:` entry), and a `hint:`.
func (p Page) Pointer() (Link, error) {
	if !p.IsPointer() {
		return Link{}, fmt.Errorf("page %q is type %q, not %q", p.ID, p.Front.Type, TypePointer)
	}
	if strings.TrimSpace(p.Front.Target) == "" {
		return Link{}, fmt.Errorf("pointer %q has no target", p.ID)
	}
	l, err := ParseLink(p.Front.Target)
	if err != nil {
		return Link{}, fmt.Errorf("pointer %q: %w", p.ID, err)
	}
	if l.Kind == LinkLocal {
		return Link{}, fmt.Errorf("pointer %q: target %q names a page in this collection — use related: for that, or page:<collection>:<id>", p.ID, p.Front.Target)
	}
	if strings.TrimSpace(p.Front.Hint) == "" {
		return Link{}, fmt.Errorf("pointer %q has no hint (one sentence telling an agent why to follow it)", p.ID)
	}
	if len(p.Front.Hint) > maxHintLen {
		return Link{}, fmt.Errorf("pointer %q: hint is %d characters, over the %d-character cap", p.ID, len(p.Front.Hint), maxHintLen)
	}
	return l, nil
}

// maxHintLen bounds a pointer hint. A hint is read by an agent before
// it decides to hop; it is a sentence, not a page, and the search
// wire shape repeats it per hit.
const maxHintLen = 300

// validCollectionName mirrors contentsource.validCollectionName without
// importing it (that package imports this one).
func validCollectionName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// validLinkID accepts what a page ID can be: a relative path with no
// empty, "." or ".." segments and no further colon.
func validLinkID(id string) bool {
	id = normaliseLinkID(id)
	if id == "" || strings.Contains(id, ":") || strings.Contains(id, `\`) {
		return false
	}
	for _, seg := range strings.Split(id, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// normaliseLinkID applies the same normalisation LoadFor applies to a
// requested ID: no leading slash, no .md suffix.
func normaliseLinkID(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "/"), ".md")
}
