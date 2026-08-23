// Package kb embeds the knowledge-base wiki content at build time and
// exposes it through a small read-only filesystem API.
//
// Content is synced from the configured content source into
// internal/kb/content/ at build time (see internal/contentsource and
// content-source.yaml) and embedded verbatim. Files matching excludedFiles
// are filtered at access time so we never serve build artefacts.
//
// At run time the embed can be overridden by a directory on disk (see
// UseFS and internal/kbdir) — the --kb-dir flag / MEERKAT_KB_DIR env
// var let an operator update KB content without rebuilding the binary.
// The embedded content remains the fallback when no directory is
// configured.
//
// Each page may carry YAML frontmatter delimited by '---'. We parse
// it into Frontmatter and expose category / owner / status / source
// / tags / related as first-class fields the search index can boost
// and the CLI/MCP/HTTP layers can filter on.
package kb

import (
	"bufio"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed all:content
var wikiFS embed.FS

// currentFS is the filesystem List/Load/FS actually read from. It
// defaults to the build-time embedded content and can be redirected to
// a directory on disk via UseFS — see internal/kbdir, which adapts a
// content-repo-layout directory onto the "content/..." paths this
// package expects, and wires it up from the --kb-dir flag /
// MEERKAT_KB_DIR env var at CLI startup.
//
// It is deliberately simple global state — set once before any
// concurrent readers start today (mirrors how -ldflags variables work) —
// but held behind an atomic.Pointer rather than a bare fs.FS var. fs.FS
// is an interface (a 2-word type+data pair); a plain `var currentFS
// fs.FS` assigned by one goroutine while another reads it is a data
// race on those words (a torn read can observe a type from one value
// and data from another), even though every write today happens-before
// any server starts via cobra's PersistentPreRunE. atomic.Pointer makes
// every read/write of the *whole* interface value atomic, so the
// package stays race-detector-clean even if a hot-reload/reconfigure
// path ever calls UseFS after readers are already running. Tests that
// want isolation without touching this should build kb.Page values
// directly instead (see internal/search/inject_test.go's NewFromPages
// pattern) rather than pointing UseFS at a fixture.
var currentFS atomic.Pointer[fs.FS]

func init() {
	setFS(wikiFS)
}

// setFS atomically stores fsys as the active filesystem.
func setFS(fsys fs.FS) {
	currentFS.Store(&fsys)
}

// loadFS atomically loads the active filesystem. Every internal read
// path (List, loadByPath) goes through this rather than touching
// currentFS directly, so a concurrent UseFS call can never produce a
// torn read of the interface value.
func loadFS() fs.FS { return *currentFS.Load() }

// UseFS redirects List/Load/FS to read from fsys instead of the
// embedded content. Passing nil restores the embedded default.
func UseFS(fsys fs.FS) {
	if fsys == nil {
		setFS(wikiFS)
		return
	}
	setFS(fsys)
}

var excludedFiles = []string{
	"content/lint-report.md",
}

// Source describes where a page was ingested from. Agents use this
// to fetch the canonical version via gh / glab / pdftotext / etc.
type Source struct {
	// Host is the git hosting provider: github | gitlab | local.
	// Empty is inferred at ingestion time (see internal/sources.Normalize):
	// "local" for synthesised/pdfs sources, "github" otherwise.
	Host          string `yaml:"host,omitempty"  json:"host,omitempty"`
	Type          string `yaml:"type,omitempty"  json:"type,omitempty"`
	Repo          string `yaml:"repo,omitempty"  json:"repo,omitempty"`
	Group         string `yaml:"group,omitempty" json:"group,omitempty"`
	Ref           string `yaml:"ref,omitempty"   json:"ref,omitempty"`
	Path          string `yaml:"path,omitempty"  json:"path,omitempty"`
	WebURL        string `yaml:"web_url,omitempty"        json:"web_url,omitempty"`
	Version       string `yaml:"version,omitempty"        json:"version,omitempty"`
	EffectiveDate string `yaml:"effective_date,omitempty" json:"effective_date,omitempty"`
}

// Frontmatter holds the common engine-core fields present in every
// meerkat page. Deployment-specific fields are captured in Extra so the
// engine core stays generic without losing data.
//
// Several fields implement OKF (Open Knowledge Format) v0.2 semantics —
// see https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md
// ("SPEC.md" in comments below). meerkat's own KBs and OKF bundles share
// this one struct: an OKF field is promoted into the core here only
// when it is useful independent of OKF (a filter facet, a trust
// signal); anything else (resource, sources, okf_version, ...) stays in
// Extra, which already satisfies OKF's "preserve unknown keys"
// consumer requirement (SPEC.md §4.1, §11).
type Frontmatter struct {
	ID    string `yaml:"id"    json:"id,omitempty"`
	Title string `yaml:"title" json:"title,omitempty"`
	// Type identifies the concept's kind (e.g. "BigQuery Table",
	// "Metric", "Playbook"). It is OKF's only REQUIRED frontmatter field
	// (SPEC.md §4.1: "type is the only always-required key; a concept
	// carrying just type is fully conformant", §11) and doubles as a
	// natural search/list facet independent of OKF — see ByType.
	// Promoted out of Extra (where it previously landed) into the core.
	Type string `yaml:"type" json:"type,omitempty"`
	// Description is OKF's recommended one-line summary (SPEC.md §4.1),
	// used by index generators, search snippets and previews. Promoted
	// out of Extra for the same reason as Type: broadly useful, not
	// just an OKF-ism.
	Description string `yaml:"description"    json:"description,omitempty"`
	Category    string `yaml:"category"       json:"category,omitempty"`
	Subcategory string `yaml:"subcategory"    json:"subcategory,omitempty"`
	Owner       string `yaml:"owner"          json:"owner,omitempty"`
	// Status is a free string, deliberately not a closed enum. meerkat's
	// own ingestion pipeline uses placeholder/reviewed/...; OKF (SPEC.md
	// §5.4) uses draft/stable/deprecated. Both vocabularies coexist
	// untouched here — see ByStatus and the ingest package, which only
	// ever test Status against specific string literals, never a
	// validated/closed set, so an OKF value that isn't one of meerkat's
	// own literals simply doesn't match those checks instead of being
	// rejected. No translation layer is applied in either direction.
	Status       string   `yaml:"status"         json:"status,omitempty"`
	Tags         []string `yaml:"tags"           json:"tags,omitempty"`
	Related      []string `yaml:"related"        json:"related,omitempty"`
	Source       Source   `yaml:"source"         json:"source,omitempty"`
	LastIngested string   `yaml:"last_ingested"  json:"last_ingested,omitempty"`
	Language     string   `yaml:"language"       json:"language,omitempty"`
	// FailureReason is set by the ingestion pipeline when a page could
	// not be refreshed, so operators can triage failures.
	FailureReason string `yaml:"failure_reason" json:"failure_reason,omitempty"`

	// Generated, Verified, and StaleAfter surface OKF's trust and
	// lifecycle families (SPEC.md §5.2, §5.5). All three are optional;
	// absence is meaningful (see TrustTier / IsStale below) rather than
	// an error (§5, §11). Unlike the plain string fields above, these
	// two use yaml omitempty: Generated is a pointer and Verified a
	// slice specifically so an absent field is dropped entirely if this
	// Frontmatter is ever round-tripped through MarshalFrontmatter,
	// rather than injecting a `generated: null` / `verified: []` line
	// into the many existing meerkat pages that have neither.
	Generated *Generated `yaml:"generated,omitempty" json:"generated,omitempty"`
	// Verified is normally a list, but the spec permits a single
	// verifier to be written as one bare mapping — see VerifiedList.
	Verified VerifiedList `yaml:"verified,omitempty" json:"verified,omitempty"`
	// StaleAfter is an absolute YYYY-MM-DD date (SPEC.md §5.5), not a
	// relative TTL. See IsStale.
	StaleAfter string `yaml:"stale_after" json:"stale_after,omitempty"`

	// Extra captures deployment-specific frontmatter fields not in the
	// common core (e.g. regulator, tier, superseded_by), and any OKF
	// field deliberately not promoted (resource, sources, okf_version).
	// Values are preserved as parsed YAML scalars or structures.
	Extra map[string]any `yaml:"extra,omitempty" json:"extra,omitempty"`
}

// Generated records how a concept's current content was produced (OKF
// SPEC.md §5.2). By is REQUIRED within generated per the spec; At is an
// ISO 8601 datetime marking the content's last meaningful change.
// meerkat does not validate either sub-field's shape (the actor
// convention of §7, or ISO 8601 for At) — an unparsed or unconventional
// value is preserved verbatim and simply won't match TrustTier's
// "human:" prefix check; it is never rejected (§11).
type Generated struct {
	By string `yaml:"by"           json:"by,omitempty"`
	At string `yaml:"at,omitempty" json:"at,omitempty"`
}

// Verifier is one verification event (OKF SPEC.md §5.2): who or what
// confirmed a concept against its sources/resource, and when.
type Verifier struct {
	By string `yaml:"by"           json:"by,omitempty"`
	At string `yaml:"at,omitempty" json:"at,omitempty"`
}

// VerifiedList is the parsed form of OKF's `verified` frontmatter field
// (SPEC.md §5.2): normally a list of Verifier events, but "a single
// verifier MAY be written as one { by, at } mapping without the list
// dash." Consumers "MUST treat a bare mapping as a one-element list"
// (§5.2, restated as a conformance MUST in §11) — UnmarshalYAML below
// implements exactly that normalization, so every other caller
// (TrustTier, the CLI, MCP) only ever sees a slice, never a bare
// mapping.
type VerifiedList []Verifier

// UnmarshalYAML implements the sequence-or-bare-mapping tolerance
// documented on VerifiedList.
func (v *VerifiedList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		var list []Verifier
		if err := value.Decode(&list); err != nil {
			return err
		}
		*v = list
		return nil
	case yaml.MappingNode:
		var single Verifier
		if err := value.Decode(&single); err != nil {
			return err
		}
		*v = VerifiedList{single}
		return nil
	case yaml.ScalarNode:
		// `verified:` written with an explicit null/empty scalar (e.g.
		// `~`) is treated the same as the key being absent rather than
		// as an error — OKF's absent-is-meaningful framing (§5) applies
		// just as well to a present-but-empty key.
		if value.Tag == "!!null" || strings.TrimSpace(value.Value) == "" {
			*v = nil
			return nil
		}
		return fmt.Errorf("verified: expected a mapping or a list of mappings, got scalar %q", value.Value)
	default:
		return fmt.Errorf("verified: unsupported YAML node kind %v", value.Kind)
	}
}

