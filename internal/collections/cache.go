package collections

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// cache.go is lazy mounting and the resident budget (meerkat-mob issue
// E): the neural-plasticity model from the brief. A `mount: lazy`
// child of the tree (issue D) is a Collection from the start — it has a
// name, a place in the tree and a source — but it is COLD: no snapshot,
// no pages, no index. The first request that names it mounts it through
// the same phases a hot reload uses (resolve, mount, enumerate, build,
// commit), after which it is warm until the cache culls it.
//
// Culling is for the cache only, never for content (Q8): a culled
// collection goes back to cold and is mounted again on the next
// request. Every traversal of a collection raises its temperature; the
// first traversal is stamped with a sequence number. When the cache
// passes its high watermark, the coldest and then the oldest-first-
// traversed lazy collections are culled until it is below it. The root
// and eager collections are never in the cache and never culled.
//
// Temperatures are flushed to the traversal log (issue G) so a restart
// warm-starts: it reads the recent flushes, matches them to its cold
// collections by hashed name, and mounts the hottest that fit.

// ColdError is what a request gets when the cold policy is async: the
// mount has been started; retry after RetryAfter. It wraps
// ErrColdCollection (and therefore ErrUnknownCollection).
type ColdError struct {
	Name       string
	RetryAfter time.Duration
}

func (e *ColdError) Error() string {
	return fmt.Sprintf("collection %q is cold and being mounted; retry after %s", e.Name, e.RetryAfter)
}

func (e *ColdError) Unwrap() error { return ErrColdCollection }

// cache is the root registry's resident-budget state.
type cache struct {
	spec contentsource.CacheSpec
	log  *traversal.Log

	mu    sync.Mutex
	bytes int64
	seq   atomic.Int64
	// mounting tracks async mounts in flight, by name.
	mounting map[string]bool
}

// SetCache configures lazy mounting on a root registry. spec nil means
// the defaults (unbounded budget, blocking cold policy); log may be nil
// (no warm start, no temperature flush).
func (r *Registry) SetCache(spec *contentsource.CacheSpec, log *traversal.Log) {
	root := r.base()
	s := contentsource.CacheSpec{}
	if spec != nil {
		s = *spec
	}
	_ = s.Validate("cache")
	root.cache = &cache{spec: s, log: log, mounting: map[string]bool{}}
}

func (r *Registry) cacheState() *cache {
	root := r.base()
	if root.cache == nil {
		root.cacheOnce.Do(func() {
			if root.cache == nil {
				s := contentsource.CacheSpec{}
				_ = s.Validate("cache")
				root.cache = &cache{spec: s, mounting: map[string]bool{}}
			}
		})
	}
	return root.cache
}

// CacheSpec returns the effective cache configuration.
func (r *Registry) CacheSpec() contentsource.CacheSpec { return r.cacheState().spec }

// --- per-collection state ------------------------------------------------

// IsCold reports whether the collection is declared but not resident.
func (c *Collection) IsCold() bool { return c.cold.Load() }

// Lazy reports whether the collection may be culled: a mount: lazy
// child of the tree.
func (c *Collection) Lazy() bool { return c.lazy }

// Temperature returns the traversal counter and the first-traversal
// sequence (0 = never traversed).
func (c *Collection) Temperature() (temperature, firstTraversal int64) {
	return c.temperature.Load(), c.firstTraversal.Load()
}

// touch records one traversal.
func (r *Registry) touch(c *Collection) {
	if c.temperature.Add(1) == 1 {
		c.firstTraversal.CompareAndSwap(0, r.cacheState().seq.Add(1))
	}
	c.lastTraversal.Store(time.Now().UnixNano())
}

// newColdCollection builds the Collection for a declared-but-unmounted
// child: named, placed, sourced, and cold.
func newColdCollection(node *contentsource.TreeNode) *Collection {
	src, _ := node.LazySource()
	c := &Collection{Name: node.Name, Tree: node, lazy: true}
	if src != nil {
		c.Source = *src
	}
	c.install(&snapshot{provenance: "cold"})
	c.cold.Store(true)
	return c
}

// ensureMounted mounts a cold collection under the cold policy. It
// returns nil when the collection is (now) resident, a *ColdError when
// the policy is async and a mount was started, or the mount error.
func (r *Registry) ensureMounted(ctx context.Context, c *Collection) error {
	if !c.IsCold() {
		return nil
	}
	cs := r.cacheState()
	if cs.spec.ColdPolicy == contentsource.ColdAsync {
		cs.mu.Lock()
		already := cs.mounting[c.Name]
		if !already {
			cs.mounting[c.Name] = true
		}
		cs.mu.Unlock()
		if !already {
			// Detached from the request: the caller has been told to
			// retry, and the mount must outlive its deadline.
			bg := context.WithoutCancel(ctx)
			go func() {
				_ = r.mount(bg, c, telemetry.MountLazy, true)
				cs.mu.Lock()
				delete(cs.mounting, c.Name)
				cs.mu.Unlock()
			}()
		}
		return &ColdError{Name: c.Name, RetryAfter: cs.spec.RetryAfter}
	}
	return r.mount(ctx, c, telemetry.MountLazy, true)
}

