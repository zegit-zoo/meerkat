package kb

import "strings"

// visibility.go is per-page read visibility: the small amount of policy
// that has to live in the page representation itself, because every
// read surface (search ranking, list enumeration, show, the ambiguity
// error) has to apply the same answer and none of them may be allowed
// to forget.
//
// # Where the owner comes from
//
// A page's owner is derived from its ID, and from nothing else:
//
//	memory/personal/<owner>/<slug>   private to <owner>
//	anything else                    visible to every reader of the
//	                                 collection
//
// That is deliberate. The alternative — a frontmatter field, or a
// struct field stamped by whichever constructor happened to build the
// page — is a field somebody can forget to set (the page silently
// becomes public) or, worse, a field a document's own bytes could
// claim. internal/memory builds a personal memory's ID out of the
// namespace it derived from the VERIFIED (issuer, subject) pair and
// out of nothing a caller supplied, so the ID is already an unforgeable
// carrier for the owner. Deriving from it means there is no second
// source of truth to drift, and no constructor whose omission opens a
// hole: a page is private if and only if its ID says so.
//
// See docs/design/memory.md ("Private personal reads").

// PrivatePrefix is the reserved page-ID prefix whose NEXT path segment
// names the principal a page is private to. It is the page-ID form of
// internal/memory's personal store layout (personal/<namespace>/<slug>
// under the reserved "memory/" page-ID prefix); a test in that package
// pins the two against each other, so a change to either side fails
// loudly rather than quietly making memories public.
//
// The prefix is reserved: an ordinary content page that happens to sit
// at content/memory/personal/x/y.md is treated as private to "x" too.
// That is the safe direction — such a page becomes harder to read, not
// easier — and it removes the question of whether the content tree or
// the memory overlay is authoritative about who owns an ID.
const PrivatePrefix = "memory/personal/"

// PrivateOwner returns the owner a page ID is private to, or "" when the
// page is visible to every reader of its collection.
//
// A truncated ID under the prefix ("memory/personal/alice", with no
// document after it) still yields an owner. There is no such page — the
// store always appends a slug — but answering "alice" rather than ""
// keeps the failure direction right: an unexpected shape hides, it does
// not publish.
func PrivateOwner(id string) string {
	rest, found := strings.CutPrefix(strings.TrimPrefix(id, "/"), PrivatePrefix)
	if !found {
		return ""
	}
	owner, _, _ := strings.Cut(rest, "/")
	return owner
}

// PrivateOwner reports the principal this page is private to, or "" when
// every reader of its collection may see it.
func (p Page) PrivateOwner() string { return PrivateOwner(p.ID) }

// Viewer is who a read is being performed as, for the purposes of
// per-page visibility. It is a value, not an interface: it is copied
// into a per-request registry view once and consulted everywhere below,
// so there is nothing to configure per call site and nothing to forget.
//
// Two states matter:
//
//	Unfiltered()      no per-page policy is in force — every page,
//	                  private or not. This is the CLI, `mk http serve`
//	                  and any surface with a single, local user.
//	AsOwner(owner)    the pages everyone may see, plus the ones private
//	                  to owner. AsOwner("") is a caller who owns
//	                  nothing: they see the public pages and no private
//	                  page at all.
//
// The zero Viewer is AsOwner("") — the least-privileged of the three.
// A viewer that was never initialised therefore hides private pages
// rather than exposing them.
type Viewer struct {
	unfiltered bool
	owner      string
}

// Unfiltered returns the viewer that sees every page regardless of
// owner. It is the "no policy in force" state, and it mirrors what a
// nil *authz.Grants means one layer up: the surfaces that use it (the
// CLI, the static-token HTTP server) serve exactly one principal, who
// owns everything in front of them.
func Unfiltered() Viewer { return Viewer{unfiltered: true} }

// AsOwner returns the viewer for a principal identified by owner — an
// opaque token this package never interprets. internal/memory derives
// it from the verified (issuer, subject) pair; nothing else may.
//
// AsOwner("") is meaningful and is NOT the same as Unfiltered: it is a
// caller with no owner identity at all (an anonymous hosted request),
// who may read every public page and no private one.
func AsOwner(owner string) Viewer { return Viewer{owner: owner} }

// IsUnfiltered reports whether this viewer sees every page.
func (v Viewer) IsUnfiltered() bool { return v.unfiltered }

// Owner returns the owner token this viewer holds, if any.
func (v Viewer) Owner() string { return v.owner }

// CanSee reports whether this viewer may see p.
func (v Viewer) CanSee(p Page) bool { return v.CanSeeOwner(p.PrivateOwner()) }

// CanSeeOwner is CanSee against an already-derived owner, for callers
// that hold an ID or an index field rather than a whole page.
//
// An empty owner is a public page and is visible to everyone. A
// non-empty owner is visible only on an exact match, so a viewer with no
// owner of their own ("") matches no private page.
func (v Viewer) CanSeeOwner(owner string) bool {
	if v.unfiltered || owner == "" {
		return true
	}
	return owner == v.owner
}

// VisiblePages returns the pages of the slice this viewer may see. An
// unfiltered viewer gets the input slice back untouched, so the common
// (single-user) case allocates nothing.
func (v Viewer) VisiblePages(pages []Page) []Page {
	if v.unfiltered {
		return pages
	}
	out := pages[:0:0]
	for _, p := range pages {
		if v.CanSee(p) {
			out = append(out, p)
		}
	}
	return out
}