// Trust tiers derived from Verified (OKF SPEC.md §5.3), lowest to
// highest. Advisory signals only: a concept with no trust frontmatter
// at all is still fully consumable and must never be rejected on that
// basis (§5.3, §11) — meerkat never does; TrustTier is purely
// informational, surfaced by `mk show --json` and the mk_show MCP tool.
const (
	TrustUnverified       = "unverified"
	TrustMachineConfirmed = "machine-confirmed"
	TrustHumanReviewed    = "human-reviewed"
)

// humanActorPrefix is the actor convention (OKF SPEC.md §7) that marks
// a verifier as a person rather than an agent/tool (<producer>/<version>)
// or an automated process (process:<id>). TrustTier keys off exactly
// this prefix, per the spec: "producers MUST use it for hand-authored
// or human-confirmed content."
const humanActorPrefix = "human:"

// TrustTier derives the OKF advisory trust tier (SPEC.md §5.3) from
// Verified:
//
//   - Verified is empty (no verified key)                  -> TrustUnverified
//   - verified, but no entry's By has the "human:" prefix   -> TrustMachineConfirmed
//   - verified, and at least one entry's By has the
//     "human:" prefix                                       -> TrustHumanReviewed
//
// This is derived on demand rather than stored, mirroring how the spec
// frames trust (and credibility, §5.1) as inferred from recorded
// signals rather than a persisted verdict.
func (fm Frontmatter) TrustTier() string {
	if len(fm.Verified) == 0 {
		return TrustUnverified
	}
	for _, v := range fm.Verified {
		if strings.HasPrefix(v.By, humanActorPrefix) {
			return TrustHumanReviewed
		}
	}
	return TrustMachineConfirmed
}

