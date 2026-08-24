// Package collections mounts one or more named knowledge-base
// collections at once and routes search/show/list across them.
//
// A collection is a name plus a resolved content root (a local
// directory, an extracted HTTPS archive, a GCS bundle or prefix, or the
// build-time embed) plus, lazily, a search index over that root. The
// Registry owns them and implements the routing rules every surface —
// CLI, MCP, HTTP — shares:
//
//	collection given      -> exactly that collection; an unknown name is
//	                         an error naming the ones that exist.
//	collection omitted:
//	  search              -> every collection, results merged and
//	                         re-sorted by score, then truncated to limit.
//	  list                -> every collection, in configuration order.
//	  show                -> every collection in configuration order. A
//	                         page ID found in exactly one is returned; an
//	                         ID present in several is an ambiguity ERROR
//	                         naming the qualified alternatives, never a
//	                         silent pick.
//
// A page is addressable as "<collection>:<page-id>" (a qualified ID)
// anywhere a page ID is accepted, which is what makes the ambiguity
// error actionable. Page IDs themselves are never rewritten: every
// result carries its collection alongside the page's own, unqualified
// ID, so anything that consumes an ID (ingest, a bookmark, a link)
// keeps working.
//
// A registry with exactly one collection behaves exactly as meerkat did
// before collections existed: nothing to name, nothing to disambiguate,
// unqualified IDs everywhere. That is the case every pre-existing
// configuration resolves to.
//
// # Two narrowings
//
// A per-request VIEW of a registry can be narrowed twice, and the two
// are independent:
//
//	Restrict(allow)   which COLLECTIONS exist for this caller
//	ViewedBy(viewer)  which PAGES exist inside them
//
// Both return a derived registry that borrows the underlying
// *Collection values, and both are applied ONCE per request so that
// every read below them inherits the answer without knowing it exists —
// see Restrict's doc comment for why filtering upstream of the
// operations is strictly stronger than a check inside each. ViewedBy is
// there for personal memories, which are readable only by the principal
// whose namespace they are in (see internal/kb's Viewer and
// docs/design/memory.md). A registry with no viewer attached reads
// unrestricted, which is what every single-principal surface gets by
// never calling it.
package collections

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/search"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// DefaultName is the name of the single collection a single-source
// config (or no config at all) resolves to.
const DefaultName = contentsource.DefaultCollectionName

// ErrUnknownCollection is returned when a requested collection name is
// not mounted. Errors wrapping it name the collections that are.
var ErrUnknownCollection = errors.New("unknown collection")

// ErrAmbiguous is returned by Show when a bare page ID exists in more
// than one mounted collection and no collection was named.
var ErrAmbiguous = errors.New("ambiguous page id")

// Collection is one mounted knowledge-base collection.
type Collection struct {
	// Name addresses the collection (--collection, the MCP tools'
	// collection argument, the "<name>:" qualified-ID prefix).
	Name string
	// Source is the resolved content-source.yaml entry, mainly for its
	// Type (surfaced by `mk list --collections`) and — since runtime
	// reconciliation — for the refresh policy and the coordinates a
	// re-resolve needs. See reload.go.
	Source contentsource.Source

	// snapMu guards snap. A read lock is taken for exactly as long as it
	// takes to read the pointer and add a reference; the write lock only
	// by a snapshot swap. See acquire/install in reload.go.
	snapMu sync.RWMutex
	// snap is the CONTENT SNAPSHOT currently serving: the filesystem
	// pages are read from, the provenance naming exactly which bytes
	// those are, and the search index built over them, as one coherent
	// unit that is replaced whole or not at all.
	//
	// Never nil after construction. Every read acquires it once, so a
	// single operation can never see one half of an old generation and
	// one half of a new one.
	snap *snapshot

	// pages/byID back a FromPages collection: a fixed, in-memory page
	// set with no filesystem behind it at all. Non-nil byID is what
	// marks the collection as such.
	pages []kb.Page
	byID  map[string]kb.Page

	// memory is the collection's writable memory store, when one is
	// configured. nil means the collection is read-only and
	// mk_save_memory will not name it.
	memory memory.Store
	// personalReadsAreCollectionWide is the collection's
	// `memory.personal_visibility: collection` opt-out: personal
	// memories here are readable by every reader of the collection, as
	// they were before per-page visibility existed.
	//
	// The zero value is the SECURE one. A collection built by any path
	// that does not explicitly configure the legacy behaviour — including
	// FromPages, and every configuration written before the key existed —
	// keeps personal memories private.
	//
	// Written once at mount (Open -> SetPersonalVisibility), before
	// anything serves, and read-only thereafter — the same lifecycle as
	// Name, and the reason it needs no lock while overlay, memory and
	// the content snapshot (which requests and refreshes mutate) do.
	//
	// Runtime reconciliation does NOT touch it, and that is deliberate:
	// a refresh swaps the snapshot INSIDE this *Collection rather than
	// replacing the *Collection, so every field outside the snapshot —
	// this one above all — keeps its mount-time value. Nothing carries
	// it across a rebuild, because nothing has to. If it ever does become
	// mutable at runtime, it needs a lock or an atomic; see
	// docs/design/hot-reload.md.
	personalReadsAreCollectionWide bool
	// overlayMu guards overlay.
	overlayMu sync.RWMutex
	// overlay holds the memory documents layered over the collection's
	// content root, by page ID. It is populated from the memory store at
	// mount time and extended by every successful write — see
	// SaveMemory.
	//
	// An overlay rather than "write into the wiki tree and re-read it"
	// because the memory store deliberately lives OUTSIDE the served
	// content root (so a staged, unreviewed document can never be picked
	// up by kb.ListFS), and because it works identically for a GCS-backed
	// store, where there is no local tree to write into at all.
	overlay map[string]kb.Page

	// writeMu serialises a LIVE index write (SaveMemory) against a
	// snapshot swap, and pending records the writes that happened while a
	// replacement index was being built off the request path. Both exist
	// only because of runtime reconciliation; see reload.go, where the
	// lost-write race they close is spelled out.
	writeMu sync.Mutex
	pending map[string]kb.Page

	// reloadMu is the collection's single reload slot. Held for the whole
	// of one reconciliation cycle — content or memory — so two cycles can
	// never stage against the same collection at once.
	reloadMu sync.Mutex

	// status is the reconciliation state reported through readiness,
	// metrics and `mk list --collections`.
	status reloadState

	// rootOnce/rootSeen latch whether the collection's content root was
	// reachable the first time its health was checked — at startup, for
	// a server, which calls Check once before it begins serving. See
	// contentRootReachable for why a latched baseline is the only way to
	// tell "this deployment has no wiki/ and never did" (legitimate,
	// documented, serves empty) from "the volume backing this
	// collection went away" (a real outage).
	rootOnce sync.Once
	rootSeen bool
}

