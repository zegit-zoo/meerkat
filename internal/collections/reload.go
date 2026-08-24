package collections

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/refresh"
	"github.com/zegit-zoo/meerkat/internal/search"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// reload.go is runtime reconciliation's business end: how a collection
// replaces what it is serving, without a restart and without a request
// ever seeing a seam.
//
// # The snapshot
//
// A collection's servable state is a SNAPSHOT: the filesystem its pages
// come from, the provenance token naming exactly which bytes those are,
// and the search index built over them. The three are swapped together
// or not at all, which is what "never expose a partially downloaded,
// partially parsed or partially indexed collection" means concretely —
// there is no moment at which the fsys is new and the index is old.
//
// # The swap, and the race it closes
//
// Swapping an index out from under a running query is a NEW hazard that
// hot reload introduces, and it is not a data race — bleve is
// concurrency-safe — it is a USE-AFTER-CLOSE. A query that has taken
// *search.Index off the collection and is about to call QueryAs on it
// will get "index closed" if a refresh closed that index in between.
// Under -race there is nothing to see; in production there is a burst of
// failed searches every time a new generation lands.
//
// So the snapshot is reference-counted:
//
//	acquire()   under snapMu.RLock: read the pointer AND take a
//	            reference, indivisibly.
//	install()   under snapMu.Lock: publish the new snapshot, then drop
//	            the collection's own reference to the old one.
//	release()   at zero: close the index.
//
// Because acquire's read-and-increment is under the read lock and
// install's swap is under the write lock, the two cannot interleave.
// Every reader that got a pointer got a reference with it, and the old
// index is closed only after the last of them is finished. In-flight
// requests run to completion against the generation they started on;
// requests that arrive after the swap see the new one.
//
// # The other race: a memory write during a rebuild
//
// Building a replacement index takes time, deliberately off the request
// path. A memory saved during that window would be indexed into the OLD
// index (the one still serving) and be absent from the NEW one — a write
// that a user watched succeed, silently lost one swap later.
//
// The fix is a staging journal. beginStaging arms `pending`; every live
// index write records itself there as well as indexing normally; the
// commit — under the same writeMu, so the ordering is total — replays
// pending into the new index (and into the rebuilt overlay, for a memory
// reload) BEFORE publishing it. A write either lands before the replay
// and is carried across, or after the commit and goes straight into the
// new index. There is no third case.

// snapshot is one coherent, immutable view of a collection's content.
//
// Immutable except for the lazily-built index and the reference count:
// nothing that a reader observes changes after the snapshot is
// published.
type snapshot struct {
	// fsys is the filesystem this generation's pages are read from. nil
	// means "read through the process-global kb filesystem" — the state a
	// single-collection deployment starts in, where internal/kbdir has
	// already pointed the globals at the right place.
	fsys fs.FS
	// provenance is the kb_source string for exactly these bytes.
	provenance string
	// version is the source version token — an object generation, a
	// prefix listing fingerprint — or "" for a source that has none. It
	// is what a probe compares against.
	version string

	// once/index/indexErr are the lazily-built search index. A snapshot
	// prepared by a reload has its index built eagerly, off the request
	// path, and marks once consumed (see newBuiltSnapshot); a snapshot
	// installed at mount builds on first use, so `mk list` still does not
	// pay to index a collection nobody searched.
	once     sync.Once
	index    *search.Index
	indexErr error

	// refs counts the collection's own reference plus one per in-flight
	// reader. The index is closed when it reaches zero.
	refs atomic.Int64
	// indexed reports that the once has run, for instrumentation only
	// (see built). It is separate from `index` because reading that field
	// outside the once is a data race, and a telemetry decision must not
	// be the thing that introduces one.
	indexed atomic.Bool
}

// acquire returns the snapshot serving right now, with a reference held
// on the caller's behalf. Every caller must release it.
func (c *Collection) acquire() *snapshot {
	c.snapMu.RLock()
	s := c.snap
	s.refs.Add(1)
	c.snapMu.RUnlock()
	return s
}

// install publishes s as the serving snapshot and drops the collection's
// reference to whatever was there.
func (c *Collection) install(s *snapshot) {
	s.refs.Store(1) // the collection's own reference
	c.snapMu.Lock()
	old := c.snap
	c.snap = s
	c.snapMu.Unlock()
	if old != nil {
		old.release()
	}
}

