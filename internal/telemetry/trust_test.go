package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestResolve_RefusesCredentialsOverPlaintextToANonLoopbackEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		insecure bool
		headers  string
		wantErr  bool
	}{
		{"remote host plaintext with headers", "collector.example:4317", true, "authorization=Bearer x", true},
		{"remote url plaintext with headers", "http://10.1.2.3:4318", true, "authorization=Bearer x", true},
		{"remote host TLS with headers", "collector.example:4317", false, "authorization=Bearer x", false},
		{"remote host plaintext no headers", "collector.example:4317", true, "", false},
		{"localhost plaintext with headers", "localhost:4317", true, "authorization=Bearer x", false},
		{"127.0.0.1 plaintext with headers", "127.0.0.1:4317", true, "authorization=Bearer x", false},
		{"ipv6 loopback url with headers", "http://[::1]:4318", true, "authorization=Bearer x", false},
		{"hostname that merely starts with localhost", "localhost.evil.example:4317", true, "authorization=Bearer x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearOTelEnv(t)
			t.Setenv("MY_OTLP_HEADERS", c.headers)
			_, err := Resolve(&Config{
				Traces: TraceConfig{Enabled: true},
				OTLP:   OTLPConfig{Endpoint: c.endpoint, Insecure: c.insecure, HeadersEnv: "MY_OTLP_HEADERS"},
			})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "headers_env") {
				t.Errorf("error should name headers_env: %v", err)
			}
		})
	}
}

func TestResolve_StandardHeadersEnvAlsoCannotGoPlaintext(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envHeaders, "authorization=Bearer x")
	t.Setenv(envEndpoint, "http://collector.example:4318")
	t.Setenv(envInsecure, "true")
	if _, err := Resolve(&Config{Traces: TraceConfig{Enabled: true}}); err == nil {
		t.Fatal("OTEL_EXPORTER_OTLP_HEADERS over insecure plaintext must be refused")
	}
}

func TestResolve_FileEndpointBeatsEveryEnvEndpoint(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envTracesEndpoint, "env-traces.example:4317")
	t.Setenv(envMetricsEndpoint, "env-metrics.example:4317")
	t.Setenv(envEndpoint, "env.example:4317")
	r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}, OTLP: OTLPConfig{Endpoint: "file.example:4317"}})
	if got := r.tracesTarget(); got != "file.example:4317" {
		t.Errorf("traces target = %q, want the file's", got)
	}
	if got := r.metricsTarget(); got != "file.example:4317" {
		t.Errorf("metrics target = %q, want the file's", got)
	}
}

func TestResolve_SignalEndpointEnvStillAppliesWhenFileIsSilent(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envTracesEndpoint, "env-traces.example:4317")
	r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}})
	if got := r.tracesTarget(); got != "env-traces.example:4317" {
		t.Errorf("traces target = %q", got)
	}
}

func TestResolve_TrustedSources(t *testing.T) {
	clearOTelEnv(t)
	r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true, TrustedSources: []string{"10.0.0.0/8", "192.0.2.7", "::1"}}})
	if len(r.TrustedSources) != 3 {
		t.Fatalf("parsed %d sources, want 3", len(r.TrustedSources))
	}
	for _, bad := range []string{"not-an-ip", "10.0.0.0/33", "", "example.com"} {
		if _, err := Resolve(&Config{Traces: TraceConfig{Enabled: true, TrustedSources: []string{bad}}}); err == nil {
			t.Errorf("trusted source %q accepted", bad)
		}
	}
	if len(resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}}).TrustedSources) != 0 {
		t.Error("default must trust nobody")
	}
}

func TestResolve_MaxSpansPerSecondDefaultsAndCanBeLifted(t *testing.T) {
	clearOTelEnv(t)
	if got := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}}).MaxSpansPerSecond; got != defaultMaxSpansPerSecond {
		t.Errorf("default = %d", got)
	}
	if got := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true, MaxSpansPerSecond: 5}}).MaxSpansPerSecond; got != 5 {
		t.Errorf("explicit = %d", got)
	}
	if got := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true, MaxSpansPerSecond: -1}}).MaxSpansPerSecond; got >= 0 {
		t.Errorf("negative should stay negative (uncapped), got %d", got)
	}
}