// FromPages builds a collection over an explicit set of pages rather
// than a filesystem — the injectable counterpart of search.NewFromPages,
// for callers (and tests) that already hold the pages they want mounted.
// The set is fixed for the collection's lifetime.
func FromPages(name string, pages []kb.Page) *Collection {
	byID := make(map[string]kb.Page, len(pages))
	for _, p := range pages {
		byID[p.ID] = p
	}
	sorted := make([]kb.Page, len(pages))
	copy(sorted, pages)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	c := newCollection(name, "memory", "", nil)
	c.pages, c.byID = sorted, byID
	return c
}

// newCollection builds a collection with its first content snapshot
// installed. Every construction path goes through it, so there is no way
// to end up with a collection whose snapshot pointer is nil.
//
// version is the source version token the snapshot was resolved at (an
// object generation, a prefix listing fingerprint), or "" for a source
// that has none. It is what the first reconciliation probe compares
// against, so a replica that started on the current generation does no
// work at all on its first tick.
func newCollection(name, provenance, version string, fsys fs.FS) *Collection {
	c := &Collection{Name: name}
	c.install(&snapshot{fsys: fsys, provenance: provenance, version: version})
	return c
}

// Provenance is what `mk version` reports for this collection:
// "embedded", "disk:<path>", "url:<url>@<digest>", or
// "gcs://<bucket>/<object>@<generation>".
//
// It is read from the CURRENT snapshot rather than latched at mount,
// because a refreshable collection's provenance is the whole point of
// the string: after a reconciliation swapped a new GCS generation in,
// "which bytes is this replica serving?" must answer the new generation,
// not the one the process happened to start on.
func (c *Collection) Provenance() string {
	c.snapMu.RLock()
	defer c.snapMu.RUnlock()
	return c.snap.provenance
}

// Type reports the collection's backend source type ("local", "gcs",
// "url", ...), or contentsource.TypeNone when there is no
// content-source.yaml entry behind it at all — the embedded build, or a
// --kb-dir directory, neither of which carries a type of its own.
//
// Every surface that enumerates collections (`mk list --collections`,
// GET /collections, mk_list_collections) reports this exact value, so a
// caller sees the same "type" whichever one it asks.
func (c *Collection) Type() string {
	if c.Source.Type != "" {
		return c.Source.Type
	}
	return contentsource.TypeNone
}

// Pages returns EVERY page in the collection, sorted by ID: the content
// root's pages with the memory overlay merged in, private ones
// included.
//
// It is the unrestricted form, and it exists for the three callers that
// legitimately need the whole set: the search-index build (the index
// must contain every document — visibility is applied at query time, not
// at index time), the readiness check's page count, and single-user
// surfaces with no principals to distinguish. Anything serving several
// principals must go through the registry, whose Pages applies the
// per-request viewer — see Registry.Pages and PagesFor.
//
// Deliberately not cached: with a runtime content directory the content
// root is live (an `mk ingest` run, a redeploy writing into a mounted
// volume), and POST /list on a long-running server has always reflected
// that immediately. The search index, which is expensive to build, is
// the thing that's built once — see Index.
func (c *Collection) Pages() ([]kb.Page, error) { return c.PagesFor(kb.Unfiltered()) }

// PagesFor is Pages as seen by v: the same list with every page v may
// not see removed, before any caller has had a chance to count them.
func (c *Collection) PagesFor(v kb.Viewer) ([]kb.Page, error) {
	snap := c.acquire()
	defer snap.release()
	return c.pagesOf(snap, v)
}

// pagesOf is PagesFor against one already-acquired snapshot, so a caller
// that needs the pages AND the index sees one coherent generation of
// both.
func (c *Collection) pagesOf(s *snapshot, v kb.Viewer) ([]kb.Page, error) {
	pages, err := c.contentPagesFrom(s.fsys)
	if err != nil {
		return nil, err
	}
	return c.mergeOverlay(pages, c.viewerFor(v)), nil
}

// viewerFor applies the collection's own visibility policy to a
// request's viewer.
//
// A collection configured `personal_visibility: collection` widens every
// viewer to an unfiltered one: its personal memories are readable by
// every reader of the collection, which is exactly the pre-#27
// behaviour and exactly what that key asks for. Any other collection —
// including every one that configures nothing — passes the viewer
// through untouched.
func (c *Collection) viewerFor(v kb.Viewer) kb.Viewer {
	if c.personalReadsAreCollectionWide {
		return kb.Unfiltered()
	}
	return v
}

// PersonalReadsAreCollectionWide reports whether this collection opted
// out of private personal reads (`memory.personal_visibility:
// collection`). The hosted server warns about it at startup when OIDC is
// configured; nothing else needs to ask.
func (c *Collection) PersonalReadsAreCollectionWide() bool {
	return c.personalReadsAreCollectionWide
}