// release drops one reference, closing the index at zero.
func (s *snapshot) release() { _ = s.releaseErr() }

// releaseErr is release, reporting the index's close error when this was
// the last reference. Only Close cares; everything else uses release.
func (s *snapshot) releaseErr() error {
	if s.refs.Add(-1) != 0 {
		return nil
	}
	// Consume the once so that a build racing us to zero has finished
	// before we read s.index. At zero there is no reader left to start a
	// new one, so an unconsumed once here means the index was never built
	// and there is nothing to close.
	s.once.Do(func() {})
	if s.index == nil {
		return nil
	}
	return s.index.Close()
}

// discard releases a snapshot that was PREPARED but never installed —
// the failure path of a reload. It closes the index the rebuild had
// already produced rather than leaving it to the garbage collector,
// which would not close it at all.
func (s *snapshot) discard() {
	s.once.Do(func() {})
	if s.index != nil {
		_ = s.index.Close()
	}
}

// built reports whether s's index already exists, so instrumentation can
// tell a real build from a lazy no-op without forcing one.
//
// It reads an atomic flag rather than s.index, which is written under
// the once and would be a data race to read beside it. Racing with a
// concurrent first build is harmless either way — the answer only
// decides whether a span is worth emitting.
func (s *snapshot) built() bool { return s.indexed.Load() }

// indexOf returns s's index, building it on first use from the pages s
// resolves to.
func (c *Collection) indexOf(s *snapshot) (*search.Index, error) {
	s.once.Do(func() {
		defer s.indexed.Store(true)
		// Unfiltered, and unfiltered on purpose: the index must contain
		// EVERY document, including private personal memories. Visibility
		// is a clause in the query, applied at read time (see
		// internal/search's visibilityClause) — a document filtered out
		// here would be invisible to its own owner, permanently, with no
		// error anywhere. Same rule as the mount-time build.
		pages, err := c.pagesOf(s, kb.Unfiltered())
		if err != nil {
			s.indexErr = fmt.Errorf("list pages: %w", err)
			return
		}
		s.index, s.indexErr = search.NewFromPages(pages)
	})
	return s.index, s.indexErr
}

// newBuiltSnapshot prepares a snapshot whose index is ALREADY built.
//
// The build is the expensive half of a reload and it happens here,
// before anything is published, which is what keeps it off the request
// path. Consuming the once means indexOf will hand out this index rather
// than lazily building a second one.
func newBuiltSnapshot(ctx context.Context, fsys fs.FS, provenance, version string, pages []kb.Page) (*snapshot, error) {
	ctx, span := telemetry.Span(ctx, telemetry.SpanIndexBuild,
		telemetry.KeyIndexPages.Int(len(pages)))
	started := time.Now()
	idx, err := search.NewFromPages(pages)
	outcome := telemetry.OutcomeOK
	if err != nil {
		outcome = telemetry.OutcomeError
	}
	telemetry.Record(ctx).IndexBuilt(outcome, time.Since(started).Seconds())
	if err != nil {
		telemetry.End(span, err)
		return nil, fmt.Errorf("build search index: %w", err)
	}
	span.SetAttributes(telemetry.Outcome(outcome))
	span.End()
	s := &snapshot{fsys: fsys, provenance: provenance, version: version, index: idx}
	s.once.Do(func() {})
	s.indexed.Store(true)
	return s, nil
}

// searchAs runs one query against the collection's live index, holding a
// reference to the snapshot for exactly as long as the query runs.
//
// This — not Index() — is how a query reaches an index, and it is the
// reason a refresh can close the previous one safely.
func (c *Collection) searchAs(ctx context.Context, v kb.Viewer, query string, limit int) ([]search.Result, error) {
	snap := c.acquire()
	defer snap.release()
	idx, err := c.indexOf(snap)
	if err != nil {
		return nil, fmt.Errorf("collection %q: %w", c.Name, err)
	}
	return idx.QueryAs(ctx, c.viewerFor(v), query, limit)
}

