package mcp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// telemetry.go is the hosted transport's tracing seam: the root server
// span, and the small amount of shaping that keeps a span's attributes
// inside the disclosure rules.
//
// # Where the span is created, and why there
//
// OUTERMOST, above the access log. The alternative — creating it below,
// beside the Prometheus instrumentation — would have left the access log
// with no span to read a trace ID from, and the identityHolder trick
// (see hosted.go) exists precisely because carrying a value back UP a
// middleware stack is awkward. Putting the span at the top means every
// frame below it, the access log included, has it in the request context
// for free, and a 401 refused by the gate is still a complete trace.
//
// # What the root span may say
//
// The MATCHED ROUTE, never r.URL.Path. It is the same rule the `route`
// metric label follows and for the same reason, one notch stricter: a
// span is exported to a collector, so a scanner probing /wp-admin must
// not be able to write into somebody's trace backend either. An
// unmatched request collapses to the one bounded value "other".
//
// Not present, deliberately: the Authorization header, the bearer token,
// any claim, the caller's subject, the MCP session ID, the query string,
// the User-Agent (unbounded caller-controlled text) and the Host header.
// The access log still carries the identity for audit — see the package
// comment in internal/telemetry for why the two surfaces differ.

// traceHTTP wraps next in a root server span.
//
// A nil *Telemetry returns next UNCHANGED — not a pass-through wrapper,
// the handler itself. That is what makes the zero-configuration path
// byte-identical: with no observability configured there is not even an
// extra stack frame between the mux and the access log.
func (s *HostedServer) traceHTTP(next http.Handler) http.Handler {
	if !s.tel.Tracing() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := s.routeLabel(r)
		// Continue the caller's trace when they sent a well-formed
		// traceparent, start a new one when they did not. A malformed
		// header yields an invalid parent span context, which is the same
		// thing as none — the request is served identically either way,
		// which is the point.
		ctx := s.tel.Extract(r.Context(), r.Header)
		ctx, span := s.tel.Start(ctx, spanName(r.Method, route),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(r.Method),
				semconv.HTTPRoute(route),
			))
		defer span.End()

		// The recorder is created HERE, above the access log, so the span
		// can report the status the access log also reports. Everything
		// below reuses it (see accessLog and instrumentHTTP), so there is
		// still exactly one recorder per request.
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		}
		r = r.WithContext(telemetry.NewContext(ctx, s.tel))
		next.ServeHTTP(rec, r)
		span.SetAttributes(semconv.HTTPResponseStatusCode(rec.status))
		if rec.status >= http.StatusInternalServerError {
			span.SetAttributes(telemetry.Outcome(telemetry.OutcomeError))
		}
	})
}

// routeLabel resolves a request to the server's own bounded route
// pattern, reusing the mux resolution the metrics already do. It is
// never r.URL.Path.
func (s *HostedServer) routeLabel(r *http.Request) string {
	if s.metrics == nil || s.metrics.routeOf == nil {
		return "other"
	}
	return s.metrics.routeOf(r)
}

// spanName renders "{method} {route}" per the HTTP semantic
// conventions.
//
// A Go 1.22 mux pattern may already carry the method ("GET /livez"), so
// the method is stripped from the pattern before it is re-prefixed —
// otherwise the liveness probe would produce spans named "GET GET
// /livez". Both halves are server-owned strings from a closed set, so
// the result is bounded whatever the caller sends.
func spanName(method, route string) string {
	if _, rest, ok := strings.Cut(route, " "); ok {
		route = rest
	}
	return method + " " + route
}

// toolSpanAttrs builds the bounded attribute set for an MCP tool span.
//
// The tool NAME is clamped through telemetry.BoundedTool, and the only
// other thing recorded about the call is how big its argument payload
// was. No argument value is read: `query`, `id`, `collection`, `key`,
// `title`, `content` and `tags` are all caller text, and several of them
// are the exact classes of data this whole subsystem is written not to
// export.
func toolSpanAttrs(req mcp.CallToolRequest) []attribute.KeyValue {
	return []attribute.KeyValue{
		telemetry.KeyMCPTool.String(telemetry.BoundedTool(req.Params.Name)),
		telemetry.KeyMCPMethod.String("tools/call"),
		telemetry.KeyMCPRequestBytes.Int(payloadBytes(req.Params.Arguments)),
	}
}

// payloadBytes measures a tool's arguments without retaining them. It
// re-encodes rather than reading a raw body because mcp-go hands the
// handler a decoded value; the cost is paid only when tracing is on.
func payloadBytes(v any) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// resultBytes measures a tool result's text content. Sizes only; the
// text itself never reaches a span or a metric.
func resultBytes(res *mcp.CallToolResult) int {
	if res == nil {
		return 0
	}
	total := 0
	for _, c := range res.Content {
		if txt, ok := c.(mcp.TextContent); ok {
			total += len(txt.Text)
		}
	}
	return total
}