// SetPersonalVisibility applies a `memory.personal_visibility:` value.
// Anything other than memory.VisibilityCollection — including the empty
// string, and including a value that somehow escaped validation — leaves
// personal memories private, so the failure direction is closed.
func (c *Collection) SetPersonalVisibility(v string) {
	c.personalReadsAreCollectionWide = v == memory.VisibilityCollection
}

// contentPagesFrom returns the pages of one content root, with no memory
// overlay. fsys is a snapshot's filesystem: nil means "read through the
// process-global kb filesystem", the state a single-collection
// deployment starts in.
func (c *Collection) contentPagesFrom(fsys fs.FS) ([]kb.Page, error) {
	switch {
	case c.byID != nil:
		return c.pages, nil
	case fsys == nil:
		return kb.List()
	default:
		return kb.ListFS(fsys)
	}
}

// mergeOverlay layers the memory overlay over a content page list,
// keeping only what v may see. A memory whose ID collides with a content
// page WINS: the overlay is the live, just-written state, and a content
// page that shadowed it would make a save look like it silently did
// nothing.
//
// Visibility is applied to both halves, and shadowing is conditioned on
// the overlay entry actually being visible — so a private overlay
// document cannot make a public content page disappear for everybody
// else by sitting on its ID.
func (c *Collection) mergeOverlay(pages []kb.Page, v kb.Viewer) []kb.Page {
	c.overlayMu.RLock()
	defer c.overlayMu.RUnlock()
	return mergeOverlayMap(pages, c.overlay, v)
}

// mergeOverlayMap is mergeOverlay over an EXPLICIT overlay map rather
// than the collection's live one.
//
// It exists for memory reconciliation, which has to build a replacement
// index over an overlay it has loaded but not yet installed — see
// reload.go. Factoring it out rather than duplicating the merge is the
// point: the shadowing and visibility rules above are the ones #27
// established, and a second copy of them is a second place for them to
// drift.
func mergeOverlayMap(pages []kb.Page, overlay map[string]kb.Page, v kb.Viewer) []kb.Page {
	if len(overlay) == 0 {
		return v.VisiblePages(pages)
	}
	out := make([]kb.Page, 0, len(pages)+len(overlay))
	for _, p := range pages {
		if op, shadowed := overlay[p.ID]; shadowed && v.CanSee(op) {
			continue
		}
		if !v.CanSee(p) {
			continue
		}
		out = append(out, p)
	}
	for _, p := range overlay {
		if !v.CanSee(p) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LoadFor is Load as seen by v: a page v may not see answers
// kb.ErrNotFound, the identical error a page that does not exist
// answers.
//
// Both halves matter. The ID is checked BEFORE the load, so an
// unauthorized lookup does no work and touches no store — there is no
// timing or error-shape difference between guessing a real private ID
// and guessing a fictional one. The loaded page is then checked again,
// because the ID a caller passed and the page that came back are two
// different things and only the second one is authoritative.
//
// Returning ErrNotFound rather than a refusal is the whole point:
// internal/collections' invisibility rule says an unauthorized thing is
// absent, not denied, and a distinguishable error here would turn
// mk_show into a per-page existence oracle over other people's memories.
func (c *Collection) LoadFor(v kb.Viewer, id string) (kb.Page, error) {
	v = c.viewerFor(v)
	norm := strings.TrimSuffix(strings.TrimPrefix(id, "/"), ".md")
	if !v.CanSeeOwner(kb.PrivateOwner(norm)) {
		return kb.Page{}, kb.ErrNotFound
	}
	page, err := c.Load(norm)
	if err != nil {
		return kb.Page{}, err
	}
	if !v.CanSee(page) {
		return kb.Page{}, kb.ErrNotFound
	}
	return page, nil
}

// Load returns one page by its unqualified ID, the memory overlay
// first. It applies no visibility policy — see LoadFor, and
// Collection.Pages for why the unrestricted form exists.
func (c *Collection) Load(id string) (kb.Page, error) {
	norm := strings.TrimSuffix(strings.TrimPrefix(id, "/"), ".md")
	if p, ok := c.overlayPage(norm); ok {
		return p, nil
	}
	if c.byID != nil {
		p, ok := c.byID[norm]
		if !ok {
			return kb.Page{}, kb.ErrNotFound
		}
		return p, nil
	}
	snap := c.acquire()
	defer snap.release()
	if snap.fsys == nil {
		return kb.Load(id)
	}
	return kb.LoadFS(snap.fsys, id)
}

// overlayPage reads one page out of the memory overlay.
func (c *Collection) overlayPage(id string) (kb.Page, bool) {
	c.overlayMu.RLock()
	defer c.overlayMu.RUnlock()
	p, ok := c.overlay[id]
	return p, ok
}

// Index returns the collection's search index, building it on first use
// and reusing it thereafter (bleve in-memory indexes are safe for
// concurrent reads). Building lazily matters for multi-collection
// deployments: `mk list` and `mk show` must not pay to index every
// mounted collection, and `mk search --collection x` must not pay to
// index the others.
//
// It returns the index of the snapshot serving RIGHT NOW, and holds no
// reference to it: a reconciliation that swaps a new snapshot in may
// close the returned index once its last in-flight reader is done. That
// is safe for the two things this method is for — warming an index at
// startup (internal/mcp's indexAll) and asking whether one builds at all
// (health) — and it is not safe for running a query.
//
// Anything that actually USES an index must go through searchAs or
// publishMemory, which hold a reference for the duration. See reload.go.
func (c *Collection) Index() (*search.Index, error) {
	snap := c.acquire()
	defer snap.release()
	return c.indexOf(snap)
}

// IndexContext is Index with a context, so a build that actually happens
// is traced and timed.
//
// The context is used for instrumentation and nothing else — a bleve
// build is not cancellable and pretending otherwise would be a lie in
// the signature. It exists because the startup build (internal/mcp's
// indexAll) is often the slowest thing a hosted server does, and it was
// the one part of startup a trace could not see.
//
// The span records how many documents were indexed and nothing about
// them.
func (c *Collection) IndexContext(ctx context.Context) (*search.Index, error) {
	snap := c.acquire()
	defer snap.release()
	if snap.built() {
		// Already built: nothing to time, and emitting a zero-duration
		// span per call would bury the one build that mattered.
		return c.indexOf(snap)
	}
	ctx, span := telemetry.Span(ctx, telemetry.SpanIndexBuild)
	started := time.Now()
	idx, err := c.indexOf(snap)
	outcome := telemetry.OutcomeOK
	if err != nil {
		outcome = telemetry.OutcomeError
	}
	telemetry.Record(ctx).IndexBuilt(outcome, time.Since(started).Seconds())
	if err != nil {
		// The error names the collection ("collection %q: ..."), so it is
		// classified rather than recorded.
		telemetry.Fail(span, outcome)
		return nil, err
	}
	span.SetAttributes(telemetry.Outcome(outcome), telemetry.KeyIndexPages.Int(c.snapshotPages(snap)))
	span.End()
	return idx, nil
}

// snapshotPages counts what went into a snapshot's index, for the build
// span. Best-effort: a snapshot whose enumeration failed reports zero
// rather than propagating an error into instrumentation.
func (c *Collection) snapshotPages(s *snapshot) int {
	pages, err := c.pagesOf(s, kb.Unfiltered())
	if err != nil {
		return 0
	}
	return len(pages)
}

// Close releases the collection's index, if one was built, and its
// memory store, if one is attached.
//
// The snapshot is REPLACED rather than cleared: a collection whose
// pointer went nil would panic on the next read, and Close is reachable
// while a request is in flight (a derived registry's Close is a no-op,
// but the owning registry's is not). The replacement carries the same
// filesystem and provenance with an unbuilt index, which is exactly the
// state this method used to leave behind when the index was a plain
// field.
func (c *Collection) Close() error {
	var firstErr error
	c.overlayMu.Lock()
	store := c.memory
	c.memory = nil
	c.overlayMu.Unlock()
	if closer, ok := store.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			firstErr = err
		}
	}

	c.snapMu.Lock()
	old := c.snap
	c.snap = &snapshot{fsys: old.fsys, provenance: old.provenance, version: old.version}
	c.snap.refs.Store(1)
	c.snapMu.Unlock()
	// Drops the collection's own reference; the index closes once the
	// last in-flight reader releases it, and its error surfaces here when
	// that reader is us (the usual case at shutdown).
	if err := old.releaseErr(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// --- memory ----------------------------------------------------------

// Memory returns the collection's writable memory store, or nil when
// none is configured. A nil store is what makes a collection read-only
// as far as mk_save_memory is concerned.
//
// Read under the overlay lock, which also guards the store pointer: it
// is written once at mount (AttachMemory) and cleared once at shutdown
// (Close), both potentially concurrent with a request in flight.
func (c *Collection) Memory() memory.Store {
	c.overlayMu.RLock()
	defer c.overlayMu.RUnlock()
	return c.memory
}

// AttachMemory wires a memory store to the collection and loads its
// LIVE documents into the overlay, so memories saved by an earlier
// process are searchable and showable from this one's first request.
//
// It is called once, at mount time, before anything serves. Documents
// that fail to parse are skipped with a warning rather than failing the
// mount: one malformed memory must not make a whole collection
// unserveable.
func (c *Collection) AttachMemory(ctx context.Context, store memory.Store) error {
	if store == nil {
		return nil
	}
	records, err := store.Load(ctx)
	if err != nil {
		return fmt.Errorf("collection %q: load memory store: %w", c.Name, err)
	}
	pages := make(map[string]kb.Page, len(records))
	for _, rec := range records {
		page, perr := memory.Page(rec.Key, rec.Body)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "meerkat: skipping memory %s in collection %q: %v\n", rec.Key, c.Name, perr)
			continue
		}
		pages[page.ID] = page
	}
	c.overlayMu.Lock()
	c.memory = store
	c.overlay = pages
	c.overlayMu.Unlock()
	return nil
}