// IsStale reports whether the concept is past its OKF stale_after date
// (SPEC.md §5.5: "content is stale when today >= stale_after"). now is
// compared as a calendar date and should be in UTC (e.g.
// time.Now().UTC()) — stale_after is a date, not an instant, and this
// parses it at UTC midnight, so comparing against now in some other
// location could flip the answer across midnight. An empty or
// unparsable StaleAfter is never stale: a missing or malformed optional
// field must not manufacture a staleness signal that isn't there (§5.5
// is optional; §11 forbids penalizing a concept for a missing optional
// field).
func (fm Frontmatter) IsStale(now time.Time) bool {
	if fm.StaleAfter == "" {
		return false
	}
	staleAfter, err := time.Parse("2006-01-02", fm.StaleAfter)
	if err != nil {
		return false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return !today.Before(staleAfter)
}

// Page represents a single wiki page with normalised metadata.
type Page struct {
	ID    string      `json:"id"`
	Path  string      `json:"path"`
	Title string      `json:"title"`
	Body  string      `json:"body"`
	Front Frontmatter `json:"front"`
}

// ErrNotFound is returned by Load when a page does not exist.
var ErrNotFound = errors.New("page not found")

// ErrPageTooLarge is returned by Load (and causes List to skip the page
// with a stderr warning — see the loadByPath call in List) when a
// page's file size exceeds maxPageSize.
var ErrPageTooLarge = errors.New("page exceeds maximum size")

// maxPageSize bounds how large a single page's file may be before
// loadByPath refuses to read it whole into memory. fs.ReadFile (the
// previous implementation) has no size limit of its own: with a
// runtime --kb-dir, the content root is a live, operator-writable
// filesystem, and every List() call — plus the search index build that
// both `http serve` and `mcp serve` run once at startup — reads every
// page's full body. A single 1 GiB markdown file (e.g. an accidentally
// committed data dump, or a binary misplaced under wiki/ with a .md
// extension) was measured driving `mk list` to ~2 GB RSS and the index
// build to ~5 GB RSS / 66s, both scaling linearly with however large a
// file someone manages to place in the tree.
//
// No real wiki page approaches this: even a generous hand-authored page
// with embedded tables or ASCII diagrams runs to a few hundred KB.
// 8 MiB leaves more than an order of magnitude of headroom above that
// while still keeping worst-case per-page memory small enough for a
// laptop-scale deployment to absorb without noticing — including
// transiently to search.New(), where every page in the KB is held at
// once while the bleve index is built.
const maxPageSize = 8 << 20 // 8 MiB

// FS returns the filesystem List/Load currently read from — the
// embedded content by default, or a disk directory after UseFS.
func FS() fs.FS { return loadFS() }

// List returns all wiki pages, sorted by ID for deterministic output.
//
// A completely missing content root (e.g. a --kb-dir directory whose
// wiki/ subdirectory doesn't exist) degrades to an empty slice rather
// than an error, mirroring sources.All's treatment of a missing
// sources.yaml: a partially-populated runtime directory should serve
// what it has, not hard-fail.
func List() ([]Page, error) { return ListFS(loadFS()) }

// ListFS is List against an explicit filesystem instead of the
// process-wide one UseFS sets. It exists for multi-collection serving
// (internal/collections), where several content roots are mounted at
// once and there is no single "current" filesystem to be redirected to:
// each collection holds its own fs.FS and reads through it directly.
// List is ListFS(the UseFS filesystem).
func ListFS(fsys fs.FS) ([]Page, error) {
	var pages []Page
	err := fs.WalkDir(fsys, "content", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip anything that isn't a regular file. With a runtime
		// --kb-dir this walks a live filesystem, so the tree can contain
		// FIFOs, sockets and device nodes. Opening a FIFO blocks until a
		// writer appears, which never happens — one stray pipe under
		// wiki/ would otherwise hang `mk list` and stop `http serve` and
		// `mcp serve` from ever binding, with no diagnostic.
		if !d.Type().IsRegular() {
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		if isExcluded(p) {
			return nil
		}
		pg, err := loadByPath(fsys, p)
		if err != nil {
			if errors.Is(err, errReservedArtifact) {
				// An OKF navigation artifact (see isReservedArtifact) —
				// expected, not a data problem, so skip quietly with no
				// stderr warning (unlike the unreadable-entry case below).
				return nil
			}
			// One unreadable entry must not take out the whole KB.
			// A broken symlink, a permission error or a symlink loop
			// aborts fs.WalkDir otherwise, so a single bad file denies
			// every page and prevents the servers from starting.
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "meerkat: skipping %s: %v\n", p, err)
			}
			return nil
		}
		pages = append(pages, pg)
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].ID < pages[j].ID })
	return pages, nil
}

