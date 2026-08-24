package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// --- shared helpers ----------------------------------------------------

// syncBuffer is a bytes.Buffer safe for a logger writing from the export
// worker while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testLogger(w *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newTestTelemetry builds a fully-wired *Telemetry over an in-memory (or
// deliberately broken) exporter. Nothing it constructs touches a socket.
func newTestTelemetry(t *testing.T, exp sdktrace.SpanExporter, reg *prometheus.Registry) *Telemetry {
	t.Helper()
	tel, err := New(context.Background(), Options{
		Config: &Config{
			Traces: TraceConfig{Enabled: true},
			// A short batch timeout so a test that does not force a flush
			// still finishes quickly.
			Limits: ExportLimits{BatchTimeout: Duration(50 * time.Millisecond)},
		},
		Registry:     reg,
		Version:      "test",
		SpanExporter: exp,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tel == nil {
		t.Fatal("New returned nil for a configuration that enabled traces")
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

// counterValue reads one counter (or the sum of a vector's series) out
// of a registry by metric name.
func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	total := 0.0
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if c := m.GetCounter(); c != nil {
				total += c.GetValue()
			}
		}
	}
	return total
}

// --- the nil path ------------------------------------------------------

func TestNilTelemetryIsAWorkingImplementationOfOff(t *testing.T) {
	// This is the zero-configuration contract in miniature. Every one of
	// these calls happens on a request path in a deployment that
	// configured nothing, and every one must be a nil check and a return
	// — not a panic, and not an allocation.
	var tel *Telemetry
	ctx := context.Background()

	if tel.Enabled() || tel.Tracing() || tel.IncludeTraceContext() {
		t.Error("a nil *Telemetry must report itself as off")
	}
	if tel.Metrics() != nil {
		t.Error("a nil *Telemetry must have no metrics recorder")
	}
	if got := tel.LogAttrs(ctx); got != nil {
		t.Errorf("LogAttrs on nil telemetry = %v, want nil — an unconfigured deployment's "+
			"access log must be byte-identical to what it was", got)
	}
	if tel.HTTPClient(nil) != nil {
		t.Error("HTTPClient(nil) on nil telemetry must return nil, so internal/authn builds its own as before")
	}
	base := &http.Client{}
	if tel.HTTPClient(base) != base {
		t.Error("HTTPClient must return the caller's own client untouched when telemetry is off")
	}
	if err := tel.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown on nil telemetry = %v, want nil", err)
	}
	if err := tel.ForceFlush(ctx); err != nil {
		t.Errorf("ForceFlush on nil telemetry = %v, want nil", err)
	}
}

func TestSpanOnAnUninstrumentedContextIsFree(t *testing.T) {
	ctx := context.Background()
	got, span := Span(ctx, "meerkat.test", KeySearchResults.Int(3))
	if got != ctx {
		t.Error("Span must return the caller's own context unchanged when there is no telemetry — " +
			"a derived context per call would be a cost paid by every deployment that opted out")
	}
	if span.SpanContext().IsValid() {
		t.Error("the returned span must be non-recording")
	}
	// Every method has to be safe on it, since call sites do not check.
	span.SetAttributes(Outcome(OutcomeOK))
	span.End()

	if Record(ctx) != nil {
		t.Error("Record on an uninstrumented context must be nil")
	}
	// And a nil recorder must absorb every call.
	Record(ctx).Searched(OutcomeOK, 0.1, 5)
	Record(ctx).IndexBuilt(OutcomeOK, 0.1)
	Record(ctx).MemorySaved("personal", OutcomeSaved)
	Record(ctx).CacheLookup(SourceGCSObject, CacheHit)
	Record(ctx).Downloaded(SourceURL, 100)
	Record(ctx).SetIndexedPages(7)
	Record(ctx).ToolPayload("mk_search", "request", 12)
	Record(ctx).MemoryBackend(BackendLocal, MemorySave, 0.1, false)
	Record(ctx).Ambiguous()
	Record(ctx).ExportFailed(SignalTraces)
	Record(ctx).SpansDropped(1)
	Record(ctx).SourceResolved(SourceLocal, OutcomeOK, 0.1)
}