func newTraceTel(t *testing.T, cfg TraceConfig, exp *tracetest.InMemoryExporter) *Telemetry {
	t.Helper()
	clearOTelEnv(t)
	cfg.Enabled = true
	tel, err := New(context.Background(), Options{Config: &Config{Traces: cfg}, SpanExporter: exp})
	if err != nil || tel == nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

func TestExtractRequestHonoursOnlyTrustedPeers(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	tel := newTraceTel(t, TraceConfig{TrustedSources: []string{"10.0.0.0/8", "::1"}}, tracetest.NewInMemoryExporter())
	for remote, want := range map[string]bool{
		"10.1.2.3:5555":        true,
		"[::1]:5555":           true,
		"[::ffff:10.1.2.3]:80": true,
		"192.0.2.1:5555":       false,
		"garbage":              false,
		"":                     false,
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("traceparent", tp)
		// X-Forwarded-For must never matter.
		r.Header.Set("X-Forwarded-For", "10.9.9.9")
		got := trace.SpanContextFromContext(tel.ExtractRequest(context.Background(), r)).IsValid()
		if got != want {
			t.Errorf("remote %q: continued=%v, want %v", remote, got, want)
		}
	}
	none := newTraceTel(t, TraceConfig{}, tracetest.NewInMemoryExporter())
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("traceparent", tp)
	if trace.SpanContextFromContext(none.ExtractRequest(context.Background(), r)).IsValid() {
		t.Error("with no trusted_sources nothing may be continued")
	}
	var nilTel *Telemetry
	if ctx := nilTel.ExtractRequest(context.Background(), r); ctx == nil {
		t.Error("nil telemetry must return ctx")
	}
}

func TestTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newTokenBucket(4, func() time.Time { return now })
	for i := 0; i < 4; i++ {
		if !b.allow() {
			t.Fatalf("burst token %d refused", i)
		}
	}
	if b.allow() {
		t.Fatal("bucket should be empty")
	}
	now = now.Add(250 * time.Millisecond)
	if !b.allow() || b.allow() {
		t.Fatal("exactly one token should refill per 1/4 s")
	}
	now = now.Add(time.Hour)
	for i := 0; i < 4; i++ {
		if !b.allow() {
			t.Fatal("refill must cap at the burst")
		}
	}
	if b.allow() {
		t.Fatal("refill exceeded the burst")
	}
}

func TestSpanRateCapDropsExcessBeforeTheQueueAndCountsIt(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	clearOTelEnv(t)
	reg := prometheus.NewRegistry()
	tel, err := New(context.Background(), Options{
		Config:       &Config{Traces: TraceConfig{Enabled: true, MaxSpansPerSecond: 5}},
		Registry:     reg,
		SpanExporter: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	for i := 0; i < 50; i++ {
		_, s := tel.Start(context.Background(), "meerkat.test")
		s.End()
	}
	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(exp.GetSpans()); n < 5 || n > 8 {
		t.Fatalf("exported %d spans with a 5/s cap, want about 5", n)
	}
	if d := counterValue(t, reg, "meerkat_otel_spans_dropped_total"); d < 40 {
		t.Fatalf("dropped counter = %v, want >= 40", d)
	}
}

func TestSpanRateCapCanBeLifted(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tel := newTraceTel(t, TraceConfig{MaxSpansPerSecond: -1}, exp)
	for i := 0; i < 1500; i++ {
		_, s := tel.Start(context.Background(), "meerkat.test")
		s.End()
	}
	_ = tel.ForceFlush(context.Background())
	if n := len(exp.GetSpans()); n != 1500 {
		t.Fatalf("exported %d, want all 1500 with the cap lifted", n)
	}
}

func TestErrorHandlerIsInstalledGloballyOnlyOnce(t *testing.T) {
	clearOTelEnv(t)
	reg1, reg2 := prometheus.NewRegistry(), prometheus.NewRegistry()
	a, err := New(context.Background(), Options{Config: &Config{Traces: TraceConfig{Enabled: true}}, Registry: reg1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(context.Background(), Options{Config: &Config{Traces: TraceConfig{Enabled: true}}, Registry: reg2})
	if err != nil {
		t.Fatal(err)
	}
	if n := errHandlerInstalls.Load(); n != 1 {
		t.Fatalf("otel.SetErrorHandler called %d times, want exactly 1", n)
	}
	otel.Handle(errors.New("boom"))
	if counterValue(t, reg2, "meerkat_otel_export_failures_total") != 1 {
		t.Error("the latest instance should receive SDK errors")
	}
	_ = b.Shutdown(context.Background())
	otel.Handle(errors.New("after shutdown"))
	if counterValue(t, reg2, "meerkat_otel_export_failures_total") != 1 {
		t.Error("a shut-down instance must stop receiving SDK errors")
	}
	_ = a.Shutdown(context.Background())
}

func TestDeprecatedLogSettingsWarn(t *testing.T) {
	clearOTelEnv(t)
	var buf syncBuffer
	tel, err := New(context.Background(), Options{
		Config: &Config{Traces: TraceConfig{Enabled: true}, Logs: LogConfig{Level: "debug"}},
		Logger: testLogger(&buf),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	if !strings.Contains(buf.String(), "have no effect") {
		t.Fatalf("no deprecation warning: %s", buf.String())
	}
}