// mount resolves, enumerates and indexes a cold collection and installs
// the snapshot. Serialised per collection; a second caller waits and
// finds it warm. cullOthers lets the mount push colder residents out
// when it overflows the budget (a request needs this one now); warm
// start passes false and stops instead.
func (r *Registry) mount(ctx context.Context, c *Collection, trigger string, cullOthers bool) (err error) {
	c.mountMu.Lock()
	defer c.mountMu.Unlock()
	if !c.IsCold() {
		return nil
	}
	src, cfgPath := c.Tree.LazySource()
	if src == nil {
		return fmt.Errorf("%w: %q has no source to mount from", ErrColdCollection, c.Name)
	}
	tier := 0
	if c.Tree != nil {
		tier = c.Tree.Depth
	}
	ctx, span := telemetry.Span(ctx, telemetry.SpanMount,
		telemetry.KeyMountTrigger.String(trigger),
		telemetry.KeyMountTier.Int(tier))
	started := time.Now()
	defer func() {
		outcome := telemetry.OutcomeOK
		if err != nil {
			outcome = telemetry.OutcomeError
			telemetry.Fail(span, outcome)
		} else {
			span.SetAttributes(telemetry.Outcome(outcome), telemetry.KeyMountBytes.Int64(c.residentBytes.Load()))
			span.End()
		}
		telemetry.Record(ctx).Mounted(trigger, outcome, tier, time.Since(started).Seconds())
	}()

	rc, err := contentsource.ResolveSource(ctx, *src, cfgPath)
	if err != nil {
		return fmt.Errorf("mount %q: %w", c.Name, err)
	}
	if rc.Dir == "" {
		return fmt.Errorf("mount %q: source resolves to no directory", c.Name)
	}
	fsys, err := kbdir.FSLayout(rc.Dir, src.Layout)
	if err != nil {
		return fmt.Errorf("mount %q: %w", c.Name, err)
	}
	pages, err := kb.ListFS(fsys)
	if err != nil {
		return fmt.Errorf("mount %q: enumerate pages: %w", c.Name, err)
	}
	snap, err := newBuiltSnapshot(ctx, fsys, rc.Provenance, rc.Version, c.mergeOverlay(pages, kb.Unfiltered()), c.searchOptions()...)
	if err != nil {
		return fmt.Errorf("mount %q: %w", c.Name, err)
	}
	var size int64
	if snap.index != nil {
		size = snap.index.SizeEstimate()
	}
	c.residentBytes.Store(size)
	c.install(snap)
	c.cold.Store(false)
	if c.Tree != nil {
		c.Tree.Mounted = true
	}
	r.account(ctx, c, size, cullOthers)
	return nil
}

// account adds a mounted collection's bytes and, when asked, culls
// colder residents if the cache is over the watermark.
func (r *Registry) account(ctx context.Context, c *Collection, size int64, cullOthers bool) {
	cs := r.cacheState()
	cs.mu.Lock()
	cs.bytes += size
	cs.mu.Unlock()
	r.publishCache(ctx)
	if cullOthers && r.overWatermark() {
		r.cull(ctx, c)
	}
}

// overWatermark reports whether resident bytes exceed the watermark.
func (r *Registry) overWatermark() bool {
	cs := r.cacheState()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.spec.MaxBytes > 0 && float64(cs.bytes) > cs.spec.HighWatermark*float64(cs.spec.MaxBytes)
}

// cull unmounts lazy collections — coldest first, then oldest first
// traversal — until the cache is under its watermark. keep is the
// collection just mounted, which is never culled by its own mount.
func (r *Registry) cull(ctx context.Context, keep *Collection) {
	cs := r.cacheState()
	limit := int64(cs.spec.HighWatermark * float64(cs.spec.MaxBytes))
	for {
		cs.mu.Lock()
		if cs.spec.MaxBytes == 0 || cs.bytes <= limit {
			cs.mu.Unlock()
			return
		}
		cs.mu.Unlock()
		victim, reason := r.coldest(keep)
		if victim == nil {
			return
		}
		r.unmount(ctx, victim, reason)
	}
}

// coldest picks the next cull victim among resident lazy collections.
func (r *Registry) coldest(keep *Collection) (*Collection, string) {
	var candidates []*Collection
	for _, c := range r.base().list {
		if c == keep || !c.lazy || c.IsCold() {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil, ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ti, fi := candidates[i].Temperature()
		tj, fj := candidates[j].Temperature()
		if ti != tj {
			return ti < tj
		}
		return fi < fj
	})
	victim := candidates[0]
	reason := telemetry.CullTemperature
	if len(candidates) > 1 {
		t0, _ := candidates[0].Temperature()
		t1, _ := candidates[1].Temperature()
		if t0 == t1 {
			reason = telemetry.CullAge
		}
	}
	return victim, reason
}