// Load returns the page for the given ID. ID is the slash-separated
// path from the wiki root without the .md suffix. Both leading slash
// and the .md suffix are tolerated.
func Load(id string) (Page, error) { return LoadFS(loadFS(), id) }

// LoadFS is Load against an explicit filesystem instead of the
// process-wide one UseFS sets — the Load counterpart of ListFS, and
// there for the same reason (see ListFS).
func LoadFS(fsys fs.FS, id string) (Page, error) {
	id = normaliseID(id)
	p := path.Join("content", id+".md")
	// path.Join cleans "..", so an id of "../etc/prompts/x" collapses to
	// "etc/prompts/x" — exactly cancelling the "content" segment and
	// escaping the wiki subtree into the ingestion prompts and templates
	// the sources package serves. Re-assert the prefix after the join
	// rather than trusting a well-formed id to keep it.
	if p != "content" && !strings.HasPrefix(p, "content/") {
		return Page{}, ErrNotFound
	}
	if isExcluded(p) {
		return Page{}, ErrNotFound
	}
	page, err := loadByPath(fsys, p)
	if errors.Is(err, errReservedArtifact) {
		// A reserved OKF navigation artifact (see isReservedArtifact) is
		// indistinguishable from a missing page to a direct Load caller.
		return Page{}, ErrNotFound
	}
	return page, err
}

