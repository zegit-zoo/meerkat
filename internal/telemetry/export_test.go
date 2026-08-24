package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// export_test.go proves the three promises the span pipeline makes, all
// of which are about what happens when the COLLECTOR IS NOT THERE:
//
//	bounded memory      a full queue drops and counts, it does not grow
//	visible failure     an export error is counted locally and logged
//	                    rarely, and never propagates
//	bounded shutdown    a hung exporter cannot hold the process open
//
// Every test here uses an in-memory or deliberately-broken exporter.
// Nothing dials, listens, or resolves a name.

// blockingExporter hangs in ExportSpans until released, and reports when
// it started. It is how a dead collector is simulated without one.
type blockingExporter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingExporter() *blockingExporter {
	return &blockingExporter{started: make(chan struct{}), release: make(chan struct{})}
}

func (e *blockingExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *blockingExporter) Shutdown(context.Context) error { return nil }

// failingExporter always errors, like a collector answering 503.
type failingExporter struct {
	mu    sync.Mutex
	calls int
}

func (e *failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return errors.New("collector unavailable")
}

func (e *failingExporter) Shutdown(context.Context) error { return nil }

func (e *failingExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// tracedSpans produces n ended spans through bp, using a real SDK
// tracer so the ReadOnlySpans are the ones production would emit.
func tracedSpans(t *testing.T, bp sdktrace.SpanProcessor, n int) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(bp),
	)
	tr := tp.Tracer("test")
	for range n {
		_, span := tr.Start(context.Background(), "span")
		span.End()
	}
}

func TestBatchProcessor_ExportsOnForceFlush(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	// A long batch timeout, so the only thing that could have exported is
	// the flush itself.
	bp := newBatchProcessor(exp, batchOptions{batchTimeout: time.Hour, batchSize: 100})
	t.Cleanup(func() { _ = bp.Shutdown(context.Background()) })

	tracedSpans(t, bp, 3)
	if err := bp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if got := len(exp.GetSpans()); got != 3 {
		t.Fatalf("exported %d spans, want 3", got)
	}
}

func TestBatchProcessor_FullQueueDropsAndCountsRatherThanGrowing(t *testing.T) {
	// The property under test: an unreachable collector costs a FIXED
	// amount of memory. A blocking exporter wedges the worker, so
	// everything after the queue's capacity must be dropped — not
	// buffered, and above all not blocking the goroutine that produced
	// the span, which in production is serving a request.
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	exp := newBlockingExporter()
	const queueSize = 4
	bp := newBatchProcessor(exp, batchOptions{
		queueSize:    queueSize,
		batchSize:    1,
		batchTimeout: time.Hour,
		metrics:      m,
	})
	t.Cleanup(func() {
		close(exp.release)
		_ = bp.Shutdown(context.Background())
	})

	// One span gets the worker into ExportSpans, where it stays.
	tracedSpans(t, bp, 1)
	select {
	case <-exp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the exporter was never called")
	}

	// With the worker wedged, the queue holds queueSize and no more.
	const overflow = 20
	done := make(chan struct{})
	go func() {
		defer close(done)
		tracedSpans(t, bp, overflow)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnEnd blocked on a full queue — the span pipeline must never make a " +
			"request wait on the collector")
	}

	dropped := bp.dropped.Load()
	wantAtLeast := int64(overflow - queueSize)
	if dropped < wantAtLeast {
		t.Fatalf("dropped %d spans, want at least %d (queue capacity is %d)", dropped, wantAtLeast, queueSize)
	}
	if got := counterValue(t, reg, "meerkat_otel_spans_dropped_total"); got < float64(wantAtLeast) {
		t.Fatalf("meerkat_otel_spans_dropped_total = %v, want at least %v — a drop must be "+
			"visible on /metrics, which is the one place an operator can look when the "+
			"collector is the thing that is broken", got, float64(wantAtLeast))
	}
}

