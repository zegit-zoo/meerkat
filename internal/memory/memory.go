// Package memory is meerkat's writable side: structured memory
// documents saved by the mk_save_memory MCP tool into a collection's
// memory store, at one of three scopes.
//
//	personal  the caller's own namespace, derived from their token
//	team      shared with the collection's writers
//	global    visible to every reader of the collection
//
// # The one rule that matters
//
// A personal memory's namespace comes from the VERIFIED IDENTITY and
// from nothing else. Namespace(id) hashes the (issuer, subject) pair a
// token was verified with; no tool argument reaches it, and there is no
// argument that could. A caller may choose their memory's key, its
// title and its body — never whose memory it is. Spoofing is not
// blocked by a check that could be forgotten; there is simply no input
// path from the request to the namespace.
//
// # Layout
//
// A store is a directory (or a GCS prefix) that is NOT part of the
// collection's served content tree. Documents live at:
//
//	personal/<namespace>/<slug>.md
//	team/<slug>.md
//	global/<slug>.md
//	_staging/<scope>/<namespace>/<slug>.md   (pending review)
//
// and are surfaced to search/show/list by being loaded into the
// collection's page overlay (see internal/collections). _staging/ is
// skipped by the loader, so a pending artifact cannot become readable
// by being forgotten about — it is excluded by construction, not by a
// filter someone has to remember to apply.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// Scope is where a memory document is written and who can see it.
type Scope string

const (
	// ScopePersonal writes into the caller's own namespace. Requires
	// authz.CapPersonalWrite.
	ScopePersonal Scope = "personal"
	// ScopeTeam writes into the collection's shared team space. Requires
	// authz.CapTeamWrite, or stages for review.
	ScopeTeam Scope = "team"
	// ScopeGlobal writes into the collection's global space. Requires
	// authz.CapGlobalWrite, or stages for review.
	ScopeGlobal Scope = "global"
)

// AllScopes lists the scopes in least- to most-shared order.
func AllScopes() []Scope { return []Scope{ScopePersonal, ScopeTeam, ScopeGlobal} }

// ScopeNames renders AllScopes for a message or a tool description.
func ScopeNames() []string {
	out := make([]string, 0, len(AllScopes()))
	for _, s := range AllScopes() {
		out = append(out, string(s))
	}
	return out
}

