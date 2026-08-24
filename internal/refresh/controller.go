package refresh

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// controller.go is the runtime half of this package: the polling loop
// that drives a set of Targets, and the metrics/logging around it.
//
// It knows nothing about GCS, collections, indexes or memory stores. A
// Target is an opaque "reconcile yourself, cheaply if nothing changed",
// which keeps the scheduling policy (interval, jitter, overlap
// suppression, degradation accounting) in one place and the reconciliation
// mechanics in the package that owns the state being reconciled — see
// internal/collections.

// Target kinds. A closed set of two, so it is safe as a metric label.
const (
	// KindContent is a collection's content source.
	KindContent = "content"
	// KindMemory is a collection's writable memory store.
	KindMemory = "memory"
)

// ErrBusy is what a Target returns when a refresh for the same
// collection is already in flight.
//
// It is a first-class outcome rather than an error, because it is the
// system working: a refresh that takes longer than the interval must not
// stack up behind itself, and an admin-triggered reload arriving during
// a scheduled one must not run a second, concurrent swap. The controller
// counts it separately from a failure and does not mark anything
// degraded.
var ErrBusy = errors.New("a refresh is already in flight for this collection")

// Key identifies a Target for metrics and logs. The two are deliberately
// different: Ordinal is what a metric is labelled with, Name is what a
// log line says.
type Key struct {
	// Ordinal is the collection's index in configuration order. It is the
	// metric label, and it is the metric label BECAUSE it is bounded by
	// the number of mounted collections and reveals nothing: /metrics is
	// unauthenticated, and which collections a deployment mounts is not
	// public information (see internal/mcp/metrics.go's label discipline).
	Ordinal int
	// Kind is KindContent or KindMemory.
	Kind string
	// Name is the collection name. LOGS ONLY — never a metric label.
	Name string
}

// Label is the bounded metric label value for this target's collection.
func (k Key) Label() string { return strconv.Itoa(k.Ordinal) }

// Outcome is what one reconciliation cycle did.
type Outcome struct {
	// Changed reports whether a new snapshot was actually swapped in. A
	// cycle that probed and found nothing new is a success with
	// Changed == false, and is the common case.
	Changed bool
	// Version is the source version token now being served: an object
	// generation, a prefix listing fingerprint, or a memory store
	// fingerprint.
	//
	// It travels in the STRUCTURED status and the log, never as a metric
	// label: a generation increments forever, so labelling a metric with
	// one would mint a new time series on every publication.
	Version string
}

// Target is one thing that can be reconciled against its source of
// truth. Implementations live in internal/collections.
type Target interface {
	// Key identifies the target for metrics and logs.
	Key() Key
	// Spec is the configured refresh policy.
	Spec() *Spec
	// Reconcile performs one full cycle: a cheap metadata probe and,
	// only if that shows a change, a hardened re-resolve, an off-request-
	// path rebuild and an atomic swap.
	//
	// It must be safe to call concurrently with serving, and must return
	// ErrBusy rather than running a second swap when one is already in
	// flight for the same collection. On any failure it must leave the
	// last known-good state serving.
	Reconcile(ctx context.Context) (Outcome, error)
}

// Options configures a Controller.
type Options struct {
	// Targets are the things to reconcile. An empty slice makes New
	// return nil, so a deployment that configures no refresh anywhere
	// carries no controller at all.
	Targets []Target
	// Logger receives refresh lifecycle logs. Nil discards them.
	Logger *slog.Logger
	// Registry is the Prometheus registry the refresh collectors are
	// registered on. Nil registers nowhere, which is what a CLI-side or
	// test caller wants.
	Registry *prometheus.Registry
}

// Controller drives a set of Targets on their configured schedules.
//
// The zero value is not usable; a nil *Controller is, and every method
// tolerates one. That is deliberate: New returns nil when nothing is
// configured, and the hosted server then calls Start/Close/ReloadNow
// unconditionally rather than guarding each one.
type Controller struct {
	targets []Target
	log     *slog.Logger
	metrics *metrics

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
}