// publishMemory makes a just-stored memory readable: into the overlay
// (so list and show see it), into the LIVE index (so search does), and —
// if a replacement index is being built right now — into the journal, so
// the swap carries it across.
//
// All three are under writeMu, and that is the whole point. Commit takes
// the same lock, so a save is either entirely BEFORE it (journalled, and
// replayed into both the rebuilt overlay and the rebuilt index) or
// entirely AFTER it (landing directly in the new snapshot). There is no
// interleaving in which the overlay swap drops a page that the index
// then gains — which would make one memory findable by search and
// invisible to show, for as long as it took the next cycle to notice.
func (c *Collection) publishMemory(p kb.Page) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.overlayMu.Lock()
	if c.overlay == nil {
		c.overlay = make(map[string]kb.Page, 1)
	}
	c.overlay[p.ID] = p
	c.overlayMu.Unlock()

	if c.pending != nil {
		c.pending[p.ID] = p
	}
	snap := c.acquire()
	defer snap.release()
	idx, err := c.indexOf(snap)
	if err != nil {
		return fmt.Errorf("memory was stored but the search index is unavailable: %w", err)
	}
	if err := idx.Put(p); err != nil {
		return fmt.Errorf("memory was stored but could not be indexed: %w", err)
	}
	return nil
}

// beginStaging arms the write journal for a reload that is about to
// build a replacement index.
func (c *Collection) beginStaging() {
	c.writeMu.Lock()
	c.pending = make(map[string]kb.Page)
	c.writeMu.Unlock()
}

// abortStaging disarms the journal after a failed reload. Nothing is
// lost: every journalled write already went into the live index too, and
// the live index is the one still serving.
func (c *Collection) abortStaging() {
	c.writeMu.Lock()
	c.pending = nil
	c.writeMu.Unlock()
}

// commit publishes a prepared snapshot, and — when overlay is non-nil —
// a rebuilt memory overlay, as ONE step.
//
// Everything slow already happened. What is left under the lock is
// replaying however many memory writes landed during the rebuild
// (normally none) and two pointer swaps.
//
// A replay failure aborts the commit and returns the error: the prepared
// snapshot is discarded by the caller and the last known-good one keeps
// serving. Nothing is lost by that — a journalled write is in the live
// index already.
func (c *Collection) commit(s *snapshot, overlay map[string]kb.Page) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	for id, p := range c.pending {
		if overlay != nil {
			overlay[id] = p
		}
		if s.index != nil {
			if err := s.index.Put(p); err != nil {
				c.pending = nil
				return fmt.Errorf("replay memory %q into the rebuilt index: %w", id, err)
			}
		}
	}
	c.pending = nil

	if overlay != nil {
		c.overlayMu.Lock()
		c.overlay = overlay
		c.overlayMu.Unlock()
	}
	c.install(s)
	return nil
}

// builtIndex reports the serving snapshot's index without building one.
// nil means "nobody has searched this collection yet".
func (c *Collection) builtIndex() *search.Index {
	c.snapMu.RLock()
	defer c.snapMu.RUnlock()
	return c.snap.index
}

// currentVersion reports the source version token the collection is
// serving.
func (c *Collection) currentVersion() string {
	c.snapMu.RLock()
	defer c.snapMu.RUnlock()
	return c.snap.version
}

// hasFilesystem reports whether the collection reads through its own
// filesystem rather than the process globals.
//
// It is consulted by the content reload for one narrow reason: a
// single-collection deployment mounts with a nil fsys and reads through
// the globals, so "the probe matches what we are serving" is not
// sufficient to skip — the collection has no handle of its own on that
// directory yet, and the first reload has to give it one. After that,
// version equality is the whole test.
func (c *Collection) hasFilesystem() bool {
	if c.byID != nil {
		// A FromPages collection has a fixed, in-memory page set and no
		// filesystem to mount. There is nothing a re-resolve could give it,
		// so an unchanged version is genuinely nothing to do.
		return true
	}
	c.snapMu.RLock()
	defer c.snapMu.RUnlock()
	return c.snap.fsys != nil
}

// --- reconciliation status --------------------------------------------

// ReloadStatus is one refresh target's reconciliation state.
//
// Version, and only Version, is the answer to "what is this replica
// serving right now". It travels here and in the structured log, never
// as a metric label — a generation increments forever, and labelling a
// series with one mints a new time series per publication.
type ReloadStatus struct {
	// Kind is refresh.KindContent or refresh.KindMemory.
	Kind string `json:"kind"`
	// Interval is the configured probe interval.
	Interval string `json:"interval"`
	// Policy is the configured failure policy.
	Policy string `json:"failure_policy"`
	// Version is the source version token currently applied.
	Version string `json:"version,omitempty"`
	// LastAttempt / LastSuccess are zero until the first cycle runs.
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	// Degraded reports that the most recent cycle failed and the last
	// known-good state is serving.
	Degraded bool `json:"degraded"`
	// Error explains a Degraded status.
	Error string `json:"error,omitempty"`
}

