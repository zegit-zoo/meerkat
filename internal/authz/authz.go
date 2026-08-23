// Package authz is meerkat's fine-grained, collection-level
// authorization model: who a caller is (Identity), what they may do to
// each collection (Grants), and the declarative policy that maps one to
// the other (Config/Policy).
//
// It is deliberately a leaf package. It knows nothing about HTTP, about
// OIDC (see internal/authn, which produces an Identity from a verified
// token), or about internal/collections (whose Registry.Restrict takes a
// plain predicate this package supplies). That is what lets
// internal/contentsource embed an auth: block in content-source.yaml
// without an import cycle.
//
// # The one rule that matters
//
// A collection a caller may not read is INVISIBLE, not denied. It is
// filtered out of the registry once, at the start of the request, and
// every downstream surface — search, list, show, the ambiguity error,
// GET /collections, the MCP tool descriptions — then operates on a
// registry that, as far as it can tell, never mounted it. Denying
// per-operation instead would turn each of those into an existence
// oracle: "id exists in 2 collections" and "available: a, b, secret"
// both leak the name and the contents of something the caller was
// refused. See docs/design/hosted-mcp.md.
//
// # Capabilities
//
// read decides visibility. personal-write, team-write and global-write
// decide what the memory toolset (mk_save_memory) may write, per scope;
// admin implies all of them. Every capability is enforced: see
// internal/mcp/memory.go for the write side and docs/design/memory.md
// for the scope->capability table.
package authz

import (
	"context"
	"sort"
	"strings"
)

// Capability is one thing a caller may do to a collection.
type Capability string

const (
	// CapRead permits search/show/list against the collection, and is
	// the capability that decides whether a collection is VISIBLE at all.
	// It confers nothing on the write side: a reader is not offered the
	// memory tool.
	CapRead Capability = "read"
	// CapPersonalWrite permits writing memory documents scoped to the
	// caller's own identity (the memory toolset's "personal" scope). The
	// namespace those documents land in is derived from the verified
	// token, never from a tool argument — holding this capability lets a
	// caller write as themselves and as nobody else.
	CapPersonalWrite Capability = "personal-write"
	// CapTeamWrite permits writing memory documents shared with the
	// caller's team (the memory toolset's "team" scope).
	CapTeamWrite Capability = "team-write"
	// CapGlobalWrite permits writing memory documents visible to every
	// reader of the collection (the memory toolset's "global" scope).
	//
	// It is a capability of its own rather than a reuse of CapAdmin
	// because admin implies capabilities that do not exist yet: folding
	// global writes into it would retroactively widen every admin rule an
	// operator has already written. See docs/design/memory.md.
	CapGlobalWrite Capability = "global-write"
	// CapAdmin implies every other capability, present and future. Grant
	// it sparingly: a rule that grants admin today also grants whatever
	// capability a later meerkat adds.
	CapAdmin Capability = "admin"
)

// AllCapabilities lists every capability in the order they are reported.
// Order is least- to most-privileged, which is also the order a human
// reads them in.
func AllCapabilities() []Capability {
	return []Capability{CapRead, CapPersonalWrite, CapTeamWrite, CapGlobalWrite, CapAdmin}
}

// WriteCapabilities lists the capabilities that permit writing
// something, in the same least- to most-privileged order. CapAdmin is
// excluded: it is an implication, not a grant of its own, and
// CapabilitySet.Has already answers true for every entry below when it
// is held.
func WriteCapabilities() []Capability {
	return []Capability{CapPersonalWrite, CapTeamWrite, CapGlobalWrite}
}

