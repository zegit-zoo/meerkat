package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpapi "github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// observability_test.go is issue #30's acceptance criteria, driven
// through the real hosted transport with a real MCP client.
//
// Two things are true of every test in this file:
//
//   - No collector, no socket, no network. Spans go to
//     sdk/trace/tracetest's in-memory exporter, injected through
//     HostedConfig.Telemetry — the seam that exists for exactly this.
//   - The disclosure rules are tested, not asserted. The last test walks
//     EVERY recorded span and EVERY exported metric label and fails on
//     any of the forbidden value classes, so an attribute added later
//     without thinking has somewhere to trip.

// tracedFixture is a hosted server with tracing on and an in-memory
// exporter behind it.
type tracedFixture struct {
	t      *testing.T
	srv    *HostedServer
	http   *httptest.Server
	issuer *authntest.Issuer
	spans  *tracetest.InMemoryExporter
	reg    *prometheus.Registry
	logs   *syncBuffer
	tel    *telemetry.Telemetry
}

// syncBuffer is a log sink safe for the export worker and the test
// goroutine at once.
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

// tracedOptions tweaks a fixture before it is built.
type tracedOptions struct {
	// rules, when non-nil, puts the server behind the fake OIDC issuer.
	rules []authz.Rule
	// exporter overrides the in-memory one (to simulate an outage).
	exporter sdktrace.SpanExporter
	// sampleRatio overrides the default of 1.0.
	sampleRatio *float64
	// withMemory mounts a single collection with a local memory store
	// instead of the three read-only ones.
	withMemory bool
}

func newTracedFixture(t *testing.T, opts tracedOptions) *tracedFixture {
	t.Helper()
	// Traces are configured through the real Config type, so the test
	// exercises the same Resolve path a deployment does.
	for _, name := range []string{
		"OTEL_SDK_DISABLED", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}

	f := &tracedFixture{t: t, logs: &syncBuffer{}, reg: prometheus.NewRegistry()}
	f.spans = tracetest.NewInMemoryExporter()

	exporter := opts.exporter
	if exporter == nil {
		exporter = f.spans
	}

	logger := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tel, err := telemetry.New(context.Background(), telemetry.Options{
		Config: &telemetry.Config{
			ServiceName: "meerkat-test",
			Traces:      telemetry.TraceConfig{Enabled: true, SampleRatio: opts.sampleRatio},
			Limits:      telemetry.ExportLimits{BatchTimeout: telemetry.Duration(20 * time.Millisecond)},
		},
		Registry:     f.reg,
		Logger:       logger,
		Version:      "test",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	f.tel = tel

	var authCfg *authz.Config
	var client *http.Client
	if opts.rules != nil {
		f.issuer = authntest.NewIssuer(t)
		client = f.issuer.Client()
		authCfg = &authz.Config{
			Resource:  testResource,
			Providers: []authz.Provider{{Issuer: f.issuer.URL, Audience: testAudience}},
			Rules:     opts.rules,
		}
	}

	reg := threeCollectionRegistry(t)
	if opts.withMemory {
		reg, err = collections.New(
			withMemoryStore(t, collections.FromPages("notes", []kb.Page{
				testPage("handbook/onboarding", "Onboarding", "how we onboard", "handbook", "reviewed", "team-a"),
			}), t.TempDir()),
		)
		if err != nil {
			t.Fatalf("collections.New: %v", err)
		}
	}

	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: reg,
		Auth:        authCfg,
		Version:     "test",
		HTTPClient:  client,
		Logger:      logger,
		Metrics:     f.reg,
		Telemetry:   tel,
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	f.srv = srv
	f.http = httptest.NewServer(srv.Handler())
	t.Cleanup(f.http.Close)
	return f
}

func (f *tracedFixture) mcpURL() string { return f.http.URL + f.srv.EndpointPath() }

func (f *tracedFixture) token(subject string, groups ...string) string {
	f.t.Helper()
	return f.issuer.Token(f.t, authntest.Claims{
		Subject: subject, Audience: testAudience, Groups: groups,
		Email: subject + "@example.com", Tenant: "acme",
	})
}

func (f *tracedFixture) client(ctx context.Context, bearer string, headers map[string]string) *mcpclient.Client {
	f.t.Helper()
	h := map[string]string{}
	for k, v := range headers {
		h[k] = v
	}
	if bearer != "" {
		h["Authorization"] = "Bearer " + bearer
	}
	var opts []transport.StreamableHTTPCOption
	if len(h) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(h))
	}
	c, err := mcpclient.NewStreamableHttpClient(f.mcpURL(), opts...)
	if err != nil {
		f.t.Fatalf("new client: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		f.t.Fatalf("start client: %v", err)
	}
	f.t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Initialize(ctx, mcpapi.InitializeRequest{}); err != nil {
		f.t.Fatalf("initialize: %v", err)
	}
	return c
}

// flush exports everything queued and returns the recorded spans.
func (f *tracedFixture) flush() tracetest.SpanStubs {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.tel.ForceFlush(ctx); err != nil {
		f.t.Fatalf("ForceFlush: %v", err)
	}
	return f.spans.GetSpans()
}

// spanNamed returns the first recorded span with the given name.
func spanNamed(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// traceContaining narrows a recording to the one trace holding the named
// span.
//
// It exists because an MCP client makes SEVERAL requests to complete one
// tool call — initialize, then tools/call, each its own POST and
// therefore its own root span and its own trace. Picking "the first
// POST /mcp span" would compare the initialize handshake's root against
// the tool call's child and report a break that is not there. The trace
// is the unit; this function is how a test names it.
func traceContaining(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	t.Helper()
	anchor, ok := spanNamed(spans, name)
	if !ok {
		t.Fatalf("no span named %q was recorded; recorded: %v", name, spanNames(spans))
	}
	want := anchor.SpanContext.TraceID()
	out := make(tracetest.SpanStubs, 0, len(spans))
	for _, s := range spans {
		if s.SpanContext.TraceID() == want {
			out = append(out, s)
		}
	}
	return out
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}

func attrValue(s tracetest.SpanStub, key string) (string, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.String(), true
		}
	}
	return "", false
}