// reloadState holds a collection's per-kind reconciliation status.
//
// Per-kind rather than one flag, because content and memory fail
// independently: a memory store that has gone unreachable must not be
// cleared by the next successful content probe, and vice versa. Merging
// them would make a real, persistent failure disappear on the next tick
// of the OTHER target.
type reloadState struct {
	mu      sync.RWMutex
	content *ReloadStatus
	memory  *ReloadStatus
}

// configure records which refresh targets a source declares.
//
// Idempotent: a slot that already exists is left alone, history and all.
// It is called both at mount (Open) and when the targets are enumerated
// (RefreshTargets), because a *Collection can also be assembled by hand
// — FromPages plus a Source — and a collection with a refresh target but
// no status slot would reconcile invisibly, never reporting a failure to
// readiness.
func (r *reloadState) configure(src contentsource.Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if src.Refreshable() && r.content == nil {
		r.content = &ReloadStatus{
			Kind:     refresh.KindContent,
			Interval: src.Refresh.Interval.String(),
			Policy:   src.Refresh.Policy(),
		}
	}
	if src.Memory != nil && src.Memory.Refresh != nil && r.memory == nil {
		r.memory = &ReloadStatus{
			Kind:     refresh.KindMemory,
			Interval: src.Memory.Refresh.Interval.String(),
			Policy:   src.Memory.Refresh.Policy(),
		}
	}
}

// slot returns the status for one kind, or nil when that kind is not
// configured.
func (r *reloadState) slot(kind string) *ReloadStatus {
	if kind == refresh.KindMemory {
		return r.memory
	}
	return r.content
}

// succeeded records a clean cycle.
func (r *reloadState) succeeded(kind, version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slot(kind)
	if s == nil {
		return
	}
	now := time.Now()
	s.LastAttempt, s.LastSuccess = now, now
	s.Degraded, s.Error = false, ""
	if version != "" {
		s.Version = version
	}
}

// failed records a failed cycle. LastSuccess is deliberately untouched:
// "it last worked at T" is the number an operator needs to decide how
// stale the content actually is.
func (r *reloadState) failed(kind string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slot(kind)
	if s == nil {
		return
	}
	s.LastAttempt = time.Now()
	s.Degraded, s.Error = true, err.Error()
}

// degraded reports whether any configured target is degraded, why, and
// whether its policy says that should fail readiness.
func (r *reloadState) degraded() (reason string, unready bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range []*ReloadStatus{r.content, r.memory} {
		if s == nil || !s.Degraded {
			continue
		}
		if reason == "" {
			reason = s.Kind + " refresh failed, serving the last known-good snapshot: " + s.Error
		}
		if s.Policy == refresh.PolicyUnready {
			unready = true
		}
	}
	return reason, unready
}