// SaveMemory stores one memory document and makes it immediately
// visible: it lands in the backend under an optimistic-locking
// precondition, then — as one step — in the overlay and in the live
// search index.
//
// Order matters. The durable write comes first, so nothing is
// searchable that was not stored; the publish comes last, so a conflict
// costs nothing. The index is FORCED to build if it hasn't yet: a save
// has to leave the collection in a state where the next search finds the
// memory, and a lazily-unbuilt index would otherwise be built later from
// Pages() — which does include the overlay, so either order is correct,
// but forcing it keeps "saved" and "searchable" one step apart instead
// of two.
//
// The overlay and the index are updated together, under one lock, so a
// concurrent reconciliation swap cannot land between them — see
// publishMemory.
func (c *Collection) SaveMemory(ctx context.Context, key string, body []byte, pre memory.Precondition) (memory.Version, kb.Page, error) {
	store := c.Memory()
	if store == nil {
		return "", kb.Page{}, fmt.Errorf("collection %q has no memory store configured", c.Name)
	}
	version, err := timedMemory(ctx, memory.Backend(store), telemetry.MemorySave, func() (memory.Version, error) {
		return store.Put(ctx, key, body, pre)
	})
	if err != nil {
		return "", kb.Page{}, err
	}
	page, err := memory.Page(key, body)
	if err != nil {
		// Stored but unparseable: report it rather than pretending the
		// save failed (it did not — the bytes are durable) or pretending
		// it fully succeeded (it is not searchable).
		return version, kb.Page{}, fmt.Errorf("memory was stored but could not be indexed: %w", err)
	}
	if err := c.publishMemory(page); err != nil {
		return version, page, err
	}
	return version, page, nil
}