// --- acceptance: one trace, root to search --------------------------------

func TestObservability_SearchProducesOneTraceFromHTTPToSearch(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := f.client(ctx, "", nil)
	if _, isErr := callText(t, ctx, c, toolSearch, map[string]any{"query": "incident"}); isErr {
		t.Fatal("mk_search returned a tool error")
	}

	// The acceptance criterion is that the three share ONE trace: an
	// operator following a slow mk_search must get from the wire to the
	// bleve query without changing traces.
	trace := traceContaining(t, f.flush(), telemetry.SpanSearch)
	root, ok := spanNamed(trace, "POST /mcp")
	if !ok {
		t.Fatalf("the search span's trace has no root HTTP server span; it holds: %v", spanNames(trace))
	}
	tool, ok := spanNamed(trace, telemetry.SpanMCPTool)
	if !ok {
		t.Fatalf("the search span's trace has no MCP tool span; it holds: %v", spanNames(trace))
	}
	search, _ := spanNamed(trace, telemetry.SpanSearch)

	if tool.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Errorf("the MCP tool span's parent is not the root HTTP span — the request context is "+
			"not reaching the tool handler. Trace holds: %v", spanNames(trace))
	}
	if search.Parent.SpanID() != tool.SpanContext.SpanID() {
		t.Errorf("the search span's parent is not the MCP tool span")
	}
	if !root.Parent.SpanID().IsValid() && root.Parent.TraceID().IsValid() {
		t.Error("the root span should have no parent when the caller sent no trace context")
	}

	if got, _ := attrValue(tool, string(telemetry.KeyMCPTool)); got != toolSearch {
		t.Errorf("tool span's tool attribute = %q, want %q", got, toolSearch)
	}
	if got, _ := attrValue(tool, string(telemetry.KeyOutcome)); got != telemetry.OutcomeOK {
		t.Errorf("tool span outcome = %q, want %q", got, telemetry.OutcomeOK)
	}
	if got, ok := attrValue(root, "http.route"); !ok || got != "/mcp" {
		t.Errorf("root span http.route = %q (present=%v), want the matched mux pattern", got, ok)
	}
}