// statuses returns a copy of every configured target's status.
func (r *reloadState) statuses() []ReloadStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ReloadStatus, 0, 2)
	for _, s := range []*ReloadStatus{r.content, r.memory} {
		if s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// ReloadStatuses returns the collection's reconciliation status, one
// entry per configured refresh target, or nothing at all when the
// collection is resolved-once (which is every collection that has not
// opted in).
func (c *Collection) ReloadStatuses() []ReloadStatus { return c.status.statuses() }

// --- the reconciliation path ------------------------------------------

// probeVersion and resolveContent are the two calls a content reload
// makes into internal/contentsource. They are package vars for exactly
// the reason newGCSClient is one over there (see its doc comment): they
// are the test seam.
//
// Swapping them lets this package's tests drive the reconciliation
// MECHANICS — staging, rebuild, atomic swap, degradation, the reload
// slot — without a bucket, while internal/contentsource's own tests
// drive the GCS mechanics (conditional reads, generation preconditions,
// hardening, caps) against its in-memory fake. Production always gets
// the real functions; there is no configuration or exported knob that
// could point them anywhere else.
var (
	probeVersion   = contentsource.GCSVersion
	resolveContent = contentsource.FetchGCS
)

// ReloadContent re-resolves the collection's content source and, when
// the bytes actually changed, swaps a new snapshot in.
//
// It is the ONE content-reconciliation path: the scheduled loop and the
// admin reload trigger both land here, so there is exactly one place
// that decides what a replica is serving.
//
// Order of operations, and every step of it is load-bearing:
//
//  1. Probe metadata only. Unchanged is the common case and costs one
//     call — no download, no reindex, no cache write.
//  2. Arm the write journal, so a memory saved during the rebuild is not
//     lost by the swap.
//  3. Re-resolve through FetchGCS — the SAME hardened path startup uses:
//     conditional reads pinned to an exact generation, os.Root-contained
//     writes, per-file/cumulative/object-count caps, and a
//     staging-directory-plus-atomic-rename cache finalisation. The new
//     cache entry is keyed by the NEW version, so a failure cannot
//     delete or corrupt the last known-good one.
//  4. Enumerate and parse the pages, then build the replacement index —
//     all before anything is published.
//  5. Commit.
//
// Any failure returns with the previous snapshot still installed and the
// collection marked degraded.
func (c *Collection) ReloadContent(ctx context.Context) (refresh.Outcome, error) {
	if !c.reloadMu.TryLock() {
		return refresh.Outcome{}, refresh.ErrBusy
	}
	defer c.reloadMu.Unlock()

	src := c.Source
	if !src.Refreshable() {
		return refresh.Outcome{}, fmt.Errorf("collection %q is not configured for content refresh", c.Name)
	}

	// Each of the six steps below gets a phase span, so a slow
	// reconciliation is attributable to the probe, the download, the
	// parse, the index build or the swap rather than to "the cycle". The
	// spans carry durations and counts; the bucket, the object name and
	// the error text stay out, exactly as they stay out of the refresh
	// metric labels (internal/refresh/metrics.go).
	version, err := phase(ctx, telemetry.PhaseProbe, func(ctx context.Context) (string, error) {
		return probeVersion(ctx, src)
	})
	if err != nil {
		return refresh.Outcome{}, c.contentFailed(fmt.Errorf("probe %s: %w", src.Bucket, err))
	}
	if version == c.currentVersion() && c.hasFilesystem() {
		c.status.succeeded(refresh.KindContent, version)
		return refresh.Outcome{Changed: false, Version: version}, nil
	}

	c.beginStaging()
	committed := false
	defer func() {
		if !committed {
			c.abortStaging()
		}
	}()

	// FetchGCS re-reads the current generation itself rather than being
	// handed the probe's answer. A publication that landed between the
	// two is then simply resolved as the newer generation — coherently,
	// with its own conditional reads — instead of being fetched under a
	// version token that no longer describes it.
	//
	// This one step is spanned by hand rather than through phase(): it
	// returns two values, and a generic wrapper would need a struct to
	// carry them. resolveCtx is deliberately a NEW variable — assigning
	// back over ctx would parent every later phase to this one, and a
	// trace that reads "mount happened inside resolve" is worse than no
	// trace.
	resolveCtx, resolveSpan := telemetry.Span(ctx, telemetry.PhaseResolve)
	dir, resolved, err := resolveContent(resolveCtx, src)
	if err != nil {
		telemetry.Fail(resolveSpan, telemetry.OutcomeError)
		return refresh.Outcome{}, c.contentFailed(fmt.Errorf("resolve: %w", err))
	}
	resolveSpan.SetAttributes(telemetry.Outcome(telemetry.OutcomeOK))
	resolveSpan.End()

	fsys, err := phase(ctx, telemetry.PhaseMount, func(context.Context) (fs.FS, error) {
		return kbdir.FSLayout(dir, src.Layout)
	})
	if err != nil {
		return refresh.Outcome{}, c.contentFailed(fmt.Errorf("mount: %w", err))
	}
	pages, err := phase(ctx, telemetry.PhaseEnumerate, func(context.Context) ([]kb.Page, error) {
		return c.contentPagesFrom(fsys)
	})
	if err != nil {
		return refresh.Outcome{}, c.contentFailed(fmt.Errorf("enumerate pages: %w", err))
	}
	// The memory overlay is not touched by a content refresh, so it is
	// merged from the live one — unfiltered, so every document is in the
	// index and visibility stays a query-time decision.
	next, err := newBuiltSnapshot(ctx, fsys, contentsource.GCSProvenance(src, resolved), resolved,
		c.mergeOverlay(pages, kb.Unfiltered()))
	if err != nil {
		return refresh.Outcome{}, c.contentFailed(err)
	}
	if err := c.commitPhase(ctx, next, nil); err != nil {
		next.discard()
		return refresh.Outcome{}, c.contentFailed(err)
	}
	committed = true
	c.status.succeeded(refresh.KindContent, resolved)
	return refresh.Outcome{Changed: true, Version: resolved}, nil
}

// phase wraps one reconciliation step in a span.
//
// The step's error is classified rather than recorded: a probe or mount
// failure quotes a bucket, an object name or a filesystem path, and none
// of the three may be exported. The caller wraps the same error with the
// full text for the log, where it belongs.
func phase[T any](ctx context.Context, name string, fn func(context.Context) (T, error)) (T, error) {
	ctx, span := telemetry.Span(ctx, name)
	v, err := fn(ctx)
	if err != nil {
		telemetry.Fail(span, telemetry.OutcomeError)
		return v, err
	}
	span.SetAttributes(telemetry.Outcome(telemetry.OutcomeOK))
	span.End()
	return v, nil
}

// commitPhase is commit under a span. The swap is the one step that
// holds the write lock, so its duration is the answer to "did a reload
// stall a save".
func (c *Collection) commitPhase(ctx context.Context, s *snapshot, overlay map[string]kb.Page) error {
	_, err := phase(ctx, telemetry.PhaseCommit, func(context.Context) (struct{}, error) {
		return struct{}{}, c.commit(s, overlay)
	})
	return err
}

// contentFailed records a failed content cycle and returns the error to
// report. Callers pair it with a zero Outcome — there is no partial
// outcome to report, because a failed cycle changed nothing.
func (c *Collection) contentFailed(err error) error {
	wrapped := fmt.Errorf("collection %q: %w", c.Name, err)
	c.status.failed(refresh.KindContent, wrapped)
	return wrapped
}

// ReloadMemory re-reads the collection's memory store and, when another
// writer has changed something, rebuilds the overlay and the index and
// swaps both in.
//
// This is what makes replicas converge. Replica A indexes its own writes
// immediately (SaveMemory); B learns about them here, by noticing that
// the store's listing fingerprint moved.
//
// The overlay is rebuilt from the store's own keys — memory.Page(key,
// body) — and never from what a document's frontmatter claims about
// itself. A personal memory's OWNER is derived from the page ID, and the
// page ID is derived from the store key, which is derived from the
// verified identity that wrote it. Reconstructing an ID from a
// `memory_namespace:` field instead would let a document's bytes choose
// whose memory it is; internal/memory's
// TestPage_OwnerComesFromTheStoreKeyNotTheDocument pins that, and this
// path is the reason it matters at runtime rather than only at mount.
func (c *Collection) ReloadMemory(ctx context.Context) (refresh.Outcome, error) {
	if !c.reloadMu.TryLock() {
		return refresh.Outcome{}, refresh.ErrBusy
	}
	defer c.reloadMu.Unlock()

	store := c.Memory()
	if store == nil {
		return refresh.Outcome{}, fmt.Errorf("collection %q has no memory store to reconcile", c.Name)
	}
	fp, ok := store.(memory.Fingerprinter)
	if !ok {
		return refresh.Outcome{}, fmt.Errorf("collection %q: memory store %s cannot be probed cheaply", c.Name, store.Describe())
	}

	backend := memory.Backend(store)
	sum, err := phase(ctx, telemetry.PhaseProbe, func(ctx context.Context) (string, error) {
		return timedMemory(ctx, backend, telemetry.MemoryFingerprint, func() (string, error) {
			return fp.Fingerprint(ctx)
		})
	})
	if err != nil {
		return refresh.Outcome{}, c.memoryFailed(fmt.Errorf("probe memory store: %w", err))
	}
	if sum == c.status.memoryVersion() {
		c.status.succeeded(refresh.KindMemory, sum)
		return refresh.Outcome{Changed: false, Version: sum}, nil
	}

	c.beginStaging()
	committed := false
	defer func() {
		if !committed {
			c.abortStaging()
		}
	}()

	records, err := phase(ctx, telemetry.PhaseResolve, func(ctx context.Context) ([]memory.Record, error) {
		return timedMemory(ctx, backend, telemetry.MemoryLoad, func() ([]memory.Record, error) {
			return store.Load(ctx)
		})
	})
	if err != nil {
		return refresh.Outcome{}, c.memoryFailed(fmt.Errorf("load memory store: %w", err))
	}
	overlay := make(map[string]kb.Page, len(records))
	for _, rec := range records {
		page, perr := memory.Page(rec.Key, rec.Body)
		if perr != nil {
			// Same rule as AttachMemory: one malformed document must not
			// make a whole collection unserveable, and must not make a
			// reload fail either — the alternative is a fleet that stops
			// converging because somebody hand-edited one object.
			fmt.Fprintf(os.Stderr, "meerkat: skipping memory %s in collection %q: %v\n", rec.Key, c.Name, perr)
			continue
		}
		overlay[page.ID] = page
	}

	// Same content generation, new overlay: read the current snapshot's
	// filesystem and provenance so a concurrent content refresh cannot
	// make this one publish a stale root.
	cur := c.acquire()
	pages, perr := phase(ctx, telemetry.PhaseEnumerate, func(context.Context) ([]kb.Page, error) {
		return c.contentPagesFrom(cur.fsys)
	})
	if perr != nil {
		cur.release()
		return refresh.Outcome{}, c.memoryFailed(fmt.Errorf("enumerate pages: %w", perr))
	}
	next, berr := newBuiltSnapshot(ctx, cur.fsys, cur.provenance, cur.version,
		mergeOverlayMap(pages, overlay, kb.Unfiltered()))
	cur.release()
	if berr != nil {
		return refresh.Outcome{}, c.memoryFailed(berr)
	}
	if err := c.commitPhase(ctx, next, overlay); err != nil {
		next.discard()
		return refresh.Outcome{}, c.memoryFailed(err)
	}
	committed = true
	c.status.succeeded(refresh.KindMemory, sum)
	return refresh.Outcome{Changed: true, Version: sum}, nil
}

// memoryFailed records a failed memory cycle and returns the error to
// report. See contentFailed.
func (c *Collection) memoryFailed(err error) error {
	wrapped := fmt.Errorf("collection %q: %w", c.Name, err)
	c.status.failed(refresh.KindMemory, wrapped)
	return wrapped
}

// memoryVersion reports the store fingerprint the overlay was last
// rebuilt from.
func (r *reloadState) memoryVersion() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.memory == nil {
		return ""
	}
	return r.memory.Version
}