// FilterFunc selects pages from a slice; nil keeps everything.
type FilterFunc func(Page) bool

// Filter returns pages where keep(p) is true.
func Filter(pages []Page, keep FilterFunc) []Page {
	if keep == nil {
		return pages
	}
	out := pages[:0:0]
	for _, p := range pages {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}

// ByCategory is a FilterFunc preset.
func ByCategory(cat string) FilterFunc {
	return func(p Page) bool { return p.Front.Category == cat }
}

// ByStatus is a FilterFunc preset.
func ByStatus(status string) FilterFunc {
	return func(p Page) bool { return p.Front.Status == status }
}

// ByOwner is a FilterFunc preset.
func ByOwner(owner string) FilterFunc {
	return func(p Page) bool { return p.Front.Owner == owner }
}

// ByType is a FilterFunc preset matching the OKF `type` field (SPEC.md
// §4.1) — OKF's only required frontmatter key, and useful as a filter
// facet independent of OKF (e.g. a meerkat KB that adopts `type` on its
// own pages). See kb.Frontmatter.Type.
func ByType(t string) FilterFunc {
	return func(p Page) bool { return p.Front.Type == t }
}

// ByPrefix matches pages whose ID starts with the given prefix.
func ByPrefix(prefix string) FilterFunc {
	if prefix == "" {
		return nil
	}
	return func(p Page) bool { return strings.HasPrefix(p.ID, prefix) }
}

// reservedOKFFilenames are OKF navigation artifacts, reserved at every
// level of a bundle's hierarchy and which "MUST NOT be used for
// concept documents" (SPEC.md §3.1): index.md is a directory listing
// (§8), log.md a change history (§9). Per §8, an index.md/log.md
// carries no frontmatter at all — the sole exception, a bundle-root
// index.md's optional okf_version key, is deliberately not
// special-cased here; see isReservedArtifact.
//
// meerkat predates OKF and already uses "index.md" as an ordinary,
// frontmatter-bearing landing page (id:/title:) in its own KBs. Keying
// the skip off the filename alone would silently stop indexing every
// existing meerkat KB's index.md. Keying it off frontmatter PRESENCE
// instead lets both worlds coexist: a reserved-named file with no
// frontmatter is an OKF artifact (skip it, see isReservedArtifact);
// the same name WITH frontmatter is a meerkat page like any other
// (index it, exactly as before this change).
var reservedOKFFilenames = map[string]bool{
	"index.md": true,
	"log.md":   true,
}

// isReservedArtifact reports whether p (a "content/..."-rooted path) is
// an OKF reserved-filename navigation artifact rather than a loadable
// concept: its basename is index.md/log.md AND it has no frontmatter
// block. path.Base makes this apply at any directory depth, per §3.1.
//
// A bundle-root index.md that opts into OKF's optional okf_version key
// (SPEC.md §12) would have a frontmatter block and therefore NOT be
// skipped by this rule. That's a deliberate trade-off, not an
// oversight: no published OKF sample bundle sets okf_version (it's
// optional and unused in practice), so there is nothing to validate
// detection logic against and no known producer to serve; such a file
// is simply indexed as an ordinary (if unusual) page instead.
func isReservedArtifact(p string, hasFrontmatter bool) bool {
	return reservedOKFFilenames[path.Base(p)] && !hasFrontmatter
}

// errReservedArtifact is loadByPath's internal signal that a path
// matched isReservedArtifact. It never escapes this package: List
// treats it as an expected, quiet skip (no stderr warning — unlike
// other loadByPath errors, this isn't a data problem); Load maps it to
// the public ErrNotFound, since a reserved artifact is indistinguishable
// from a missing page to a direct caller.
var errReservedArtifact = errors.New("reserved OKF navigation artifact")

func loadByPath(fsys fs.FS, p string) (Page, error) {
	body, err := readCapped(fsys, p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Page{}, ErrNotFound
		}
		return Page{}, err
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, "content/"), ".md")
	front, bodyOnly, hasFrontmatter := splitFrontmatter(string(body))
	if isReservedArtifact(p, hasFrontmatter) {
		return Page{}, errReservedArtifact
	}
	if front.ID == "" {
		front.ID = id
	}
	title := front.Title
	if title == "" {
		title = extractTitle(bodyOnly, id)
	}
	return Page{
		ID:    id,
		Path:  p,
		Title: title,
		Body:  bodyOnly,
		Front: front,
	}, nil
}