func TestObservability_ToolErrorIsDistinctFromTransportError(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := f.client(ctx, "", nil)
	// An unknown collection is the MODEL's problem, handed back as a
	// normal result. The span must say tool_error, not error, for the
	// same reason the metric does: conflating them makes a dashboard read
	// as broken whenever a model mistypes.
	if _, isErr := callText(t, ctx, c, toolSearch, map[string]any{
		"query": "incident", "collection": "nope",
	}); !isErr {
		t.Fatal("expected a tool error for an unknown collection")
	}

	tool, ok := spanNamed(f.flush(), telemetry.SpanMCPTool)
	if !ok {
		t.Fatal("no MCP tool span")
	}
	if got, _ := attrValue(tool, string(telemetry.KeyOutcome)); got != telemetry.OutcomeToolError {
		t.Errorf("tool span outcome = %q, want %q", got, telemetry.OutcomeToolError)
	}
}

// --- acceptance: trace context continuation --------------------------------

func TestObservability_ContinuesAValidInboundTraceContext(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const parentID = "00f067aa0ba902b7"

	resp := f.get(t, ReadinessPath, map[string]string{
		"traceparent": "00-" + traceID + "-" + parentID + "-01",
	})
	_ = resp.Body.Close()

	spans := f.flush()
	root, ok := spanNamed(spans, "GET "+ReadinessPath)
	if !ok {
		t.Fatalf("no root span for the readiness probe; recorded: %v", spanNames(spans))
	}
	if got := root.SpanContext.TraceID().String(); got != traceID {
		t.Fatalf("root span trace id = %s, want the caller's %s", got, traceID)
	}
	if got := root.Parent.SpanID().String(); got != parentID {
		t.Fatalf("root span parent = %s, want the caller's %s", got, parentID)
	}
}

func TestObservability_MalformedTraceContextIsIgnoredSafely(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})

	for _, bad := range []string{
		"garbage",
		"00-not-hex-at-all-01",
		"00-00000000000000000000000000000000-0000000000000000-01",
		strings.Repeat("A", 8192),
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01, 00-bad",
	} {
		f.spans.Reset()
		resp := f.get(t, ReadinessPath, map[string]string{"traceparent": bad})
		status := resp.StatusCode
		_ = resp.Body.Close()

		// The request is served exactly as it would have been. A caller
		// must not be able to change a response — or suppress
		// instrumentation — with a broken header.
		if status != http.StatusOK {
			t.Fatalf("traceparent %q changed the readiness response to %d", bad, status)
		}
		spans := f.flush()
		root, ok := spanNamed(spans, "GET "+ReadinessPath)
		if !ok {
			t.Fatalf("traceparent %q suppressed the root span entirely; recorded: %v", bad, spanNames(spans))
		}
		if !root.SpanContext.TraceID().IsValid() {
			t.Fatalf("traceparent %q produced an invalid trace id", bad)
		}
		if root.Parent.SpanID().IsValid() {
			t.Fatalf("traceparent %q was accepted as a parent; a malformed context must be ignored", bad)
		}
	}
}

// --- acceptance: log correlation --------------------------------------------

func TestObservability_AccessLogCarriesTheTraceItBelongsTo(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	resp := f.get(t, LivenessPath, nil)
	_ = resp.Body.Close()

	spans := f.flush()
	root, ok := spanNamed(spans, "GET "+LivenessPath)
	if !ok {
		t.Fatalf("no root span; recorded: %v", spanNames(spans))
	}

	entry, ok := findLogEntry(f.logs.String(), "mcp.access", "path", LivenessPath)
	if !ok {
		t.Fatalf("no mcp.access log line for %s; logs:\n%s", LivenessPath, f.logs.String())
	}
	if got, _ := entry["trace_id"].(string); got != root.SpanContext.TraceID().String() {
		t.Errorf("access log trace_id = %q, want %s — the log line and the trace of the same "+
			"request have to join, or the correlation is decorative",
			got, root.SpanContext.TraceID())
	}
	if got, _ := entry["span_id"].(string); got == "" {
		t.Error("access log carries no span_id")
	}
}

func TestObservability_AuthDenialLogCarriesTheTraceToo(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{rules: []authz.Rule{{Collections: []string{"runbooks"}}}})
	resp := f.post(t, f.srv.EndpointPath(), nil)
	_ = resp.Body.Close()

	entry, ok := findLogEntry(f.logs.String(), "mcp.auth_denied", "reason", "missing_token")
	if !ok {
		t.Fatalf("no mcp.auth_denied line; logs:\n%s", f.logs.String())
	}
	if got, _ := entry["trace_id"].(string); got == "" {
		t.Error("a 401 must still be a complete, correlated trace — it is the line an operator " +
			"most wants to follow")
	}
}