// ParseScope maps a tool argument to a Scope. Unknown input is refused
// rather than defaulted: guessing which scope a caller meant is how a
// note intended for one person ends up readable by a company.
func ParseScope(s string) (Scope, bool) {
	sc := Scope(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range AllScopes() {
		if sc == known {
			return sc, true
		}
	}
	return "", false
}

// Capability is the authz capability a scope requires.
func (s Scope) Capability() authz.Capability {
	switch s {
	case ScopePersonal:
		return authz.CapPersonalWrite
	case ScopeTeam:
		return authz.CapTeamWrite
	case ScopeGlobal:
		return authz.CapGlobalWrite
	default:
		// Unreachable: every Scope value comes from ParseScope. Returning
		// a capability nobody can hold is the safe answer to a scope that
		// somehow isn't one of the three.
		return authz.Capability("unknown-scope")
	}
}

// StagingPrefix is the store-relative directory pending (unauthorized)
// team/global writes land in. It is skipped by Store.Load, so nothing
// under it is ever indexed or served.
const StagingPrefix = "_staging"

// PageIDPrefix is the page-ID namespace every memory document occupies.
// Keeping memories under one reserved prefix is what lets a reader tell
// a saved memory from an ingested wiki page at a glance, and what makes
// `mk_list --prefix memory/` a listing of exactly the memories.
const PageIDPrefix = "memory"

// anonymousNamespace is the personal namespace used when there is no
// verified identity at all. It is reachable only on the stdio transport
// (see Service.AllowAnonymousPersonal), where the process was spawned
// by the one user it serves and there is nobody to be confused with.
const anonymousNamespace = "local"

// Namespace derives the personal-memory namespace for a verified
// identity.
//
// It is a hash of (issuer, subject) — the two fields that together name
// a principal, and the only two that are stable. Email and groups are
// deliberately NOT inputs: a person who changes team or address must
// not lose their memories, and a namespace that moves when a directory
// attribute changes is a namespace that silently orphans data.
//
// A readable slug of the subject is prefixed for operator legibility
// only; the hash carries the uniqueness, so two subjects that sanitize
// to the same slug still get different namespaces.
func Namespace(id authz.Identity) string {
	if id.Subject == "" {
		return anonymousNamespace
	}
	sum := sha256.Sum256([]byte(id.Issuer + "\x00" + id.Subject))
	digest := hex.EncodeToString(sum[:])[:16]
	if label := Slug(id.Subject); label != "" {
		if len(label) > 24 {
			label = strings.TrimRight(label[:24], "-")
		}
		return label + "-" + digest
	}
	return digest
}

// Anonymous reports whether an identity carries no verified subject —
// the state a stdio session and an allow_unauthenticated deployment are
// in. Personal writes are refused for such a caller unless the
// transport explicitly opted in (see Service.AllowAnonymousPersonal),
// because every anonymous caller would otherwise share one namespace.
func Anonymous(id authz.Identity) bool { return id.Subject == "" }

// maxSlugLen bounds a slug. Object names and file names have limits,
// and a memory key is caller-supplied; the bound is generous enough
// that no realistic key is truncated and small enough that no path
// built from one is pathological.
const maxSlugLen = 96

// Slug reduces caller-supplied text to one safe path component:
// lowercase, [a-z0-9] plus '-' and '_', runs collapsed, trimmed,
// truncated.
//
// This is a security boundary, not a cosmetic one. It is what a
// crafted key like "../../team/payroll" or "..%2f.." meets: every
// separator, dot and control character becomes '-', so the result can
// only ever be a leaf name inside the namespace the IDENTITY chose.
// There is no input for which Slug returns something containing '/',
// '\' or "..".
func Slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := true // leading dashes are trimmed by never being written
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= maxSlugLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// Document is one memory, as the tool receives it: the caller's
// content plus the scope they asked for. Who is saving it is not a
// field here — it arrives separately, from the token, and is filled in
// by Service.Save.
type Document struct {
	// Key is the caller's stable identifier for this memory. Slugified
	// on the way in; re-saving the same key updates the same document
	// (under an optimistic-locking precondition). Empty falls back to
	// the title.
	Key string
	// Title is the memory's human title, and the search index's
	// highest-boosted field.
	Title string
	// Body is the memory itself: markdown, stored verbatim.
	Body string
	// Tags are optional frontmatter tags.
	Tags []string
}

// Ref names a memory document's place in a store: the scope it was
// written at, the namespace (personal only), the slug, and the derived
// store key and page ID.
type Ref struct {
	Scope     Scope
	Namespace string
	Slug      string
	// Key is the store-relative object/file name, always ending .md.
	Key string
	// PageID is how the document is addressed by mk_show / mk_search.
	PageID string
}

// StagingKey is where this reference's document goes when the caller is
// not authorized to write the scope directly. It carries the namespace
// even for team/global, so a reviewer can see who proposed what without
// having to open the file.
func (r Ref) StagingKey() string {
	return path.Join(StagingPrefix, string(r.Scope), r.Namespace, r.Slug+".md")
}

// Resolve builds the Ref for a document at a scope, in a namespace. It
// is the only place a store key is constructed, and it takes the
// namespace as an argument rather than reading it from the document
// precisely so that the caller-supplied half (doc) and the
// identity-derived half (ns) cannot be confused for one another.
func Resolve(doc Document, scope Scope, ns string) (Ref, error) {
	slug := Slug(doc.Key)
	if slug == "" {
		slug = Slug(doc.Title)
	}
	if slug == "" {
		return Ref{}, fmt.Errorf("a memory needs a key or a title containing at least one letter or digit")
	}
	if ns == "" {
		return Ref{}, fmt.Errorf("no memory namespace could be derived for this caller")
	}
	r := Ref{Scope: scope, Namespace: ns, Slug: slug}
	switch scope {
	case ScopePersonal:
		r.Key = path.Join(string(ScopePersonal), ns, slug+".md")
	default:
		r.Key = path.Join(string(scope), slug+".md")
	}
	r.PageID = path.Join(PageIDPrefix, strings.TrimSuffix(r.Key, ".md"))
	return r, nil
}

// Status values written into a memory document's frontmatter.
const (
	// StatusLive marks a document that is stored, indexed and readable.
	StatusLive = "saved"
	// StatusPending marks a staged document awaiting review. It is never
	// loaded into a collection, so this value is only ever read by a
	// human (or a promotion tool) looking at the staging area directly.
	StatusPending = "pending-review"
)

// TypeMemory is the OKF `type` every memory document carries, so
// `mk_list --type Memory` and a frontmatter filter both find them.
const TypeMemory = "Memory"

// CategoryMemory is the frontmatter category memory documents carry.
const CategoryMemory = "memory"

// frontmatter is the memory document's YAML header. It is a struct of
// its own rather than a kb.Frontmatter because the memory_* provenance
// keys must be written as TOP-LEVEL keys: kb's parser collects unknown
// top-level keys into Frontmatter.Extra, so writing them at the top
// level is exactly what makes them round-trip back into Extra on read.
// (kb.MarshalFrontmatter would nest them under an `extra:` key, which
// re-parses one level deeper than it was written.)
//
// Field order is the emitted order, and it is the order a human reads:
// what this is, then who recorded it.
type frontmatter struct {
	ID          string        `yaml:"id"`
	Title       string        `yaml:"title"`
	Type        string        `yaml:"type"`
	Category    string        `yaml:"category"`
	Status      string        `yaml:"status"`
	Tags        []string      `yaml:"tags,omitempty"`
	Generated   *kb.Generated `yaml:"generated,omitempty"`
	MemoryScope string        `yaml:"memory_scope"`
	// MemoryNamespace is the identity-derived namespace — recorded so a
	// document is self-describing about whose memory it is even after it
	// has been copied out of the store.
	MemoryNamespace string `yaml:"memory_namespace"`
	MemoryKey       string `yaml:"memory_key"`
	// MemorySubject / MemoryIssuer are the verified claims the namespace
	// was derived from. The subject is an opaque identifier, not a name
	// or an address: this is provenance, not a contact list.
	MemorySubject string `yaml:"memory_subject,omitempty"`
	MemoryIssuer  string `yaml:"memory_issuer,omitempty"`
}

// Render turns a document into the exact bytes stored: a `---`
// delimited YAML header followed by the caller's markdown.
//
// status distinguishes a live document from a staged one. now is
// injected rather than read from the clock so a test asserts on bytes
// rather than on a moving target.
func Render(doc Document, ref Ref, id authz.Identity, status string, now time.Time) ([]byte, error) {
	fm := frontmatter{
		ID:       ref.PageID,
		Title:    strings.TrimSpace(doc.Title),
		Type:     TypeMemory,
		Category: CategoryMemory,
		Status:   status,
		Tags:     normaliseTags(doc.Tags),
		Generated: &kb.Generated{
			By: "meerkat/mk_save_memory",
			At: now.UTC().Format(time.RFC3339),
		},
		MemoryScope:     string(ref.Scope),
		MemoryNamespace: ref.Namespace,
		MemoryKey:       ref.Slug,
		MemorySubject:   id.Subject,
		MemoryIssuer:    id.Issuer,
	}
	if fm.Title == "" {
		fm.Title = ref.Slug
	}
	head, err := yaml.Marshal(fm)
	if err != nil {
		return nil, fmt.Errorf("render memory frontmatter: %w", err)
	}
	body := strings.TrimRight(doc.Body, "\n")
	return []byte("---\n" + string(head) + "---\n\n" + body + "\n"), nil
}

// normaliseTags trims, drops empties, de-duplicates and sorts tags, so
// two saves of the same memory with the tags in a different order
// produce the same bytes (and therefore the same local version token).
func normaliseTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// Page parses stored bytes back into the kb.Page a collection serves.
// key is the store-relative name the bytes were read from; it is what
// the page ID is derived from, so a document whose frontmatter claims a
// different id: cannot be served under that claimed id.
func Page(key string, body []byte) (kb.Page, error) {
	id := path.Join(PageIDPrefix, strings.TrimSuffix(key, ".md"))
	page, err := kb.ParsePage(id, path.Join("memory", key), body)
	if err != nil {
		return kb.Page{}, fmt.Errorf("parse memory %q: %w", key, err)
	}
	return page, nil
}