// StageMemory writes a pending review artifact and returns where it
// landed. It touches neither the overlay nor the index: a staged
// document is a proposal, and a proposal that turned up in search
// results would be indistinguishable from an approved one.
func (c *Collection) StageMemory(ctx context.Context, key string, body []byte) (string, error) {
	store := c.Memory()
	if store == nil {
		return "", fmt.Errorf("collection %q has no memory store configured", c.Name)
	}
	return timedMemory(ctx, memory.Backend(store), telemetry.MemoryStage, func() (string, error) {
		return store.Stage(ctx, key, body)
	})
}

// timedMemory times one memory-store operation and records it under the
// two bounded labels the backend metrics carry.
//
// It records a SPAN as well as the metric, so a slow save is
// attributable to the object store rather than to meerkat, and the span
// says which backend and which operation and nothing else — not the key,
// not the bucket, not the object name. `backend` comes from
// memory.Backend and `operation` from internal/telemetry's closed set;
// there is no path by which either could become caller text.
func timedMemory[T any](ctx context.Context, backend, operation string, fn func() (T, error)) (T, error) {
	_, span := telemetry.Span(ctx, telemetry.SpanMemoryStore,
		telemetry.KeyMemoryBackend.String(backend),
		telemetry.KeyMemoryOperation.String(operation),
	)
	started := time.Now()
	v, err := fn()
	telemetry.Record(ctx).MemoryBackend(backend, operation, time.Since(started).Seconds(), err != nil)
	if err != nil {
		// Classified, not recorded: a GCS error can quote the object path
		// and a local one the filesystem path.
		telemetry.Fail(span, telemetry.OutcomeError)
		return v, err
	}
	span.SetAttributes(telemetry.Outcome(telemetry.OutcomeOK))
	span.End()
	return v, nil
}

// Registry is an ordered set of mounted collections. Configuration
// order is preserved and load-bearing: it is the order results are
// listed in and the order Show disambiguates in.
type Registry struct {
	list []*Collection
	by   map[string]*Collection
	// derived marks a registry produced by Restrict or ViewedBy: a
	// per-request VIEW over another registry's collections rather than an
	// owner of them. Close is a no-op on a derived registry, because the
	// *Collection values (and the built indexes inside them) belong to
	// the registry it was derived from and outlive this request.
	derived bool
	// viewer is WHO this view reads as, for per-page visibility. nil
	// means no per-page policy is in force — the same thing a nil
	// *authz.Grants means for collections, and the state the CLI, `mk
	// http serve` and every pre-existing caller are in.
	//
	// It is a pointer rather than a kb.Viewer so that "nobody set one"
	// (unrestricted, the back-compat answer) is distinguishable from
	// kb.AsOwner("") (a caller who owns nothing and may see no private
	// page at all). Collapsing the two would either break single-user
	// surfaces or silently unhide private pages from anonymous callers,
	// depending on which way the zero value fell.
	viewer *kb.Viewer
}

// PageRef is a page together with the collection it came from.
type PageRef struct {
	Collection string  `json:"collection"`
	Page       kb.Page `json:"page"`
}

// QualifiedID is the "<collection>:<page-id>" form of the reference —
// what an operator pastes back into `mk show` or mk_show to address
// this exact page unambiguously.
func (r PageRef) QualifiedID() string { return r.Collection + ":" + r.Page.ID }

// Hit is a search result together with the collection it came from.
type Hit struct {
	Collection string `json:"collection"`
	search.Result
}

// New builds a registry over cols, which must be non-empty and
// uniquely named.
func New(cols ...*Collection) (*Registry, error) {
	if len(cols) == 0 {
		return nil, errors.New("a registry needs at least one collection")
	}
	r := &Registry{list: cols, by: make(map[string]*Collection, len(cols))}
	for _, c := range cols {
		if c.Name == "" {
			return nil, errors.New("collection name must not be empty")
		}
		if _, dup := r.by[c.Name]; dup {
			return nil, fmt.Errorf("duplicate collection name %q", c.Name)
		}
		r.by[c.Name] = c
	}
	return r, nil
}

// Global returns a one-collection registry backed by the process-global
// kb filesystem — whatever internal/kbdir has been pointed at, or the
// build-time embed. It is the fallback for any surface that hasn't been
// handed a registry, so that code paths which predate collections (and
// tests that drive a subcommand directly with kb.UseFS) keep behaving
// exactly as they did.
func Global(provenance string) *Registry {
	c := newCollection(DefaultName, provenance, "", nil)
	return &Registry{list: []*Collection{c}, by: map[string]*Collection{DefaultName: c}}
}

// Open mounts every resolved collection, wiring each non-embedded one
// to its own content-repo-layout filesystem (via internal/kbdir's
// adapter, honouring that collection's own layout: block) and, for a
// collection with a `memory:` block, opening its writable memory store
// and loading the memories already in it.
//
// The single-collection case deliberately gets a nil filesystem: the
// caller has already pointed the process globals at that one content
// root, so the collection reads through them and every pre-existing
// code path (internal/sources, `mk ingest`, shell completion) keeps
// seeing exactly what it saw before.
//
// ctx bounds the memory stores' startup work (a GCS list + read).
// Opening them here rather than lazily is the same call the rest of
// this package makes about indexes on a server: a store that cannot be
// reached should fail the process at startup, where a deployment's own
// health gate catches it, not on the first save hours later.
func Open(ctx context.Context, resolved []contentsource.ResolvedCollection) (*Registry, error) {
	cols := make([]*Collection, 0, len(resolved))
	for _, rc := range resolved {
		var fsys fs.FS
		if len(resolved) > 1 && rc.Dir != "" {
			mounted, err := kbdir.FSLayout(rc.Dir, rc.Source.Layout)
			if err != nil {
				return nil, fmt.Errorf("collection %q: %w", rc.Name, err)
			}
			fsys = mounted
		}
		c := newCollection(rc.Name, rc.Provenance, rc.Version, fsys)
		c.Source = rc.Source
		c.status.configure(rc.Source)
		// Personal-read visibility is set even when there is no memory:
		// block (Spec.Visibility answers private for a nil Spec), because
		// the reserved page-ID prefix means the same thing in every
		// collection whether or not this one happens to accept writes.
		c.SetPersonalVisibility(rc.Source.Memory.Visibility())
		if rc.Source.Memory != nil {
			store, err := rc.Source.Memory.Open(ctx, rc.Dir)
			if err != nil {
				return nil, fmt.Errorf("collection %q: memory store: %w", rc.Name, err)
			}
			if err := c.AttachMemory(ctx, store); err != nil {
				return nil, err
			}
		}
		cols = append(cols, c)
	}
	return New(cols...)
}