// --- acceptance: zero configuration is byte-identical -----------------------

// baselineMetricFamilies is every metric family a hosted server could
// expose BEFORE this issue. A server with no observability configuration
// must expose NOTHING outside this set — a deployment that opted out
// gains no series — and one that opted in must lose none of it (see
// TestObservability_MetricsRemainBackwardCompatible).
//
// Prometheus omits a *Vec with no children from a scrape, so which
// subset actually appears depends on the traffic. The assertions below
// are written accordingly: "no name outside this set" for the
// zero-config case, "these specific names still present" for the
// traffic-driven one.
var baselineMetricFamilies = []string{
	"meerkat_auth_failures_total",
	"meerkat_build_info",
	"meerkat_collections_degraded",
	"meerkat_collections_mounted",
	"meerkat_collections_ready",
	"meerkat_http_request_duration_seconds",
	"meerkat_http_requests_total",
	"meerkat_mcp_sessions_active",
	"meerkat_mcp_tool_calls_total",
	"meerkat_mcp_tool_duration_seconds",
	"meerkat_ready",
	// Present only when a collection opted into runtime reconciliation
	// (#28); listed so the assertion does not fail on a fixture that
	// gains one later.
	"meerkat_refresh_attempts_total",
	"meerkat_refresh_changes_total",
	"meerkat_refresh_degraded",
	"meerkat_refresh_duration_seconds",
	"meerkat_refresh_failures_total",
	"meerkat_refresh_last_success_timestamp_seconds",
	"meerkat_refresh_skipped_total",
}

func TestObservability_ZeroConfigurationChangesNothing(t *testing.T) {
	logs := &syncBuffer{}
	reg := prometheus.NewRegistry()
	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: threeCollectionRegistry(t),
		Version:     "test",
		Logger:      slog.New(slog.NewJSONHandler(logs, nil)),
		Metrics:     reg,
		// No Observability, no Telemetry. The shape every deployment that
		// has not opted in gets.
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if srv.tel != nil {
		t.Fatal("a server with no observability configuration must hold NO telemetry — " +
			"the nil is what makes every instrumented call site downstream a no-op")
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	for _, path := range []string{LivenessPath, ReadinessPath, MetricsPath} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	allowed := make(map[string]bool, len(baselineMetricFamilies))
	for _, name := range baselineMetricFamilies {
		allowed[name] = true
	}
	for _, name := range gatheredFamilies(t, reg) {
		if !allowed[name] {
			t.Errorf("an unconfigured server published %s — with no observability: block the "+
				"/metrics surface must be the one it was before tracing existed: no domain "+
				"collectors, no exporter counters, nothing new at all", name)
		}
	}

	// And the access log must be the line it always was: no trace_id, no
	// span_id, nothing new.
	entry, ok := findLogEntry(logs.String(), "mcp.access", "path", LivenessPath)
	if !ok {
		t.Fatalf("no access log line; logs:\n%s", logs.String())
	}
	for _, key := range []string{"trace_id", "span_id"} {
		if _, present := entry[key]; present {
			t.Errorf("an unconfigured server's access log must not carry %q", key)
		}
	}
}

func TestObservability_ZeroConfigurationLeavesTheOTelGlobalsAlone(t *testing.T) {
	before := globalTracerProviderPointer()
	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: threeCollectionRegistry(t),
		Version:     "test",
		Logger:      slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)),
		Metrics:     prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if globalTracerProviderPointer() != before {
		t.Fatal("a server with no observability configuration must not install a process-global " +
			"tracer provider — no SDK is constructed at all, so there is nothing to install")
	}
}