// New builds a controller over opts.Targets, or returns nil when there
// are none.
func New(opts Options) *Controller {
	if len(opts.Targets) == 0 {
		return nil
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{
		targets: opts.Targets,
		log:     log,
		metrics: newMetrics(opts.Registry),
	}
	// Publish a zero for every series up front, so a dashboard shows
	// "0 failures" rather than "no data" for a target that has not failed
	// yet — the difference between a healthy panel and an unreadable one.
	for _, t := range c.targets {
		c.metrics.init(t.Key())
	}
	return c
}

// Targets reports how many targets are being reconciled.
func (c *Controller) Targets() int {
	if c == nil {
		return 0
	}
	return len(c.targets)
}

// Start launches one goroutine per target and returns immediately. The
// goroutines stop when ctx is cancelled or Close is called.
//
// Nothing is reconciled at Start: the startup path already resolved
// every source, so an immediate cycle would re-probe content that is by
// definition current. The first probe happens one (jittered) interval
// later.
//
// Calling Start twice is a no-op after the first.
func (c *Controller) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.running = true
	for _, t := range c.targets {
		c.wg.Add(1)
		go func(t Target) {
			defer c.wg.Done()
			c.loop(runCtx, t)
		}(t)
	}
	c.log.Info("collection refresh started", "targets", len(c.targets))
}

// Close stops every loop and waits for the in-flight cycles to finish.
//
// Waiting matters: a cycle that is mid-swap holds the collection's
// reload slot and may be about to install a snapshot. Returning before
// it finished would let the process tear the registry down underneath
// it. Close is idempotent.
func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil
	}
	cancel := c.cancel
	c.running = false
	c.cancel = nil
	c.mu.Unlock()

	cancel()
	c.wg.Wait()
	return nil
}

// ReloadNow runs one cycle for every target, right now, sequentially.
//
// It is the ADMIN TRIGGER, and it deliberately goes through the very
// same Reconcile call the scheduled loop uses: a second update path
// would be a second set of bugs, a second place to forget the staging
// discipline, and a second answer to "what is this replica serving".
//
// Sequential rather than concurrent so that an operator reloading a
// deployment with many collections produces a predictable, bounded burst
// against the bucket rather than a fan-out. Errors from every target are
// joined, so one broken collection does not hide the others' outcomes.
func (c *Controller) ReloadNow(ctx context.Context) error {
	if c == nil {
		return nil
	}
	var errs []error
	for _, t := range c.targets {
		if err := c.runOnce(ctx, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// loop is one target's schedule.
func (c *Controller) loop(ctx context.Context, t Target) {
	spec := t.Spec()
	timer := time.NewTimer(spec.Delay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = c.runOnce(ctx, t)
			timer.Reset(spec.Delay())
		}
	}
}

// runOnce performs one cycle and records it.
func (c *Controller) runOnce(ctx context.Context, t Target) error {
	key := t.Key()
	start := time.Now()
	c.metrics.attempts(key).Inc()
	out, err := t.Reconcile(ctx)
	c.metrics.duration(key).Observe(time.Since(start).Seconds())

	switch {
	case errors.Is(err, ErrBusy):
		// Not a failure: the previous cycle (or an admin reload) is still
		// running. Nothing is degraded and nothing is retried early — the
		// in-flight cycle will finish and the next tick will see its
		// result.
		c.metrics.skipped(key).Inc()
		c.log.Debug("collection refresh skipped: already in flight",
			"collection", key.Name, "kind", key.Kind)
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Shutdown, or a caller-imposed deadline. Counting it as a refresh
		// failure would make every graceful restart look like an incident.
		c.log.Debug("collection refresh cancelled", "collection", key.Name, "kind", key.Kind)
		return nil
	case err != nil:
		c.metrics.failures(key).Inc()
		c.metrics.setDegraded(key, true)
		c.log.Error("collection refresh failed — continuing to serve the last known-good content",
			"collection", key.Name, "kind", key.Kind, "policy", t.Spec().Policy(), "error", err.Error())
		return err
	}

	c.metrics.setDegraded(key, false)
	c.metrics.lastSuccess(key).SetToCurrentTime()
	if out.Changed {
		c.metrics.changes(key).Inc()
		c.log.Info("collection refreshed",
			"collection", key.Name, "kind", key.Kind, "version", out.Version)
	}
	return nil
}
