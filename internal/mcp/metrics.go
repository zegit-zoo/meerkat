package mcp

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// metrics is the hosted server's Prometheus instrumentation.
//
// # Label discipline
//
// /metrics is unauthenticated (see HostedServer.routes), so a label is
// as public as the endpoint. Nothing here is labelled with a collection
// name, a page ID, a query string, a caller subject or a URL path taken
// from the request: those are either private (the mounted set is not
// public information) or unbounded (a path label turns one scrape into
// a cardinality bomb the moment something scans for /wp-admin).
//
// The route label is therefore the server's OWN route pattern from a
// closed set of five, resolved through the mux — never r.URL.Path.
type metrics struct {
	reg *prometheus.Registry

	requests         *prometheus.CounterVec
	duration         *prometheus.HistogramVec
	authFailures     *prometheus.CounterVec
	toolCalls        *prometheus.CounterVec
	toolDuration     *prometheus.HistogramVec
	sessions         prometheus.Gauge
	ready            prometheus.Gauge
	collections      prometheus.Gauge
	collectionsReady prometheus.Gauge
	collectionsStale prometheus.Gauge
	buildInfo        *prometheus.GaugeVec

	// routeOf resolves a request to one of the fixed route labels. Set
	// by instrumentHTTP once the mux is known.
	routeOf func(*http.Request) string
}

func newMetrics(reg *prometheus.Registry, version string, mounted int) *metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &metrics{
		reg: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_http_requests_total",
			Help: "HTTP requests handled by the hosted MCP server, by route, method and status class.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_http_request_duration_seconds",
			Help:    "HTTP request latency for the hosted MCP server.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_auth_failures_total",
			Help: "Requests refused by the authentication gate, by reason (missing_token, invalid_token, no_grants).",
		}, []string{"reason"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_mcp_tool_calls_total",
			Help: "MCP tool invocations, by tool name and outcome (ok, tool_error, error).",
		}, []string{"tool", "outcome"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_mcp_tool_duration_seconds",
			Help:    "MCP tool handler latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tool"}),
		sessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_mcp_sessions_active",
			Help: "MCP sessions currently registered on this process.",
		}),
		ready: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_ready",
			Help: "1 when every mounted collection enumerates and holds a built search index, else 0.",
		}),
		collections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_collections_mounted",
			Help: "Number of collections mounted by this process (the deployment's total, not any caller's view).",
		}),
		collectionsReady: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_collections_ready",
			Help: "Number of mounted collections currently answering queries.",
		}),
		collectionsStale: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_collections_degraded",
			Help: "Number of mounted collections whose most recent refresh failed and which are serving the last known-good snapshot.",
		}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "meerkat_build_info",
			Help: "Always 1; the version label carries the running meerkat version.",
		}, []string{"version"}),
	}
	reg.MustRegister(
		m.requests, m.duration, m.authFailures, m.toolCalls,
		m.toolDuration, m.sessions, m.ready, m.collections,
		m.collectionsReady, m.collectionsStale, m.buildInfo,
	)
	m.collections.Set(float64(mounted))
	m.buildInfo.WithLabelValues(version).Set(1)
	return m
}

func (m *metrics) setReady(ready bool) {
	if ready {
		m.ready.Set(1)
		return
	}
	m.ready.Set(0)
}

// setCollectionStates publishes the two COUNTS a readiness computation
// produces. Counts, not per-collection series: naming a collection in a
// label would put the mounted set on an unauthenticated endpoint, which
// is the one thing /readyz's body is careful not to do. The refresh
// metrics carry per-target detail keyed by configuration ordinal (see
// internal/refresh), and the log carries the names.
// It also returns the two counts and the total enumerated page count, so
// the caller can put the same numbers on a readiness span without
// re-walking the slice. The page total is a TOTAL, deliberately: a
// per-collection gauge would name the mounted set on an unauthenticated
// endpoint, which is the one thing /readyz's body is careful not to do.
func (m *metrics) setCollectionStates(health []collections.Health) (ready, degraded, pages int) {
	for _, h := range health {
		if h.Ready {
			ready++
		}
		if h.Degraded {
			degraded++
		}
		pages += h.Pages
	}
	m.collectionsReady.Set(float64(ready))
	m.collectionsStale.Set(float64(degraded))
	return ready, degraded, pages
}

// instrumentHTTP wraps the mux, counting and timing every request under
// the mux's own matched pattern.
//
// Resolving the label through mux.Handler means an unmatched request
// (a scanner probing /wp-admin, say) collapses to the empty pattern,
// which is reported as "other" — one bounded label value instead of one
// per probe.
func (m *metrics) instrumentHTTP(mux *http.ServeMux) http.Handler {
	m.routeOf = func(r *http.Request) string {
		_, pattern := mux.Handler(r)
		if pattern == "" {
			return "other"
		}
		return pattern
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := m.routeOf(r)
		timer := prometheus.NewTimer(m.duration.WithLabelValues(route, r.Method))
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			w = rec
		}
		mux.ServeHTTP(w, r)
		timer.ObserveDuration()
		m.requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
	})
}

// instrumentTool counts and times tool handler invocations, and — when
// tracing is on — wraps each one in the MCP tool span.
//
// The outcome label distinguishes a tool-level error (a bad query, an
// unknown collection — the model's problem, handed back as a normal
// result) from a transport-level error (meerkat's problem). Conflating
// them would make a dashboard read as broken every time a model
// mistyped a page ID. The span uses the same three-value vocabulary, so
// a metric and a trace group the same way.
//
// The span is created here rather than in each handler for the same
// reason the counter is: one place, so a tool added later is instrumented
// by existing. It is a CHILD of the root HTTP span — mcp-go threads the
// request context through to the handler — which is what makes a search
// call one trace from the wire down to the bleve query.
func (m *metrics) instrumentTool(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.Params.Name
		tel := telemetry.FromContext(ctx)
		ctx, span := tel.Start(ctx, telemetry.SpanMCPTool,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(toolSpanAttrs(req)...))

		timer := prometheus.NewTimer(m.toolDuration.WithLabelValues(name))
		res, err := next(ctx, req)
		timer.ObserveDuration()

		outcome := telemetry.OutcomeOK
		switch {
		case err != nil:
			outcome = telemetry.OutcomeError
		case res != nil && res.IsError:
			outcome = telemetry.OutcomeToolError
		}
		m.toolCalls.WithLabelValues(name, outcome).Inc()

		if tel.Tracing() {
			bounded := telemetry.BoundedTool(name)
			size := resultBytes(res)
			span.SetAttributes(
				telemetry.Outcome(outcome),
				telemetry.KeyMCPResponseBytes.Int(size),
			)
			if outcome == telemetry.OutcomeError {
				// The error object is deliberately NOT recorded: a
				// transport-level failure's message can quote a page ID or a
				// collection name, and neither may leave the process on a
				// span. The bounded outcome is what a consumer groups by
				// anyway, and the full text is already in the structured log.
				span.SetStatus(codes.Error, telemetry.OutcomeError)
			}
			tel.Metrics().ToolPayload(bounded, "request", payloadBytes(req.Params.Arguments))
			tel.Metrics().ToolPayload(bounded, "response", size)
		}
		span.End()
		return res, err
	}
}

// defaultLogWriter is where the default structured logger writes.
// stderr, not stdout: stdout is the stdio transport's wire, and a
// server that logged there would corrupt any future in-process reuse of
// the same handlers.
func defaultLogWriter() io.Writer { return os.Stderr }