func TestObservability_MetricsRemainBackwardCompatible(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := f.client(ctx, "", nil)
	callText(t, ctx, c, toolSearch, map[string]any{"query": "incident"}) //nolint:errcheck // exercising, not asserting
	resp := f.get(t, ReadinessPath, nil)
	_ = resp.Body.Close()

	got := gatheredFamilies(t, f.reg)
	index := make(map[string]bool, len(got))
	for _, name := range got {
		index[name] = true
	}
	// The families this traffic (one tool call, one readiness probe) would
	// have produced on a pre-#30 server. Every one has to still be there.
	for _, name := range []string{
		"meerkat_build_info",
		"meerkat_collections_degraded",
		"meerkat_collections_mounted",
		"meerkat_collections_ready",
		"meerkat_http_request_duration_seconds",
		"meerkat_http_requests_total",
		"meerkat_mcp_sessions_active",
		"meerkat_mcp_tool_calls_total",
		"meerkat_mcp_tool_duration_seconds",
		"meerkat_ready",
	} {
		if !index[name] {
			t.Errorf("turning observability on removed %s from /metrics — every existing series "+
				"has to survive, or an opt-in costs a deployment its dashboards", name)
		}
	}
	// And the additions are present, so the domain telemetry is actually
	// wired rather than merely declared.
	for _, name := range []string{
		"meerkat_search_total",
		"meerkat_search_duration_seconds",
		"meerkat_search_results",
		"meerkat_index_pages",
		"meerkat_mcp_tool_payload_bytes",
	} {
		if !index[name] {
			t.Errorf("expected domain metric %s to be published", name)
		}
	}
}

// --- acceptance: an exporter outage is not an availability problem ---------

// brokenExporter fails every export, like a collector that is down.
type brokenExporter struct{}

func (brokenExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("collector unreachable")
}
func (brokenExporter) Shutdown(context.Context) error { return nil }

func TestObservability_ExporterOutageAffectsNeitherRequestsNorReadiness(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{exporter: brokenExporter{}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := f.client(ctx, "", nil)
	body, isErr := callText(t, ctx, c, toolSearch, map[string]any{"query": "incident"})
	if isErr {
		t.Fatalf("a dead collector made mk_search fail: %s", body)
	}
	if !strings.Contains(body, "incidents/paging") {
		t.Fatalf("search returned the wrong thing while the collector was down: %s", body)
	}

	resp := f.get(t, ReadinessPath, nil)
	status := resp.StatusCode
	_ = resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("/readyz = %d while the collector was down; telemetry export must never be an "+
			"availability dependency", status)
	}

	// Give the batch timer a chance to fire, then check the failure is
	// visible where it can be seen WITHOUT a working collector.
	_ = f.tel.ForceFlush(ctx)
	if got := counterTotal(t, f.reg, "meerkat_otel_export_failures_total"); got < 1 {
		t.Errorf("meerkat_otel_export_failures_total = %v, want at least 1 — /metrics is the one "+
			"place an operator can look when the collector is the broken thing", got)
	}

	// And shutdown still completes.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.srv.Close()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Close blocked on an unreachable collector")
	}
}

// --- acceptance: sampling ---------------------------------------------------

func TestObservability_SampleRatioZeroExportsNothingButStillServes(t *testing.T) {
	zero := 0.0
	f := newTracedFixture(t, tracedOptions{sampleRatio: &zero})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := f.client(ctx, "", nil)
	body, isErr := callText(t, ctx, c, toolSearch, map[string]any{"query": "incident"})
	if isErr || !strings.Contains(body, "incidents/paging") {
		t.Fatalf("sampling must not change what a search returns: %s", body)
	}
	if spans := f.flush(); len(spans) != 0 {
		t.Fatalf("sample_ratio 0 exported %d spans, want 0: %v", len(spans), spanNames(spans))
	}
}

// --- acceptance: concurrency -------------------------------------------------

func TestObservability_ConcurrentSessionsProduceIndependentTraces(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{
		rules: []authz.Rule{
			{Name: "a", Groups: []string{"team-a"}, Collections: []string{"runbooks"}},
			{Name: "b", Groups: []string{"team-b"}, Collections: []string{"architecture"}},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const sessions = 12
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			group := "team-a"
			if i%2 == 1 {
				group = "team-b"
			}
			c := f.client(ctx, f.token(fmt.Sprintf("user-%d", i), group), nil)
			if _, isErr := callText(t, ctx, c, toolSearch, map[string]any{"query": "overview"}); isErr {
				t.Errorf("session %d: mk_search returned a tool error", i)
			}
		}(i)
	}
	wg.Wait()

	spans := f.flush()
	traces := make(map[string]int)
	toolSpans := 0
	for _, s := range spans {
		traces[s.SpanContext.TraceID().String()]++
		if s.Name == telemetry.SpanMCPTool {
			toolSpans++
		}
	}
	if toolSpans < sessions {
		t.Fatalf("recorded %d tool spans for %d concurrent sessions", toolSpans, sessions)
	}
	if len(traces) < sessions {
		t.Fatalf("recorded %d distinct traces for %d concurrent sessions — a shared trace ID "+
			"would mean the request context is leaking between sessions", len(traces), sessions)
	}
	// Every span of a trace shares that trace's ID by construction; what
	// this asserts is that no span was orphaned into the invalid trace.
	for id := range traces {
		if id == "00000000000000000000000000000000" {
			t.Fatal("a span was recorded outside any trace")
		}
	}
}

