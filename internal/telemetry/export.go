package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// export.go is the span pipeline between "a span ended" and "bytes left
// the process", and it exists to make three promises enforceable rather
// than aspirational:
//
//  1. An unreachable collector costs a FIXED, configured amount of
//     memory. The queue is a buffered channel of exactly QueueSize; a
//     span that arrives when it is full is dropped and counted, never
//     buffered, and the enqueue never blocks the request that produced
//     it.
//  2. An export failure is VISIBLE without the collector. It increments
//     meerkat_otel_export_failures_total and writes one log line per
//     minute, rate-limited — because an exporter failing is a condition
//     that repeats every batch, and the log it would otherwise produce
//     is itself an outage.
//  3. Shutdown is BOUNDED. The final flush gets the configured timeout
//     and then the process carries on. A pod being terminated must not
//     sit in Terminating because its collector went first.
//
// The OpenTelemetry SDK ships a batch span processor that does most of
// this. meerkat has its own because the parts that matter here — how
// many spans were dropped, whether the queue bound actually holds, and
// what a failing exporter does — are not observable from outside the
// SDK's, so they could be documented but not tested. This one is about
// a hundred lines and every one of those properties has a test.

// batchOptions configures newBatchProcessor.
type batchOptions struct {
	queueSize     int
	batchSize     int
	batchTimeout  time.Duration
	exportTimeout time.Duration
	log           *slog.Logger
	metrics       *Metrics
}

// batchProcessor is a bounded, non-blocking sdktrace.SpanProcessor.
type batchProcessor struct {
	exp     sdktrace.SpanExporter
	opts    batchOptions
	queue   chan sdktrace.ReadOnlySpan
	flushCh chan chan struct{}
	stop    chan struct{}
	done    chan struct{}

	stopOnce sync.Once
	dropped  atomic.Int64
	limiter  *rateLimiter
}

func newBatchProcessor(exp sdktrace.SpanExporter, opts batchOptions) *batchProcessor {
	if opts.queueSize <= 0 {
		opts.queueSize = defaultQueueSize
	}
	if opts.batchSize <= 0 || opts.batchSize > opts.queueSize {
		opts.batchSize = min(defaultBatchSize, opts.queueSize)
	}
	if opts.batchTimeout <= 0 {
		opts.batchTimeout = defaultBatchTimeout
	}
	if opts.exportTimeout <= 0 {
		opts.exportTimeout = defaultExportTimeout
	}
	if opts.log == nil {
		opts.log = slog.New(slog.DiscardHandler)
	}
	b := &batchProcessor{
		exp:     exp,
		opts:    opts,
		queue:   make(chan sdktrace.ReadOnlySpan, opts.queueSize),
		flushCh: make(chan chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		limiter: newRateLimiter(time.Minute),
	}
	go b.run()
	return b
}

// OnStart is required by the interface and deliberately does nothing:
// span attributes are set by the instrumented code, not injected here.
func (b *batchProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd enqueues a finished span, or drops it.
//
// The non-blocking send is the whole design. This runs on the goroutine
// that served the request; blocking it on a full queue would make a slow
// or dead collector into request latency, which is precisely the
// coupling this subsystem is not allowed to have.
func (b *batchProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	select {
	case b.queue <- s:
	default:
		b.dropped.Add(1)
		b.opts.metrics.SpansDropped(1)
		if b.limiter.allow() {
			b.opts.log.Warn("telemetry span queue is full — dropping spans",
				"queue_size", b.opts.queueSize, "dropped_total", b.dropped.Load())
		}
	}
}

// ForceFlush exports everything queued and waits for it, bounded by ctx.
func (b *batchProcessor) ForceFlush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case b.flushCh <- ack:
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown stops the worker, drains what it can within ctx, and shuts
// the exporter down. Idempotent.
func (b *batchProcessor) Shutdown(ctx context.Context) error {
	b.stopOnce.Do(func() { close(b.stop) })
	select {
	case <-b.done:
	case <-ctx.Done():
		// The worker is still draining. Returning here rather than
		// waiting is the bounded-shutdown promise: the process continues
		// to exit and the remaining spans are lost, which is the correct
		// trade against a pod that never terminates.
		return ctx.Err()
	}
	return b.exp.Shutdown(ctx)
}

// run is the single worker: accumulate, then export on a full batch, on
// the batch timer, on an explicit flush, or at shutdown.
func (b *batchProcessor) run() {
	defer close(b.done)
	timer := time.NewTimer(b.opts.batchTimeout)
	defer timer.Stop()
	batch := make([]sdktrace.ReadOnlySpan, 0, b.opts.batchSize)

	for {
		select {
		case <-b.stop:
			batch = b.drain(batch)
			b.export(batch)
			return
		case ack := <-b.flushCh:
			batch = b.drain(batch)
			b.export(batch)
			batch = batch[:0]
			close(ack)
		case <-timer.C:
			b.export(batch)
			batch = batch[:0]
			timer.Reset(b.opts.batchTimeout)
		case s := <-b.queue:
			batch = append(batch, s)
			if len(batch) >= b.opts.batchSize {
				b.export(batch)
				batch = batch[:0]
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(b.opts.batchTimeout)
			}
		}
	}
}

// drain moves everything currently queued into batch without blocking.
func (b *batchProcessor) drain(batch []sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	for {
		select {
		case s := <-b.queue:
			batch = append(batch, s)
		default:
			return batch
		}
	}
}

// export ships one batch. A failure is counted and (rarely) logged, and
// then forgotten: there is no retry queue, because a retry queue is an
// unbounded one wearing a disguise, and losing telemetry is always the
// right answer over holding a request or a shutdown hostage to it.
func (b *batchProcessor) export(batch []sdktrace.ReadOnlySpan) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.opts.exportTimeout)
	defer cancel()
	if err := b.exp.ExportSpans(ctx, batch); err != nil {
		b.opts.metrics.ExportFailed(SignalTraces)
		if b.limiter.allow() {
			b.opts.log.Warn("telemetry export failed — serving is unaffected",
				"signal", SignalTraces, "spans", len(batch), "error", err.Error())
		}
	}
}

// rateLimiter permits one event per window. It is what keeps a dead
// collector from producing a log line per batch forever — the failure is
// counted every time and described occasionally, which is the right way
// round for a condition that repeats.
type rateLimiter struct {
	window time.Duration
	mu     sync.Mutex
	last   time.Time
}

func newRateLimiter(window time.Duration) *rateLimiter {
	return &rateLimiter{window: window}
}

func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < r.window {
		return false
	}
	r.last = now
	return true
}

// errorHandler receives the OpenTelemetry SDK's own internal errors.
//
// It is installed globally (otel.SetErrorHandler) because that is the
// only interface the SDK offers, and it does two things the default
// does not: it counts, so an operator sees the failure on /metrics
// without a working collector, and it writes to meerkat's structured
// logger on STDERR rather than through the standard logger — stdout is
// the stdio MCP transport's wire and nothing here may ever touch it.
type errorHandler struct {
	log     *slog.Logger
	metrics *Metrics
	limiter *rateLimiter
}

func (h *errorHandler) Handle(err error) {
	if err == nil {
		return
	}
	h.metrics.ExportFailed(SignalTraces)
	if h.limiter.allow() {
		h.log.Warn("opentelemetry sdk error — serving is unaffected", "error", err.Error())
	}
}