// readCapped reads p from fsys, refusing (ErrPageTooLarge) rather than
// buffering the whole thing if it exceeds maxPageSize.
//
// It stats first: cheap, and gives an exact size for the error message
// on any fs.FS that reports one accurately. The read itself is still
// capped via io.LimitReader (reading one byte past the cap, so a file
// that's exactly at the cap succeeds and anything larger is caught
// deterministically) regardless of whether Stat succeeded — the
// authoritative enforcement doesn't depend on the filesystem
// implementing an accurate (or any) Stat, only on fs.File.Stat, which
// every fs.File must implement.
func readCapped(fsys fs.FS, p string) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	if info, statErr := f.Stat(); statErr == nil && info.Size() > maxPageSize {
		return nil, fmt.Errorf("%w: %s is %d bytes (cap %d)", ErrPageTooLarge, p, info.Size(), maxPageSize)
	}

	body, err := io.ReadAll(io.LimitReader(f, maxPageSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPageSize {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrPageTooLarge, p, maxPageSize)
	}
	return body, nil
}

// coreKeys is the set of YAML top-level keys that are part of the typed
// Frontmatter core. Any key NOT in this set ends up in Frontmatter.Extra.
//
// type/description/generated/verified/stale_after are OKF (SPEC.md §4.1,
// §5) fields promoted into the core; resource/sources/okf_version are
// deliberately NOT here, so they keep landing in Extra.
var coreKeys = map[string]bool{
	"id":             true,
	"title":          true,
	"type":           true,
	"description":    true,
	"category":       true,
	"subcategory":    true,
	"owner":          true,
	"status":         true,
	"tags":           true,
	"related":        true,
	"source":         true,
	"last_ingested":  true,
	"language":       true,
	"failure_reason": true,
	"generated":      true,
	"verified":       true,
	"stale_after":    true,
}