func TestNewReturnsNilForAConfigurationThatAskedForNothing(t *testing.T) {
	clearOTelEnv(t)
	tel, err := New(context.Background(), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tel != nil {
		t.Fatal("New must return nil telemetry when nothing was configured — that nil is what " +
			"makes the hosted server's zero-configuration path identical to the pre-tracing one")
	}
}

func TestNewDoesNotTouchTheOpenTelemetryGlobalsUnlessAsked(t *testing.T) {
	clearOTelEnv(t)
	before := otelTracerProvider()
	tel := newTestTelemetry(t, tracetest.NewInMemoryExporter(), nil)
	if tel == nil {
		t.Fatal("expected telemetry")
	}
	if otelTracerProvider() != before {
		t.Fatal("New must not install itself as the process-global tracer provider unless " +
			"SetGlobals was set — a test binary runs several servers and exactly one may own a global")
	}
}

// --- context propagation ------------------------------------------------

func TestExtractContinuesAValidInboundTraceContext(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	tel := newTestTelemetry(t, exp, nil)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	h := http.Header{}
	h.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	ctx := tel.Extract(context.Background(), h)
	_, span := tel.Start(ctx, "meerkat.test")
	got := span.SpanContext().TraceID().String()
	span.End()

	if got != traceID {
		t.Fatalf("trace id = %s, want %s — a request that arrives with a valid traceparent must "+
			"join that trace rather than starting its own", got, traceID)
	}
}

func TestExtractIgnoresAMalformedTraceContextSafely(t *testing.T) {
	clearOTelEnv(t)
	tel := newTestTelemetry(t, tracetest.NewInMemoryExporter(), nil)

	for _, bad := range []string{
		"garbage",
		"00-tooshort-00f067aa0ba902b7-01",
		"99-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-0000000000000000-01",
		strings.Repeat("A", 4096),
	} {
		h := http.Header{}
		h.Set("traceparent", bad)
		ctx := tel.Extract(context.Background(), h)
		_, span := tel.Start(ctx, "meerkat.test")
		sc := span.SpanContext()
		span.End()
		if !sc.IsValid() {
			t.Errorf("traceparent %q produced no usable span — a malformed header must be ignored, "+
				"leaving a fresh root trace, not suppress instrumentation", bad)
		}
		if sc.TraceID().String() == "00000000000000000000000000000000" {
			t.Errorf("traceparent %q leaked an invalid trace id into the span", bad)
		}
	}
}

func TestHTTPClientInjectsTraceContextOutbound(t *testing.T) {
	clearOTelEnv(t)
	tel := newTestTelemetry(t, tracetest.NewInMemoryExporter(), nil)

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := tel.HTTPClient(srv.Client())
	ctx, span := tel.Start(context.Background(), "meerkat.test")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	span.End()

	if got == "" {
		t.Fatal("an outbound request through the instrumented client must carry traceparent — " +
			"this is what puts a slow OIDC discovery in the same trace as the request waiting for it")
	}
	if want := span.SpanContext().TraceID().String(); !strings.Contains(got, want) {
		t.Fatalf("traceparent %q does not carry the caller's trace id %s", got, want)
	}
}

func TestOutboundClientSpanCarriesNoPathOrQuery(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	tel := newTestTelemetry(t, exp, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := tel.HTTPClient(srv.Client())
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/.well-known/openid-configuration?secret=leaked", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, s := range exp.GetSpans() {
		for _, a := range s.Attributes {
			if strings.Contains(a.Value.String(), "secret=leaked") || strings.Contains(a.Value.String(), "well-known") {
				t.Fatalf("client span attribute %s=%s carries the request path/query; only method, "+
					"scheme and host may be recorded", a.Key, a.Value.String())
			}
		}
	}
}

func TestLogAttrsCarryIDsAndNothingElse(t *testing.T) {
	clearOTelEnv(t)
	tel := newTestTelemetry(t, tracetest.NewInMemoryExporter(), nil)

	ctx, span := tel.Start(context.Background(), "meerkat.test")
	defer span.End()
	attrs := tel.LogAttrs(ctx)
	if len(attrs) != 4 {
		t.Fatalf("LogAttrs = %v, want exactly trace_id and span_id — the log/trace bridge is "+
			"two IDs wide, deliberately", attrs)
	}
	if attrs[0] != "trace_id" || attrs[2] != "span_id" {
		t.Fatalf("LogAttrs keys = %v, want trace_id and span_id", attrs)
	}
	if attrs[1] != span.SpanContext().TraceID().String() {
		t.Errorf("trace_id does not match the active span")
	}
}

func TestLogAttrsAreEmptyWithoutAnActiveSpan(t *testing.T) {
	clearOTelEnv(t)
	tel := newTestTelemetry(t, tracetest.NewInMemoryExporter(), nil)
	if got := tel.LogAttrs(context.Background()); got != nil {
		t.Fatalf("LogAttrs with no active span = %v, want nil", got)
	}
}

func TestIncludeTraceContextCanBeTurnedOff(t *testing.T) {
	clearOTelEnv(t)
	off := false
	tel, err := New(context.Background(), Options{
		Config: &Config{
			Traces: TraceConfig{Enabled: true},
			Logs:   LogConfig{IncludeTraceContext: &off},
		},
		SpanExporter: tracetest.NewInMemoryExporter(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	ctx, span := tel.Start(context.Background(), "meerkat.test")
	defer span.End()
	if got := tel.LogAttrs(ctx); got != nil {
		t.Fatalf("logs.include_trace_context: false must keep the log shape unchanged, got %v", got)
	}
}

// --- sampling -----------------------------------------------------------

func TestSampleRatioZeroRecordsNothing(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	zero := 0.0
	tel, err := New(context.Background(), Options{
		Config:       &Config{Traces: TraceConfig{Enabled: true, SampleRatio: &zero}},
		SpanExporter: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	for range 50 {
		_, span := tel.Start(context.Background(), "meerkat.test")
		span.End()
	}
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("sample_ratio 0 exported %d spans, want 0", got)
	}
}

func TestSamplingIsParentBasedSoAGatewaysDecisionIsHonoured(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	zero := 0.0
	tel, err := New(context.Background(), Options{
		// This process would sample NOTHING of its own. A caller who
		// already decided to keep the trace must still get a complete one:
		// half a trace is worse than none, and re-sampling under a parent
		// is what produces one.
		Config:       &Config{Traces: TraceConfig{Enabled: true, SampleRatio: &zero}},
		SpanExporter: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	h := http.Header{}
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx := tel.Extract(context.Background(), h)
	_, span := tel.Start(ctx, "meerkat.test")
	span.End()

	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(exp.GetSpans()); got != 1 {
		t.Fatalf("exported %d spans, want 1 — a sampled parent must be honoured even at ratio 0", got)
	}
}

// --- bounded vocabularies ------------------------------------------------

func TestBoundedToolClampsAnUnknownName(t *testing.T) {
	for _, known := range []string{"mk_search", "mk_show", "mk_list", "mk_list_collections", "mk_save_memory"} {
		if got := BoundedTool(known); got != known {
			t.Errorf("BoundedTool(%q) = %q, want it unchanged", known, got)
		}
	}
	for _, unknown := range []string{"", "mk_search'; DROP", strings.Repeat("x", 500), "../../etc/passwd"} {
		if got := BoundedTool(unknown); got != "other" {
			t.Errorf("BoundedTool(%q) = %q, want \"other\" — an unbounded tool label is a "+
				"cardinality incident waiting for a transport that dispatches differently", unknown, got)
		}
	}
}

func TestSourceTypeIsAClosedSet(t *testing.T) {
	allowed := map[string]bool{
		SourceEmbedded: true, SourceLocal: true, SourceURL: true,
		SourceGCSObject: true, SourceGCSPrefix: true, SourceOther: true,
	}
	cases := []struct {
		typ                string
		hasObject, hasPref bool
		want               string
	}{
		{"none", false, false, SourceEmbedded},
		{"", false, false, SourceEmbedded},
		{"local", false, false, SourceLocal},
		{"url", false, false, SourceURL},
		{"gcs", true, false, SourceGCSObject},
		{"gcs", false, true, SourceGCSPrefix},
		{"gcs", false, false, SourceOther},
		{"something-new", false, false, SourceOther},
	}
	for _, c := range cases {
		got := SourceType(c.typ, c.hasObject, c.hasPref)
		if got != c.want {
			t.Errorf("SourceType(%q, %v, %v) = %q, want %q", c.typ, c.hasObject, c.hasPref, got, c.want)
		}
		if !allowed[got] {
			t.Errorf("SourceType returned %q, which is outside the documented closed set", got)
		}
	}
}

func TestFailRecordsABoundedReasonAndNoErrorObject(t *testing.T) {
	clearOTelEnv(t)
	exp := tracetest.NewInMemoryExporter()
	tel := newTestTelemetry(t, exp, nil)

	_, span := tel.Start(context.Background(), "meerkat.test")
	Fail(span, OutcomeInvalidQuery)
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if n := len(spans[0].Events); n != 0 {
		t.Fatalf("Fail recorded %d events; it must NOT record the error object — an error's "+
			"text can quote a query or a page ID", n)
	}
	found := false
	for _, a := range spans[0].Attributes {
		if a.Key == KeyOutcome && a.Value.String() == OutcomeInvalidQuery {
			found = true
		}
	}
	if !found {
		t.Fatalf("Fail must set the bounded outcome attribute, got %v", spans[0].Attributes)
	}
}

// otelTracerProvider reads the process-global tracer provider, so the
// test above can assert that New left it alone.
func otelTracerProvider() trace.TracerProvider { return otel.GetTracerProvider() }