// ParseCapability maps a policy document's string to a Capability. The
// second result is false for anything unrecognised — an unknown
// capability is a configuration error, never a silently-ignored line,
// because "the deployment ignored the capability I wrote" is exactly
// how a policy ends up more permissive than its author believes.
func ParseCapability(s string) (Capability, bool) {
	c := Capability(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range AllCapabilities() {
		if c == known {
			return c, true
		}
	}
	return "", false
}

// CapabilitySet is a set of capabilities held over one collection.
type CapabilitySet map[Capability]bool

// Has reports whether the set confers c. CapAdmin implies everything,
// so an admin set answers true for a capability it doesn't literally
// contain — including capabilities that don't exist yet.
func (cs CapabilitySet) Has(c Capability) bool {
	if cs == nil {
		return false
	}
	return cs[c] || cs[CapAdmin]
}

// List returns the capabilities literally present, in AllCapabilities
// order. It does not expand CapAdmin's implications: it reports what
// the policy said, which is what an operator wants to see echoed back.
func (cs CapabilitySet) List() []Capability {
	out := make([]Capability, 0, len(cs))
	for _, c := range AllCapabilities() {
		if cs[c] {
			out = append(out, c)
		}
	}
	return out
}

// Strings renders List as plain strings, in the same order — the
// JSON-ready form a collection-discovery surface (mk_list_collections)
// wants for its "capabilities" field.
func (cs CapabilitySet) Strings() []string {
	list := cs.List()
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = string(c)
	}
	return out
}

// Identity is a caller, as established by an authenticated token. It is
// the input to policy evaluation and nothing else: it carries no
// decisions, only verified assertions about who is asking.
type Identity struct {
	// Subject is the token's `sub` — the stable, issuer-scoped
	// identifier for the principal. Never empty for a verified token.
	Subject string
	// Issuer is the `iss` the token was verified against. Two identities
	// with the same Subject from different issuers are different
	// principals, and policy matching treats them as such.
	Issuer string
	// Email is the caller's email, from whichever claim the provider's
	// claims.email mapping names. May be empty.
	Email string
	// Groups is the caller's group/role membership, from the provider's
	// claims.groups mapping. Values are compared case-insensitively by
	// policy rules, since directory products disagree about the case of
	// group names and none of them mean it significantly.
	Groups []string
	// Tenant is the caller's tenant/organization, from the provider's
	// claims.tenant mapping (Entra's `tid`, say). May be empty.
	Tenant string
}

// String renders an identity for a log line: subject and issuer always,
// email when known. Groups are deliberately omitted — a caller's full
// group membership is not something to spray across access logs.
func (i Identity) String() string {
	if i.Email != "" {
		return i.Subject + " (" + i.Email + ") @ " + i.Issuer
	}
	return i.Subject + " @ " + i.Issuer
}

// Grants is the evaluated authorization decision for one caller: what
// they may do to each collection, by name. A collection absent from the
// map confers nothing.
//
// A nil *Grants means "no policy is in force" — the state stdio MCP and
// the single-static-token HTTP server are always in, and the state a
// hosted server with no auth: block configured is in. It is
// deliberately distinct from an empty non-nil Grants, which means "a
// policy ran and granted this caller nothing".
type Grants struct {
	// byCollection maps collection name -> capabilities held.
	byCollection map[string]CapabilitySet
	// wildcard holds capabilities granted over every collection by a
	// rule that named "*". Kept separate from byCollection so a
	// wildcard rule doesn't have to be re-expanded whenever the mounted
	// set changes.
	wildcard CapabilitySet
	// identity is the caller the grants were evaluated for, carried
	// along so access logging and (later) write-scope derivation have
	// one thing to reach for.
	identity Identity
}

// NewGrants builds Grants directly, for tests and for callers that
// compute an authorization decision some other way than Policy.
// byCollection may name "*" to grant over every collection.
func NewGrants(id Identity, byCollection map[string][]Capability) *Grants {
	g := &Grants{byCollection: map[string]CapabilitySet{}, identity: id}
	for name, caps := range byCollection {
		set := make(CapabilitySet, len(caps))
		for _, c := range caps {
			set[c] = true
		}
		if name == Wildcard {
			g.wildcard = mergeCaps(g.wildcard, set)
			continue
		}
		g.byCollection[name] = mergeCaps(g.byCollection[name], set)
	}
	return g
}

// Wildcard is the collection name a policy rule uses to mean "every
// mounted collection, including ones added later".
const Wildcard = "*"

// Identity returns the caller the grants were evaluated for.
func (g *Grants) Identity() Identity {
	if g == nil {
		return Identity{}
	}
	return g.identity
}