// --- acceptance: attribute hygiene ------------------------------------------

func TestObservability_NoSpanOrMetricCarriesAForbiddenValue(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{
		rules: []authz.Rule{{Name: "all", Groups: []string{"team-a"}, Collections: []string{"*"}}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const (
		subject   = "alice-the-subject"
		queryText = "confidential-compensation-query"
	)
	token := f.token(subject, "team-a")
	c := f.client(ctx, token, nil)

	// Drive every read surface, plus an ambiguity, plus a not-found, plus
	// an unauthenticated scan of a path that does not exist.
	callText(t, ctx, c, toolSearch, map[string]any{"query": queryText})             //nolint:errcheck // exercising
	callText(t, ctx, c, toolShow, map[string]any{"id": "shared/overview"})          //nolint:errcheck // ambiguous on purpose
	callText(t, ctx, c, toolShow, map[string]any{"id": "payroll/salaries"})         //nolint:errcheck // exercising
	callText(t, ctx, c, toolList, map[string]any{"prefix": "payroll/"})             //nolint:errcheck // exercising
	callText(t, ctx, c, toolListCollections, map[string]any{})                      //nolint:errcheck // exercising
	callText(t, ctx, c, toolShow, map[string]any{"id": "secrets:payroll/salaries"}) //nolint:errcheck // qualified
	resp := f.get(t, "/wp-admin/setup-config.php?scan=1", map[string]string{"User-Agent": "sqlmap/1.0"})
	_ = resp.Body.Close()
	resp = f.get(t, ReadinessPath, nil)
	_ = resp.Body.Close()

	// The classes of value that must never leave the process on a span or
	// a metric label. Several of these are things the ACCESS LOG does
	// carry — that is the point: the log stays on the operator's stderr
	// and a span goes to a collector, so the two are held to different
	// rules.
	forbidden := map[string]string{
		queryText:                       "query text",
		token:                           "the bearer token",
		subject:                         "the caller's OIDC subject",
		"alice-the-subject@example.com": "the caller's email",
		"team-a":                        "the caller's group membership",
		"acme":                          "the caller's tenant",
		"payroll/salaries":              "a page ID",
		"shared/overview":               "a page ID",
		"incidents/paging":              "a page ID",
		"runbooks":                      "a collection name",
		"architecture":                  "a collection name",
		"secrets":                       "a collection name",
		"confidential compensation":     "page content",
		"wp-admin":                      "a caller-supplied request path",
		"sqlmap":                        "a caller-supplied user agent",
	}

	for _, s := range f.flush() {
		haystack := []string{s.Name}
		for _, a := range s.Attributes {
			haystack = append(haystack, string(a.Key), a.Value.String())
		}
		for _, e := range s.Events {
			haystack = append(haystack, e.Name)
			for _, a := range e.Attributes {
				haystack = append(haystack, string(a.Key), a.Value.String())
			}
		}
		haystack = append(haystack, s.Status.Description)
		for _, r := range s.Resource.Attributes() {
			haystack = append(haystack, string(r.Key), r.Value.String())
		}
		for _, text := range haystack {
			for needle, what := range forbidden {
				if strings.Contains(strings.ToLower(text), strings.ToLower(needle)) {
					t.Errorf("span %q carries %s (%q) in %q — spans are exported OUT of the "+
						"process and may carry only counts, durations, booleans and closed-set "+
						"outcomes", s.Name, what, needle, text)
				}
			}
		}
	}

	families, err := f.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range families {
		for _, m := range fam.GetMetric() {
			for _, label := range m.GetLabel() {
				for needle, what := range forbidden {
					if strings.Contains(strings.ToLower(label.GetValue()), strings.ToLower(needle)) {
						t.Errorf("metric %s label %s=%q carries %s — /metrics is unauthenticated, "+
							"so a label is as public as the endpoint",
							fam.GetName(), label.GetName(), label.GetValue(), what)
					}
				}
			}
		}
	}
}

// --- memory outcomes ---------------------------------------------------------

func TestObservability_MemorySaveRecordsScopeAndOutcomeAndNothingElse(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{
		withMemory: true,
		rules: []authz.Rule{{
			Name: "writers", Groups: []string{"team-a"}, Collections: []string{"notes"},
			Capabilities: []string{string(authz.CapRead), string(authz.CapPersonalWrite)},
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const secretBody = "the-memory-body-nobody-should-export"
	c := f.client(ctx, f.token("bob", "team-a"), nil)
	if _, text, isErr := callSaveMemory(t, ctx, c, map[string]any{
		"scope": "personal", "title": "A Convention", "content": secretBody, "key": "conventions",
	}); isErr {
		t.Fatalf("mk_save_memory: %s", text)
	}

	trace := traceContaining(t, f.flush(), telemetry.SpanMemorySave)
	save, _ := spanNamed(trace, telemetry.SpanMemorySave)
	if got, _ := attrValue(save, string(telemetry.KeyMemoryScope)); got != "personal" {
		t.Errorf("memory scope attribute = %q, want personal", got)
	}
	if got, _ := attrValue(save, string(telemetry.KeyOutcome)); got != telemetry.OutcomeSaved {
		t.Errorf("memory outcome = %q, want %q", got, telemetry.OutcomeSaved)
	}
	if _, ok := attrValue(save, string(telemetry.KeyMemoryBytes)); !ok {
		t.Error("the save span should record the document's SIZE — it is the most a collector may learn about it")
	}

	// The backend span exists and names the backend by type, never by
	// location.
	store, ok := spanNamed(trace, telemetry.SpanMemoryStore)
	if !ok {
		t.Fatalf("no memory store span; trace holds: %v", spanNames(trace))
	}
	if got, _ := attrValue(store, string(telemetry.KeyMemoryBackend)); got != telemetry.BackendLocal {
		t.Errorf("memory backend attribute = %q, want %q", got, telemetry.BackendLocal)
	}
	if got, _ := attrValue(store, string(telemetry.KeyMemoryOperation)); got != telemetry.MemorySave {
		t.Errorf("memory operation = %q, want %q", got, telemetry.MemorySave)
	}

	for _, s := range trace {
		for _, a := range s.Attributes {
			for _, needle := range []string{secretBody, "conventions", "notes", "bob", "A Convention"} {
				if strings.Contains(a.Value.String(), needle) {
					t.Errorf("span %q attribute %s=%q carries %q — a memory's body, key, title, "+
						"collection and owner all stay in the process",
						s.Name, a.Key, a.Value.String(), needle)
				}
			}
		}
	}

	if got := counterTotal(t, f.reg, "meerkat_memory_saves_total"); got != 1 {
		t.Errorf("meerkat_memory_saves_total = %v, want 1", got)
	}
}

func TestObservability_UnmatchedPathsCollapseToOneSpanName(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	for _, path := range []string{"/wp-admin", "/.env", "/actuator/health", "/phpmyadmin"} {
		resp := f.get(t, path, nil)
		_ = resp.Body.Close()
	}
	names := map[string]int{}
	for _, s := range f.flush() {
		if s.SpanContext.IsValid() && strings.HasPrefix(s.Name, "GET ") {
			names[s.Name]++
		}
	}
	if got := names["GET other"]; got != 4 {
		t.Fatalf("scanned paths produced span names %v; every unmatched request must collapse to "+
			"one bounded name, or a scanner writes into somebody's trace backend", names)
	}
}

// --- helpers ------------------------------------------------------------------

func (f *tracedFixture) get(t *testing.T, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.http.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *tracedFixture) post(t *testing.T, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.http.URL+path,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// findLogEntry returns the first JSON log line whose "msg" is msg and
// whose key equals want.
func findLogEntry(logs, msg, key, want string) (map[string]any, bool) {
	for _, line := range strings.Split(logs, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["msg"] != msg {
			continue
		}
		if got, _ := entry[key].(string); got == want {
			return entry, true
		}
	}
	return nil, false
}

func gatheredFamilies(t *testing.T, reg *prometheus.Registry) []string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make([]string, 0, len(families))
	for _, f := range families {
		out = append(out, f.GetName())
	}
	sort.Strings(out)
	return out
}

func counterTotal(t *testing.T, reg *prometheus.Registry, name string) float64 {
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