// splitFrontmatter returns parsed frontmatter, the body with any
// frontmatter block removed, and whether a frontmatter block was
// present and successfully parsed. If no frontmatter is present — or a
// block opens but never closes, or the YAML fails to parse — the input
// is returned unchanged as the body, Frontmatter{} is returned, and
// present is false.
//
// present is what lets a caller distinguish "no frontmatter at all"
// from "frontmatter present but every field happens to be empty" — see
// isReservedArtifact, which relies on exactly that distinction to skip
// OKF's frontmatter-less index.md/log.md navigation artifacts (SPEC.md
// §3.1, §8, §9) without mis-skipping a meerkat page that reuses one of
// those filenames but carries real frontmatter (id:/title:).
//
// Unknown top-level keys (those not in the engine core) are collected
// into Frontmatter.Extra so no data is lost.
func splitFrontmatter(content string) (fm Frontmatter, body string, present bool) {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return fm, content, false
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var (
		yamlLines []string
		bodyStart int
		seenOpen  bool
		closed    bool
	)
	pos := 0
	for scanner.Scan() {
		line := scanner.Text()
		pos += len(line) + 1 // +1 for the newline
		if !seenOpen {
			if strings.TrimSpace(line) == "---" {
				seenOpen = true
				continue
			}
			break
		}
		if strings.TrimSpace(line) == "---" {
			closed = true
			bodyStart = pos
			break
		}
		yamlLines = append(yamlLines, line)
	}
	if !closed {
		return fm, content, false
	}
	raw := []byte(strings.Join(yamlLines, "\n"))
	if err := yaml.Unmarshal(raw, &fm); err != nil {
		// On parse error, treat as no frontmatter rather than fail
		// the whole page load. The lint task can flag invalid YAML.
		return Frontmatter{}, content, false
	}
	// Second pass: collect all top-level keys that are not in the core
	// into fm.Extra so deployment-specific fields are preserved.
	var allFields map[string]any
	if err := yaml.Unmarshal(raw, &allFields); err == nil {
		extra := make(map[string]any, len(allFields))
		for k, v := range allFields {
			if !coreKeys[k] {
				extra[k] = v
			}
		}
		if len(extra) > 0 {
			fm.Extra = extra
		}
	}
	if bodyStart > len(content) {
		bodyStart = len(content)
	}
	return fm, content[bodyStart:], true
}

// extractTitle returns the first markdown heading in body, falling
// back to the page ID if no heading is present.
func extractTitle(body, fallback string) string {
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "#") {
			t := strings.TrimSpace(strings.TrimLeft(trim, "#"))
			if t != "" {
				return t
			}
		}
	}
	return fallback
}

func isExcluded(p string) bool {
	for _, ex := range excludedFiles {
		if p == ex {
			return true
		}
	}
	return false
}

func normaliseID(id string) string {
	id = strings.TrimPrefix(id, "/")
	id = strings.TrimSuffix(id, ".md")
	return id
}

// MarshalFrontmatter renders a Frontmatter as a `---`-delimited
// YAML block including trailing newline. Used by ingestion tools
// to rewrite a page's metadata without touching the body.
func MarshalFrontmatter(fm Frontmatter) (string, error) {
	out, err := yaml.Marshal(fm)
	if err != nil {
		return "", err
	}
	return "---\n" + string(out) + "---\n", nil
}
