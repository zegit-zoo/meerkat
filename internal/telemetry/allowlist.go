package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// allowlist.go makes the span disclosure rule structural.
//
// The rule (see the package comment) is stated for meerkat's own call
// sites, and every one of them is written to obey it. That is a
// convention, and a convention cannot cover code meerkat does not own:
// when the tracer provider is installed process-globally, every
// third-party client instrumented against the globals — the object-store
// SDKs' HTTP transports above all — emits spans into the same pipeline,
// carrying whatever its authors thought useful (full request URLs, host
// names, error text). This processor sits in front of the exporter queue
// and removes everything that is not positively known to be safe:
//
//   - an attribute survives only when its key is in the meerkat.*
//     namespace or in allowedSemconvKeys;
//   - a span from any other instrumentation scope is renamed to a fixed
//     "external.<kind>" and loses its events, links and status text;
//   - a meerkat span keeps its events only when they are the classified
//     exception marker or meerkat.*-named, with attributes filtered the
//     same way.
//
// The filter runs at span END on a read-only copy, so attributes added at
// any point in the span's life are covered.

// allowedSemconvKeys is the complete set of non-meerkat attribute keys a
// span may export. Every one is a bounded, server-owned value: the
// request method, the matched route pattern, the response status, the
// configured IdP host and scheme, and the classified exception type.
var allowedSemconvKeys = map[attribute.Key]struct{}{
	"http.request.method":       {},
	"http.route":                {},
	"http.response.status_code": {},
	"server.address":            {},
	"url.scheme":                {},
	"exception.type":            {},
}

// attrAllowed reports whether a span attribute key may be exported.
func attrAllowed(k attribute.Key) bool {
	if strings.HasPrefix(string(k), "meerkat.") {
		return true
	}
	_, ok := allowedSemconvKeys[k]
	return ok
}

func filterAttrs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if attrAllowed(kv.Key) {
			out = append(out, kv)
		}
	}
	return out
}

// allowlistProcessor wraps the exporting processor and hands it a
// sanitized view of every ended span.
type allowlistProcessor struct {
	next sdktrace.SpanProcessor
}

func newAllowlistProcessor(next sdktrace.SpanProcessor) *allowlistProcessor {
	return &allowlistProcessor{next: next}
}

func (p *allowlistProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.next.OnStart(ctx, s)
}

func (p *allowlistProcessor) OnEnd(s sdktrace.ReadOnlySpan) { p.next.OnEnd(sanitize(s)) }

func (p *allowlistProcessor) ForceFlush(ctx context.Context) error { return p.next.ForceFlush(ctx) }

func (p *allowlistProcessor) Shutdown(ctx context.Context) error { return p.next.Shutdown(ctx) }

// sanitizedSpan overrides exactly the fields that can carry text.
type sanitizedSpan struct {
	sdktrace.ReadOnlySpan
	name   string
	attrs  []attribute.KeyValue
	events []sdktrace.Event
	links  []sdktrace.Link
	status sdktrace.Status
}

func (s sanitizedSpan) Name() string                     { return s.name }
func (s sanitizedSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s sanitizedSpan) Events() []sdktrace.Event         { return s.events }
func (s sanitizedSpan) Links() []sdktrace.Link           { return s.links }
func (s sanitizedSpan) Status() sdktrace.Status          { return s.status }

func sanitize(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	own := s.InstrumentationScope().Name == instrumentationScope
	out := sanitizedSpan{
		ReadOnlySpan: s,
		name:         s.Name(),
		attrs:        filterAttrs(s.Attributes()),
		status:       s.Status(),
	}
	for _, l := range s.Links() {
		l.Attributes = filterAttrs(l.Attributes)
		out.links = append(out.links, l)
	}
	if own {
		for _, ev := range s.Events() {
			if ev.Name != "exception" && !strings.HasPrefix(ev.Name, "meerkat.") {
				continue
			}
			ev.Attributes = filterAttrs(ev.Attributes)
			out.events = append(out.events, ev)
		}
		return out
	}
	// A third-party span: its name may embed a URL, bucket or object, and
	// its status description an error string. Keep the shape (kind,
	// timing, status code, allowlisted attributes) and nothing else.
	out.name = externalName(s.SpanKind())
	out.status.Description = ""
	return out
}

// externalName is the fixed name a third-party span exports under.
func externalName(k trace.SpanKind) string {
	return "external." + strings.ToLower(k.String())
}