// Len reports how many collections are mounted.
func (r *Registry) Len() int { return len(r.list) }

// Single reports whether exactly one collection is mounted — the case
// in which nothing needs qualifying or disambiguating.
func (r *Registry) Single() bool { return len(r.list) == 1 }

// All returns the mounted collections in configuration order.
func (r *Registry) All() []*Collection { return r.list }

// Names returns the mounted collection names in configuration order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.list))
	for _, c := range r.list {
		out = append(out, c.Name)
	}
	return out
}

// WithMemory returns a VIEW holding only the collections that have a
// writable memory store. Like Restrict it borrows rather than owns, so
// closing it is a no-op.
//
// Composing it with Restrict is what gives the memory tool its own
// visible set: a caller sees exactly the collections they may reach AND
// that can be written to. A deployment that configures no memory store
// at all ends up with an empty view, and the tool is not offered.
func (r *Registry) WithMemory() *Registry {
	return r.Restrict(func(name string) bool {
		c, ok := r.by[name]
		return ok && c.Memory() != nil
	})
}

// MemoryNames returns the names of the collections in this registry
// that have a memory store, in configuration order.
func (r *Registry) MemoryNames() []string { return r.WithMemory().Names() }

// Get returns the named collection, or an error wrapping
// ErrUnknownCollection that names the ones that exist.
//
// "the ones that exist" means the ones THIS registry holds. On a
// registry restricted to a caller's readable collections (see Restrict)
// that is the caller's own view, so asking for a collection they may
// not read produces the identical error to asking for one nobody has
// ever mounted — which is the point.
func (r *Registry) Get(name string) (*Collection, error) {
	if c, ok := r.by[name]; ok {
		return c, nil
	}
	if len(r.list) == 0 {
		return nil, fmt.Errorf("%w %q — no collections are mounted", ErrUnknownCollection, name)
	}
	return nil, fmt.Errorf("%w %q — available: %s", ErrUnknownCollection, name, strings.Join(r.Names(), ", "))
}

// Restrict returns a VIEW of r holding only the collections allow
// accepts, in the same configuration order. It is the single point at
// which authorization is applied.
//
// Filtering here rather than per-operation is what makes an
// unauthorized collection *invisible* rather than *denied*. Everything
// this type exposes — target (and so Search/Pages/Show), Get's
// "available: ..." list, Names, All, Len, Single, SplitQualified,
// Provenance — reads r.list/r.by and therefore sees exactly the allowed
// set. There is no path by which a filtered-out collection can be
// counted, named, disambiguated against, or reported as existing:
//
//   - Show's ambiguity error counts only visible matches, so it can't
//     be used to probe whether a page also exists somewhere the caller
//     can't read;
//   - Get's error names only visible collections, so a rejected name is
//     indistinguishable from a name nobody mounted;
//   - SplitQualified stops recognising "<hidden>:" as a qualification,
//     so a qualified ID for a hidden collection degrades to a bare page
//     ID and 404s rather than 403ing;
//   - a caller who can read one of several mounted collections gets
//     Single() == true and the unqualified, single-collection UX.
//
// The view shares the underlying *Collection values, so a restricted
// search reuses the already-built per-collection bleve indexes — no
// rebuild, which is exactly why the indexes are per-collection. It does
// not own them: Close on a derived registry is a no-op.
//
// A nil allow returns r unchanged, so "no policy in force" costs
// nothing.
func (r *Registry) Restrict(allow func(name string) bool) *Registry {
	if allow == nil {
		return r
	}
	out := &Registry{
		list:    make([]*Collection, 0, len(r.list)),
		by:      make(map[string]*Collection, len(r.list)),
		derived: true,
		viewer:  r.viewer,
	}
	for _, c := range r.list {
		if !allow(c.Name) {
			continue
		}
		out.list = append(out.list, c)
		out.by[c.Name] = c
	}
	return out
}

// ViewedBy returns a VIEW of r whose reads are performed as v: every
// page-returning operation below (Pages, Search, Show, and therefore the
// ambiguity error's count) drops the pages v may not see, before any
// caller sees a title, a snippet, an ID or a number.
//
// It is the per-page counterpart of Restrict, and it is deliberately the
// same shape: authorization is attached ONCE to a per-request view, and
// every operation downstream inherits it without knowing it exists.
// There is no per-operation viewer argument to forget, and no read path
// on this type that can bypass it — including any added later, so long
// as it routes through Pages/Search/Show.
//
// The two compose in either order: Restrict decides which collections
// are in view, ViewedBy decides which pages within them are.
//
// A registry with no viewer attached reads unrestricted, which is what
// every surface serving a single principal (the CLI, `mk http serve`)
// gets by never calling this.
func (r *Registry) ViewedBy(v kb.Viewer) *Registry {
	out := &Registry{
		list:    r.list,
		by:      r.by,
		derived: true,
		viewer:  &v,
	}
	return out
}