// unmount returns a lazy collection to cold. Readers holding the old
// snapshot drain before its index closes (refcount).
func (r *Registry) unmount(ctx context.Context, c *Collection, reason string) {
	c.mountMu.Lock()
	defer c.mountMu.Unlock()
	if c.IsCold() {
		return
	}
	size := c.residentBytes.Swap(0)
	c.cold.Store(true)
	if c.Tree != nil {
		c.Tree.Mounted = false
	}
	c.install(&snapshot{provenance: "cold"})
	cs := r.cacheState()
	cs.mu.Lock()
	cs.bytes -= size
	if cs.bytes < 0 {
		cs.bytes = 0
	}
	cs.mu.Unlock()
	telemetry.Record(ctx).Culled(reason)
	r.publishCache(ctx)
}

// Evict culls one named lazy collection explicitly (an operator or a
// librarian action). Not an error for a collection that is already
// cold; an error for one that is not lazy.
func (r *Registry) Evict(ctx context.Context, name string) error {
	c, ok := r.base().by[name]
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownCollection, name)
	}
	if !c.lazy {
		return fmt.Errorf("collection %q is not lazily mounted and cannot be evicted", name)
	}
	r.unmount(ctx, c, telemetry.CullEvict)
	return nil
}

// Resident returns the cache's resident bytes and the count of
// resident lazy collections.
func (r *Registry) Resident() (bytes int64, collections int) {
	cs := r.cacheState()
	cs.mu.Lock()
	bytes = cs.bytes
	cs.mu.Unlock()
	for _, c := range r.base().list {
		if c.lazy && !c.IsCold() {
			collections++
		}
	}
	return bytes, collections
}

func (r *Registry) publishCache(ctx context.Context) {
	bytes, n := r.Resident()
	telemetry.Record(ctx).SetCache(bytes, int64(r.cacheState().spec.MaxBytes), n)
}

// --- temperatures: flush and warm start ---------------------------------

// FlushTemperatures writes every collection's temperature to the
// traversal log (no-op without one) and observes the distribution.
func (r *Registry) FlushTemperatures(ctx context.Context) error {
	cs := r.cacheState()
	byName := map[string]traversal.Temperature{}
	for _, c := range r.base().list {
		t, first := c.Temperature()
		if t == 0 {
			continue
		}
		depth := 0
		if c.Tree != nil {
			depth = c.Tree.Depth
		}
		byName[c.Name] = traversal.Temperature{Temperature: t, FirstTraversal: first, Depth: depth}
		telemetry.Record(ctx).ObserveTemperature(t)
	}
	if cs.log == nil || len(byName) == 0 {
		return nil
	}
	return cs.log.RecordTemperatures(ctx, byName)
}

// RunFlusher flushes temperatures every interval until ctx ends, then
// once more. It returns when ctx is done.
func (r *Registry) RunFlusher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = contentsource.DefaultFlushInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = r.FlushTemperatures(context.WithoutCancel(ctx))
			return
		case <-t.C:
			_ = r.FlushTemperatures(ctx)
		}
	}
}

// WarmStart reads the last days of temperature flushes from the
// traversal log and mounts the hottest cold collections that fit under
// the watermark, hottest first. It returns how many were mounted.
func (r *Registry) WarmStart(ctx context.Context, days int) (int, error) {
	cs := r.cacheState()
	if cs.log == nil || days <= 0 {
		return 0, nil
	}
	temps, err := cs.log.ReadTemperatures(ctx, days)
	if err != nil {
		return 0, fmt.Errorf("warm start: %w", err)
	}
	type candidate struct {
		c *Collection
		t traversal.Temperature
	}
	var cands []candidate
	for _, c := range r.base().list {
		if !c.lazy || !c.IsCold() {
			continue
		}
		if t, ok := temps[cs.log.Hash(c.Name)]; ok && t.Temperature > 0 {
			cands = append(cands, candidate{c, t})
			c.temperature.Store(t.Temperature)
			c.firstTraversal.Store(t.FirstTraversal)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].t.Temperature > cands[j].t.Temperature })
	mounted := 0
	for _, cand := range cands {
		if r.overWatermark() {
			break
		}
		if err := r.mount(ctx, cand.c, telemetry.MountWarmStart, false); err != nil {
			if errors.Is(err, context.Canceled) {
				return mounted, err
			}
			continue // a cold path that fails to mount is a request-time error later
		}
		if r.overWatermark() {
			// Hottest first: the newcomer is the coldest of what was
			// pre-mounted, so it is the one that does not fit.
			r.unmount(ctx, cand.c, telemetry.CullEvict)
			break
		}
		mounted++
	}
	return mounted, nil
}