func TestBatchProcessor_ExportFailureIsCountedAndNeverPropagates(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	exp := &failingExporter{}
	bp := newBatchProcessor(exp, batchOptions{batchTimeout: time.Hour, batchSize: 100, metrics: m})
	t.Cleanup(func() { _ = bp.Shutdown(context.Background()) })

	tracedSpans(t, bp, 2)
	if err := bp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush must not surface an export failure to the caller: %v", err)
	}
	if exp.count() == 0 {
		t.Fatal("the exporter was never called")
	}
	if got := counterValue(t, reg, "meerkat_otel_export_failures_total"); got < 1 {
		t.Fatalf("meerkat_otel_export_failures_total = %v, want at least 1", got)
	}
}

func TestBatchProcessor_ExportFailureLoggingIsRateLimited(t *testing.T) {
	// A failing exporter fails on EVERY batch. Logging each one turns a
	// collector outage into a second outage, in the log pipeline.
	logs := &syncBuffer{}
	exp := &failingExporter{}
	bp := newBatchProcessor(exp, batchOptions{
		batchTimeout: time.Hour,
		batchSize:    1,
		log:          testLogger(logs),
	})
	t.Cleanup(func() { _ = bp.Shutdown(context.Background()) })

	tracedSpans(t, bp, 25)
	if err := bp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(logs.String(), "telemetry export failed"); lines > 1 {
		t.Fatalf("%d export-failure log lines for 25 failing batches; the limiter must "+
			"allow one per window", lines)
	}
}

func TestBatchProcessor_ShutdownIsBoundedWhenTheExporterHangs(t *testing.T) {
	exp := newBlockingExporter()
	bp := newBatchProcessor(exp, batchOptions{batchTimeout: time.Hour, batchSize: 1})
	t.Cleanup(func() { close(exp.release) })

	tracedSpans(t, bp, 1)
	select {
	case <-exp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the exporter was never called")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := bp.Shutdown(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Shutdown against a hung exporter should report the expired deadline for the log")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s against a hung exporter — a pod being terminated must not "+
			"sit in Terminating because its collector went first", elapsed)
	}
}

func TestBatchProcessor_ShutdownIsIdempotent(t *testing.T) {
	bp := newBatchProcessor(tracetest.NewInMemoryExporter(), batchOptions{})
	if err := bp.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := bp.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown must be a no-op, got: %v", err)
	}
}

func TestBatchProcessor_UnsampledSpansAreNotQueued(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	bp := newBatchProcessor(exp, batchOptions{batchTimeout: time.Hour, batchSize: 100})
	t.Cleanup(func() { _ = bp.Shutdown(context.Background()) })

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.NeverSample()),
		sdktrace.WithSpanProcessor(bp),
	)
	tr := tp.Tracer("test")
	for range 10 {
		_, span := tr.Start(context.Background(), "span")
		span.End()
	}
	if err := bp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("exported %d unsampled spans, want 0 — sampling has to bound the export "+
			"volume, not merely the storage bill", got)
	}
}

// countingExporter records how many spans it was handed and — unlike
// tracetest.InMemoryExporter, which RESETS itself on Shutdown — survives
// being shut down, which is exactly what the graceful-flush test has to
// observe.
type countingExporter struct {
	mu sync.Mutex
	n  int
}

func (e *countingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	e.n += len(spans)
	e.mu.Unlock()
	return nil
}

func (e *countingExporter) Shutdown(context.Context) error { return nil }