// viewer returns the viewer in force for reads through this registry.
func (r *Registry) viewerOf() kb.Viewer {
	if r.viewer == nil {
		return kb.Unfiltered()
	}
	return *r.viewer
}

// target resolves a collection argument to the collections to act on:
// all of them when name is empty, exactly one otherwise.
//
// Every read routes through here, over r.list — which on a restricted
// registry (see Restrict) is already the caller's visible set. That is
// the whole enforcement mechanism; no operation below re-checks
// anything.
func (r *Registry) target(name string) ([]*Collection, error) {
	if name == "" {
		return r.list, nil
	}
	c, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	return []*Collection{c}, nil
}

// Close releases every collection's index. It is a no-op on a registry
// produced by Restrict, which borrows its collections rather than
// owning them — closing a per-request view must not tear down indexes
// the server is still serving other requests from.
func (r *Registry) Close() error {
	if r.derived {
		return nil
	}
	var firstErr error
	for _, c := range r.list {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Provenance returns the kb_source string for the registry as a whole:
// the single collection's own provenance when only one is mounted (so
// `mk version` is byte-identical to before for every pre-existing
// deployment), and a count otherwise — the per-collection detail is
// reported alongside it rather than crammed into one string.
func (r *Registry) Provenance() string {
	if r.Single() {
		return r.list[0].Provenance()
	}
	return fmt.Sprintf("collections:%d", len(r.list))
}

// Health is one collection's serving state, as reported by Check.
type Health struct {
	// Name is the collection the entry describes.
	Name string `json:"collection"`
	// Ready is true when the collection's content root enumerated and
	// its search index is built — i.e. search/show/list against it can
	// be expected to work.
	Ready bool `json:"ready"`
	// Pages is how many pages enumerated, when Ready.
	Pages int `json:"pages"`
	// Error explains a not-Ready entry.
	Error string `json:"error,omitempty"`
	// Degraded is true when the collection's most recent reconciliation
	// cycle FAILED and it is serving the last known-good snapshot.
	//
	// It is a separate axis from Ready, and keeping the two apart is the
	// point. A degraded collection is answering every query correctly
	// with content that is merely older than the bucket's — which is a
	// staleness problem, not an availability one. Under the default
	// serve-last-good policy it stays Ready; only `failure_policy:
	// unready` couples the two, for a collection whose whole value is
	// that it is current. See internal/refresh.
	Degraded bool `json:"degraded,omitempty"`
	// StaleReason explains a Degraded entry. It reaches the structured
	// log and authenticated collection discovery, never /readyz's
	// unauthenticated body.
	StaleReason string `json:"stale_reason,omitempty"`
}

// contentRootReachable reports whether the collection's content root
// can be stat'd as a directory right now.
//
// It is a separate signal from Pages() erroring because Pages()
// deliberately does NOT error on a missing content root: kb.ListFS
// degrades a missing "content/" to an empty page list, so that a
// partially-populated --kb-dir serves what it has instead of hard
// failing (see kb.List's doc comment). That is right for serving and
// useless for a probe — an unmounted volume would read as a healthy,
// empty knowledge base forever.
func (c *Collection) contentRootReachable() bool {
	if c.byID != nil {
		// A FromPages collection has no filesystem to lose.
		return true
	}
	snap := c.acquire()
	defer snap.release()
	fsys := snap.fsys
	if fsys == nil {
		fsys = kb.FS()
	}
	info, err := fs.Stat(fsys, "content")
	return err == nil && info.IsDir()
}

// health derives one collection's serving state.
func (c *Collection) health() Health {
	// Latch the startup baseline on the first call. A deployment whose
	// content root was already absent when the server started has
	// deliberately (and documentedly) asked to serve an empty
	// collection; only a root that WAS there and has since gone is an
	// outage, and only that transition should un-ready the process.
	c.rootOnce.Do(func() { c.rootSeen = c.contentRootReachable() })

	h := Health{Name: c.Name}
	pages, err := c.Pages()
	if err != nil {
		h.Error = fmt.Sprintf("list pages: %v", err)
		return h
	}
	h.Pages = len(pages)
	if _, err := c.Index(); err != nil {
		h.Error = fmt.Sprintf("search index: %v", err)
		return h
	}
	if c.rootSeen && !c.contentRootReachable() {
		h.Error = "content root is no longer reachable (it was present at startup)"
		return h
	}
	h.Ready = true
	// A failed refresh is reported ON TOP of a serving collection, not
	// instead of one: the pages enumerated and the index built, both just
	// now, so this collection answers queries. What it cannot currently
	// do is prove it is current.
	if reason, unready := c.status.degraded(); reason != "" {
		h.Degraded = true
		h.StaleReason = reason
		if unready {
			h.Ready = false
			h.Error = reason
		}
	}
	return h
}

// Check re-derives each collection's serving state: that its content
// root is still reachable, that its pages still enumerate, and that its
// search index is built (building it if it somehow isn't). It is what a
// readiness probe reports, so it deliberately does real work rather
// than reading a flag latched at startup — a content directory that
// disappeared out from under a long-running server, or an unmounted
// volume, must show up as not ready rather than as a cached "ready"
// from an hour ago.
//
// Call it once before serving begins: that first call latches each
// collection's startup baseline (see health).
//
// Enumeration is the expensive part for a large collection. Callers
// that probe frequently should cache the result for a few seconds (the
// hosted MCP server does).
func (r *Registry) Check() []Health {
	out := make([]Health, 0, len(r.list))
	for _, c := range r.list {
		out = append(out, c.health())
	}
	return out
}

// Ready reports whether every mounted collection is serveable, and the
// per-collection detail behind that answer. An empty registry is ready:
// there is nothing that could be broken.
func (r *Registry) Ready() (bool, []Health) {
	health := r.Check()
	for _, h := range health {
		if !h.Ready {
			return false, health
		}
	}
	return true, health
}

// Pages returns the pages of the named collection, or of every
// collection in configuration order when collection is empty — as seen
// by this view's viewer (see ViewedBy).
func (r *Registry) Pages(collection string) ([]PageRef, error) {
	targets, err := r.target(collection)
	if err != nil {
		return nil, err
	}
	v := r.viewerOf()
	var out []PageRef
	for _, c := range targets {
		pages, err := c.PagesFor(v)
		if err != nil {
			return nil, fmt.Errorf("collection %q: %w", c.Name, err)
		}
		for _, p := range pages {
			out = append(out, PageRef{Collection: c.Name, Page: p})
		}
	}
	return out, nil
}

// Search queries the named collection, or every collection when
// collection is empty, and returns up to limit hits.
//
// Cross-collection ranking is a plain score merge: each collection is
// queried with the same limit and the union re-sorted by score. Scores
// are BM25 values from independent indexes, so they are comparable only
// approximately — good enough to interleave results usefully, and the
// alternative (one shared index) would forfeit the per-collection
// isolation this whole abstraction exists to provide. Ties break on
// configuration order, then page ID, so output is deterministic.
func (r *Registry) Search(ctx context.Context, collection, query string, limit int) ([]Hit, error) {
	targets, err := r.target(collection)
	if err != nil {
		return nil, err
	}
	order := make(map[string]int, len(r.list))
	for i, c := range r.list {
		order[c.Name] = i
	}
	v := r.viewerOf()
	ctx, span := telemetry.Span(ctx, telemetry.SpanSearchCollection,
		telemetry.KeySearchLimit.Int(limit),
		telemetry.KeySearchFiltered.Bool(!v.IsUnfiltered()),
	)
	var out []Hit
	for _, c := range targets {
		// The viewer goes INTO the query, not over its results. Each
		// collection is asked for its best `limit` documents that this
		// caller may see, so a collection full of somebody else's private
		// memories contributes its best visible hits rather than
		// contributing nothing — which is what a post-filter over the
		// per-collection top-N would do, and it would do it invisibly.
		results, err := c.searchAs(ctx, v, query, limit)
		if err != nil {
			// Classified rather than recorded: the error wraps bleve's own
			// `search %q: ...`, which quotes the caller's query verbatim.
			telemetry.Fail(span, telemetry.OutcomeError)
			return nil, err
		}
		for _, res := range results {
			out = append(out, Hit{Collection: c.Name, Result: res})
		}
	}
	// One span per fan-out, not per collection: the per-collection detail
	// a span could carry is the collection's NAME, which is exactly what
	// may not be exported, so it would be a span that says nothing the
	// parent does not. What is worth recording is the shape of the
	// fan-out — how many indexes were queried, whether a visibility
	// clause was in force — and that is one attribute set.
	span.SetAttributes(
		telemetry.KeyCollectionCount.Int(len(targets)),
		telemetry.KeySearchResults.Int(len(out)),
	)
	span.End()
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		if out[a].Collection != out[b].Collection {
			return order[out[a].Collection] < order[out[b].Collection]
		}
		return out[a].Page.ID < out[b].Page.ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SplitQualified splits a possibly-qualified page ID into its
// collection and page parts. The "<collection>:" prefix is only
// recognised when it names a collection that is actually mounted:
// anything else is returned untouched as a bare page ID, so an ID that
// happens to contain a colon can never be mistaken for a qualification.
func (r *Registry) SplitQualified(id string) (collection, pageID string) {
	name, rest, found := strings.Cut(id, ":")
	if !found || rest == "" {
		return "", id
	}
	if _, ok := r.by[name]; !ok {
		return "", id
	}
	return name, rest
}

// Show resolves one page.
//
// id may be qualified ("<collection>:<page-id>"). collection, when
// non-empty, restricts the lookup; giving both is fine as long as they
// agree — disagreeing is an error rather than a silent precedence rule.
//
// With no collection named and a bare ID, every collection is tried in
// configuration order: exactly one match is returned, several is an
// error wrapping ErrAmbiguous that lists the qualified IDs to pick
// from, and none is kb.ErrNotFound.
//
// "Match" means "match this view's viewer can see". A page hidden by
// per-page visibility is not counted, not named in the ambiguity error,
// and not distinguishable from a page that was never written — the same
// property Restrict gives a hidden collection, one level down.
func (r *Registry) Show(collection, id string) (PageRef, error) {
	qualified, pageID := r.SplitQualified(id)
	switch {
	case qualified != "" && collection == "":
		collection = qualified
	case qualified != "" && qualified != collection:
		return PageRef{}, fmt.Errorf("page id %q names collection %q but collection %q was requested", id, qualified, collection)
	}

	targets, err := r.target(collection)
	if err != nil {
		return PageRef{}, err
	}
	v := r.viewerOf()
	var found []PageRef
	for _, c := range targets {
		// LoadFor, so a page this caller may not see is not merely
		// withheld — it is not FOUND. That is what keeps the ambiguity
		// error below from counting invisible pages, and what makes a
		// guessed private ID answer exactly as a fictional one.
		page, err := c.LoadFor(v, pageID)
		if errors.Is(err, kb.ErrNotFound) {
			continue
		}
		if err != nil {
			return PageRef{}, fmt.Errorf("collection %q: %w", c.Name, err)
		}
		found = append(found, PageRef{Collection: c.Name, Page: page})
	}
	switch len(found) {
	case 0:
		return PageRef{}, kb.ErrNotFound
	case 1:
		return found[0], nil
	default:
		ids := make([]string, 0, len(found))
		for _, f := range found {
			ids = append(ids, f.QualifiedID())
		}
		return PageRef{}, fmt.Errorf("%w %q: it exists in %d collections — ask for one of %s",
			ErrAmbiguous, pageID, len(found), strings.Join(ids, ", "))
	}
}
