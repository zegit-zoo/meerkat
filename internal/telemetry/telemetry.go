// Package telemetry is meerkat's OpenTelemetry layer: distributed
// traces, OTLP export, and the bounded domain metrics that sit beside
// the hosted server's existing Prometheus collectors.
//
// # It is off unless somebody turned it on
//
// With no `observability:` block and no OTEL_* environment variable,
// New returns (nil, nil) and NOTHING here runs. No SDK is constructed,
// no exporter is built, no goroutine is started, no socket is opened,
// and the global OpenTelemetry providers are left exactly as the Go
// runtime found them. The hosted server then never puts a *Telemetry
// into a request context, so every Span call below the request path
// takes the nil branch and returns the caller's own context unchanged.
//
// That is the zero-config invariance the design promises, and it is a
// property of the plumbing rather than of a flag check at each call
// site: a nil *Telemetry is a working *Telemetry that does nothing.
//
// # How instrumentation reaches the leaves
//
// Through the CONTEXT, exactly as authorization does (authz.FromContext).
// The hosted server puts one *Telemetry into the request context; every
// package below — collections, search, memory, contentsource, refresh —
// calls telemetry.Span(ctx, ...) and gets either a real span or nothing
// at all, with no new parameter on any signature and no package-level
// mutable state for a test to race on.
//
// The alternative — a package-level default, or otel's own global —
// would have made two hosted servers in one test binary share one
// tracer and one set of domain counters. The context carries the right
// one by construction, which is the same reason a per-request registry
// view carries the caller's grants.
//
// # What a span may carry
//
// The same non-disclosure rules the Prometheus label discipline states
// (internal/mcp/metrics.go, internal/refresh/metrics.go), plus one
// more. Spans are EXPORTED OUT OF THE PROCESS to a collector, so they
// are held to a stricter standard than the access log, which stays on
// the operator's own stderr:
//
//	never in a span:  query text, page IDs, page/memory content, tags,
//	                  memory keys, collection names, bucket or object
//	                  names, bearer tokens, OAuth claims, subjects,
//	                  emails, groups, tenants, session IDs, request
//	                  paths taken from the caller.
//	fine in a span:   counts, durations, booleans, outcomes from a
//	                  closed set, the matched route pattern, a
//	                  collection's configuration ORDINAL, and — because
//	                  a span is not a time series — a source generation
//	                  or fingerprint.
//
// The access log deliberately does carry sub/issuer/tenant, for audit.
// Traces deliberately do not: correlating a trace to a principal is a
// separate decision with a separate privacy footprint, and it is not
// made here. See docs/design/observability.md.
package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// instrumentationScope names meerkat as the instrumentation library in
// every emitted span. One scope, not one per package: a consumer
// filtering by scope wants "spans meerkat produced", and the package is
// already legible from the span name.
const instrumentationScope = "github.com/zegit-zoo/meerkat"

// nonRecording is the span every disabled call site returns. noop.Span
// is an empty struct, so handing one back costs nothing and callers can
// use it — SetAttributes, RecordError, End — without a nil check.
var nonRecording trace.Span = noop.Span{}

// ctxKey is the private context key *Telemetry travels under.
type ctxKey struct{}

// NewContext returns ctx carrying t. A nil t is stored as nothing at
// all, so FromContext's fast path stays a single type assertion and a
// disabled deployment never even allocates the derived context.
func NewContext(ctx context.Context, t *Telemetry) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, t)
}

// FromContext returns the telemetry ctx carries, or nil. Every method
// on the result tolerates nil.
func FromContext(ctx context.Context) *Telemetry {
	t, _ := ctx.Value(ctxKey{}).(*Telemetry)
	return t
}

// Span starts a child span of whatever is in ctx, and is THE entry
// point for instrumented code below the request path.
//
// With no telemetry in ctx it returns ctx unchanged and a non-recording
// span: no allocation, no derived context, no measurable cost. That is
// what makes it safe to call from paths the CLI also walks.
func Span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	t := FromContext(ctx)
	if t == nil {
		return ctx, nonRecording
	}
	return t.Start(ctx, name, trace.WithAttributes(attrs...))
}

// Record returns the domain metric recorder ctx carries. Nil-safe in
// both directions: no telemetry means a nil *Metrics, and every method
// on *Metrics tolerates nil.
func Record(ctx context.Context) *Metrics {
	return FromContext(ctx).Metrics()
}

// End finishes span, marking it an error when err is non-nil.
//
// It is the one-liner every instrumented function defers, and it
// deliberately records only err.Error() — which is meerkat's own
// message, not caller input — through OpenTelemetry's exception
// recording. Where an error could embed caller-supplied text (a query
// string, a page ID), the call site passes a classified outcome instead
// and does not hand the error here; see internal/search's
// searchOutcome.
func End(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, "")
		span.RecordError(err)
	}
	span.End()
}

// Fail marks span failed with a bounded reason and ends it, WITHOUT
// recording the error object.
//
// This is the form used anywhere the error text could contain caller
// input. reason must come from a closed set.
func Fail(span trace.Span, reason string) {
	span.SetStatus(codes.Error, reason)
	span.SetAttributes(Outcome(reason))
	span.End()
}