// --- targets -----------------------------------------------------------

// RefreshTargets returns the reconciliation targets this registry's
// configuration declares, in configuration order: a content target for
// every collection with a `refresh:` block, and a memory target for
// every collection whose `memory:` block has one.
//
// A DERIVED registry (Restrict, ViewedBy) returns none. A per-request
// view borrows its collections; reconciling through one would be a
// request deciding what a whole process serves.
func (r *Registry) RefreshTargets() []refresh.Target {
	if r.derived {
		return nil
	}
	out := make([]refresh.Target, 0, len(r.list))
	for i, c := range r.list {
		// Idempotent, and here as well as in Open so that a hand-assembled
		// collection still gets a status slot — without one, a failed
		// refresh would be invisible to readiness.
		c.status.configure(c.Source)
		if c.Source.Refreshable() {
			out = append(out, &contentTarget{c: c, ordinal: i})
		}
		if c.Source.Memory != nil && c.Source.Memory.Refresh != nil {
			out = append(out, &memoryTarget{c: c, ordinal: i})
		}
	}
	return out
}

// contentTarget reconciles one collection's content source.
type contentTarget struct {
	c       *Collection
	ordinal int
}

func (t *contentTarget) Key() refresh.Key {
	return refresh.Key{Ordinal: t.ordinal, Kind: refresh.KindContent, Name: t.c.Name}
}

func (t *contentTarget) Spec() *refresh.Spec { return t.c.Source.Refresh }

func (t *contentTarget) Reconcile(ctx context.Context) (refresh.Outcome, error) {
	return t.c.ReloadContent(ctx)
}

// memoryTarget reconciles one collection's memory store.
type memoryTarget struct {
	c       *Collection
	ordinal int
}

func (t *memoryTarget) Key() refresh.Key {
	return refresh.Key{Ordinal: t.ordinal, Kind: refresh.KindMemory, Name: t.c.Name}
}

func (t *memoryTarget) Spec() *refresh.Spec { return t.c.Source.Memory.Refresh }

func (t *memoryTarget) Reconcile(ctx context.Context) (refresh.Outcome, error) {
	return t.c.ReloadMemory(ctx)
}