// Can reports whether the caller holds capability c over the named
// collection.
//
// A nil *Grants answers true unconditionally: no policy in force means
// no restriction, which is what keeps stdio MCP and the pre-OIDC HTTP
// server behaving exactly as they did. Every surface that must be
// gated is responsible for ensuring a non-nil Grants is in context —
// see internal/authn.Middleware, which always installs one.
func (g *Grants) Can(collection string, c Capability) bool {
	if g == nil {
		return true
	}
	if g.wildcard.Has(c) {
		return true
	}
	return g.byCollection[collection].Has(c)
}

// CanRead is Can(collection, CapRead) as a func value, ready to hand to
// collections.Registry.Restrict. This is *the* authorization filter:
// every READ surface inherits it by operating on the restricted
// registry.
func (g *Grants) CanRead(collection string) bool { return g.Can(collection, CapRead) }

// CanWrite reports whether the caller holds ANY write capability over
// the collection. It answers "may this caller write here at all", not
// "may they write at the scope they asked for" — the per-scope check is
// Can(collection, <the scope's capability>) and happens on top of this.
//
// It is the write path's counterpart to CanRead: the memory tool
// restricts the registry with it, so a collection the caller was not
// granted a write on is invisible to that tool exactly as an unreadable
// collection is invisible to search. Being strictly narrower than
// CanRead's view, it can leak nothing the read path does not already.
//
// It is also what decides whether an unauthorized team/global write is
// STAGED for review or refused outright: staging a memory into a
// collection is only offered to a principal who is already a writer
// there in some other scope.
func (g *Grants) CanWrite(collection string) bool {
	for _, c := range WriteCapabilities() {
		if g.Can(collection, c) {
			return true
		}
	}
	return false
}

// Capabilities returns the capabilities held over one collection,
// wildcard grants folded in. Reported by `mk mcp serve-http`'s
// diagnostics and consumed by the memory toolset's scope check.
func (g *Grants) Capabilities(collection string) CapabilitySet {
	if g == nil {
		set := make(CapabilitySet, len(AllCapabilities()))
		for _, c := range AllCapabilities() {
			set[c] = true
		}
		return set
	}
	return mergeCaps(g.wildcard, g.byCollection[collection])
}

// Empty reports whether the grants confer nothing at all — a caller
// who authenticated successfully but whom no policy rule matched. A nil
// *Grants (no policy in force) is never empty.
//
// It is capability-AGNOSTIC on purpose: any non-empty capability set,
// over any collection, makes the grants non-empty. That is what keeps
// the authentication gate's 403 from refusing a principal who holds
// only write capabilities — a rule granting `[personal-write]` and no
// read is unusual, but its holder has business here and must reach the
// memory tool. See internal/authn.Gate.Middleware.
func (g *Grants) Empty() bool {
	if g == nil {
		return false
	}
	if len(g.wildcard) > 0 {
		return false
	}
	for _, set := range g.byCollection {
		if len(set) > 0 {
			return false
		}
	}
	return true
}

// Named returns the collection names the grants mention explicitly, in
// sorted order. It excludes wildcard grants (which name no collection),
// so it is a description of the policy, not of what the caller can
// reach — use a restricted registry's Names() for that.
func (g *Grants) Named() []string {
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.byCollection))
	for name := range g.byCollection {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Wildcarded reports whether the caller holds c over every collection.
func (g *Grants) Wildcarded(c Capability) bool {
	if g == nil {
		return true
	}
	return g.wildcard.Has(c)
}

func mergeCaps(a, b CapabilitySet) CapabilitySet {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(CapabilitySet, len(a)+len(b))
	for c := range a {
		out[c] = true
	}
	for c := range b {
		out[c] = true
	}
	return out
}

// --- request context -----------------------------------------------

type ctxKey int

const grantsKey ctxKey = iota

// NewContext returns ctx carrying g. A non-nil g puts the request under
// policy; installing a nil g is a no-op, so a caller can't accidentally
// downgrade an already-authorized context to unrestricted.
func NewContext(ctx context.Context, g *Grants) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, grantsKey, g)
}

// FromContext returns the grants in force for ctx, or nil when no
// policy applies (stdio MCP, the static-token HTTP server, or a hosted
// server with no auth: configured).
//
// Callers should treat nil as "unrestricted" — see Grants.Can — rather
// than as an error. It is the back-compat path, not a failure.
func FromContext(ctx context.Context) *Grants {
	g, _ := ctx.Value(grantsKey).(*Grants)
	return g
}
