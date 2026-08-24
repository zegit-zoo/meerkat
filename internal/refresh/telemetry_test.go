package refresh

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// telemetry_test.go covers the cycle span runOnce emits, and the one
// place this package's SPAN rules deliberately differ from its LABEL
// rules: a span may carry the source version, and a metric label may
// not.

// tracedController builds a controller over targets, with tracing on and
// an in-memory exporter behind it. No collector, no network.
func tracedController(t *testing.T, targets ...Target) (*Controller, *tracetest.InMemoryExporter, context.Context) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tel, err := telemetry.New(context.Background(), telemetry.Options{
		Config: &telemetry.Config{
			Traces: telemetry.TraceConfig{Enabled: true},
			Limits: telemetry.ExportLimits{BatchTimeout: telemetry.Duration(20 * time.Millisecond)},
		},
		SpanExporter: exp,
	})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	c := New(Options{Targets: targets})
	if c == nil {
		t.Fatal("New returned nil for a non-empty target set")
	}
	return c, exp, telemetry.NewContext(context.Background(), tel)
}

// flushSpans forces an export and returns what was recorded.
func flushSpans(t *testing.T, ctx context.Context, exp *tracetest.InMemoryExporter) tracetest.SpanStubs {
	t.Helper()
	tel := telemetry.FromContext(ctx)
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.ForceFlush(flushCtx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	return exp.GetSpans()
}

func cycleSpan(t *testing.T, spans tracetest.SpanStubs) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == telemetry.SpanRefreshCycle {
			return s
		}
	}
	t.Fatalf("no %s span was recorded", telemetry.SpanRefreshCycle)
	return tracetest.SpanStub{}
}

func spanAttr(s tracetest.SpanStub, key string) (string, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.String(), true
		}
	}
	return "", false
}

func TestCycleSpan_CarriesTheOrdinalAndKindButNeverTheName(t *testing.T) {
	target := newFakeTarget(3, KindContent)
	target.setResult(Outcome{Changed: true, Version: "1712345678901234"}, nil)
	c, exp, ctx := tracedController(t, target)

	if err := c.ReloadNow(ctx); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	span := cycleSpan(t, flushSpans(t, ctx, exp))

	if got, ok := spanAttr(span, string(telemetry.KeyCollectionOrdinal)); !ok || got != "3" {
		t.Errorf("ordinal attribute = %q (present=%v), want 3", got, ok)
	}
	if got, _ := spanAttr(span, string(telemetry.KeyRefreshKind)); got != KindContent {
		t.Errorf("kind attribute = %q, want %q", got, KindContent)
	}
	// The collection's NAME is the log's business, not the collector's.
	for _, a := range span.Attributes {
		if strings.Contains(a.Value.String(), target.key.Name) {
			t.Errorf("cycle span attribute %s=%s carries the collection name; the ordinal is the "+
				"identifier that leaves the process, exactly as it is for the metric labels",
				a.Key, a.Value.String())
		}
	}
}

func TestCycleSpan_CarriesTheVersionThatAMetricLabelMayNot(t *testing.T) {
	const version = "1712345678901234"
	target := newFakeTarget(0, KindContent)
	target.setResult(Outcome{Changed: true, Version: version}, nil)
	c, exp, ctx := tracedController(t, target)

	if err := c.ReloadNow(ctx); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	span := cycleSpan(t, flushSpans(t, ctx, exp))

	// This is the one deliberate asymmetry between the span and the
	// metric. A generation increments forever, so a Prometheus label
	// carrying it would mint a permanent time series per publication; a
	// span is one event, so the same string costs one record and answers
	// the question an operator actually has.
	if got, ok := spanAttr(span, string(telemetry.KeyRefreshVersion)); !ok || got != version {
		t.Fatalf("version attribute = %q (present=%v), want %q", got, ok, version)
	}
	if got, _ := spanAttr(span, string(telemetry.KeyRefreshChanged)); got != "true" {
		t.Errorf("changed attribute = %q, want true", got)
	}
	if got, _ := spanAttr(span, string(telemetry.KeyOutcome)); got != telemetry.OutcomeOK {
		t.Errorf("outcome = %q, want %q", got, telemetry.OutcomeOK)
	}
}

func TestCycleSpan_ClassifiesTheOutcomes(t *testing.T) {
	cases := []struct {
		name string
		out  Outcome
		err  error
		want string
	}{
		{"unchanged", Outcome{Changed: false, Version: "7"}, nil, telemetry.OutcomeUnchanged},
		{"busy", Outcome{}, ErrBusy, telemetry.OutcomeBusy},
		{"cancelled", Outcome{}, context.Canceled, telemetry.OutcomeCancelled},
		{"failed", Outcome{}, errors.New(`collection "notes": probe my-private-bucket: 503`), telemetry.OutcomeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := newFakeTarget(1, KindMemory)
			target.setResult(tc.out, tc.err)
			c, exp, ctx := tracedController(t, target)
			_ = c.ReloadNow(ctx)

			span := cycleSpan(t, flushSpans(t, ctx, exp))
			if got, _ := spanAttr(span, string(telemetry.KeyOutcome)); got != tc.want {
				t.Errorf("outcome = %q, want %q", got, tc.want)
			}
			// A failure's message names the collection and the bucket. It
			// goes to the log; the span gets a word from a closed set.
			for _, ev := range span.Events {
				t.Errorf("the cycle span recorded event %q — a reconciliation error's text quotes "+
					"the collection and the bucket, so it must be classified rather than recorded", ev.Name)
			}
			for _, a := range span.Attributes {
				if strings.Contains(a.Value.String(), "my-private-bucket") {
					t.Errorf("attribute %s leaked the bucket name", a.Key)
				}
			}
		})
	}
}

func TestCycleSpan_IsAbsentWithoutTelemetry(t *testing.T) {
	// The zero-configuration path: the very same runOnce, with a plain
	// context, must create nothing at all.
	target := newFakeTarget(0, KindContent)
	target.setResult(Outcome{Changed: true, Version: "1"}, nil)
	c := New(Options{Targets: []Target{target}})
	if c == nil {
		t.Fatal("New returned nil")
	}
	if err := c.ReloadNow(context.Background()); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	if target.callCount() != 1 {
		t.Fatalf("the cycle did not run: %d calls", target.callCount())
	}
	// Nothing to assert about spans, because there is no exporter and no
	// provider — which is the assertion. The test earns its place by
	// pinning that the instrumented path still reconciles when telemetry
	// is absent, which is the failure mode a nil-unsafe span helper would
	// produce.
}

// interfaceCheck keeps the in-memory exporter honest about the SDK
// interface this package's tests rely on.
var _ sdktrace.SpanExporter = (*tracetest.InMemoryExporter)(nil)