func (e *countingExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

func TestOTLPExportIsConfigurableWithoutTouchingTheNetwork(t *testing.T) {
	clearOTelEnv(t)
	// The documented configuration, pointed at an address nothing is
	// listening on. Construction must succeed and must not block:
	// OTLP/gRPC dials lazily, and a collector that is not up yet is a
	// normal state at startup, not a reason to fail a process whose job
	// is serving a knowledge base.
	for _, protocol := range []string{ProtocolGRPC, ProtocolHTTP} {
		t.Run(protocol, func(t *testing.T) {
			done := make(chan *Telemetry, 1)
			go func() {
				tel, err := New(context.Background(), Options{
					Config: &Config{
						ServiceName: "meerkat",
						Environment: "production",
						Traces:      TraceConfig{Enabled: true},
						OTLP: OTLPConfig{
							Endpoint: "127.0.0.1:1",
							Protocol: protocol,
							Insecure: true,
						},
						Limits: ExportLimits{ShutdownTimeout: Duration(200 * time.Millisecond)},
					},
				})
				if err != nil {
					t.Errorf("New with a documented OTLP configuration: %v", err)
				}
				done <- tel
			}()
			select {
			case tel := <-done:
				if tel == nil {
					t.Fatal("New returned nil for a configuration that enabled traces and OTLP")
				}
				if !tel.cfg.Exports() {
					t.Error("a configured endpoint must resolve as exporting")
				}
				// Recording a span against a dead collector must not block
				// the caller either.
				_, span := tel.Start(context.Background(), "meerkat.test")
				span.End()
				_ = tel.Shutdown(context.Background())
			case <-time.After(10 * time.Second):
				t.Fatal("New blocked constructing an OTLP exporter — startup must not wait on a collector")
			}
		})
	}
}

func TestOTLPMetricsWithNoEndpointIsARefusalNotASilentNoOp(t *testing.T) {
	clearOTelEnv(t)
	// `metrics.otlp: true` with nowhere to send them is a configuration an
	// operator believes is working. Failing at load is the whole point of
	// validating here rather than shrugging at runtime.
	_, err := New(context.Background(), Options{
		Config: &Config{Metrics: MetricConfig{OTLP: true}},
	})
	if err == nil {
		t.Fatal("metrics.otlp: true with no endpoint must be refused")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("the error must name the missing setting, got: %v", err)
	}
}

func TestTracingWithNoEndpointStillCorrelatesAndExportsNothing(t *testing.T) {
	clearOTelEnv(t)
	// A useful middle state, and the one every test in this repo runs in:
	// spans exist (so logs correlate and a future collector can be added
	// without a code change) and nothing leaves the process.
	tel, err := New(context.Background(), Options{
		Config: &Config{Traces: TraceConfig{Enabled: true}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	if tel.cfg.Exports() {
		t.Fatal("no endpoint must resolve as not exporting")
	}
	ctx, span := tel.Start(context.Background(), "meerkat.test")
	defer span.End()
	if attrs := tel.LogAttrs(ctx); len(attrs) == 0 {
		t.Fatal("tracing with no exporter must still correlate the logs — that is most of the value")
	}
}

func TestTelemetry_ShutdownFlushesWhatWasRecorded(t *testing.T) {
	clearOTelEnv(t)
	exp := &countingExporter{}
	tel := newTestTelemetry(t, exp, nil)

	_, span := tel.Start(context.Background(), "meerkat.test")
	span.End()

	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := exp.count(); got != 1 {
		t.Fatalf("shutdown exported %d spans, want 1 — the graceful path must flush what it has", got)
	}
}

func TestTelemetry_ShutdownAgainstADeadCollectorIsBoundedByItsTimeout(t *testing.T) {
	clearOTelEnv(t)
	exp := newBlockingExporter()
	t.Cleanup(func() { close(exp.release) })

	tel, err := New(context.Background(), Options{
		Config: &Config{
			Traces: TraceConfig{Enabled: true},
			Limits: ExportLimits{
				BatchTimeout: Duration(10 * time.Millisecond),
				// The promise: shutdown is bounded by THIS, not by the
				// collector's willingness to answer.
				ShutdownTimeout: Duration(200 * time.Millisecond),
			},
		},
		SpanExporter: exp,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, span := tel.Start(context.Background(), "meerkat.test")
	span.End()
	select {
	case <-exp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the exporter was never called")
	}

	start := time.Now()
	// The error is returned for the log; what matters is that it comes
	// back at all, promptly.
	_ = tel.Shutdown(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s against an unreachable collector with a 200ms shutdown timeout — "+
			"a pod being terminated must not wait on its collector", elapsed)
	}
}
