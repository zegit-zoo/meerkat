package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
)

// provider.go builds (and tears down) the OpenTelemetry SDK.
//
// Everything here is reached only when an operator opted in. New's first
// act is to return (nil, nil) for a configuration that asked for
// nothing, and every caller is written so that a nil *Telemetry is the
// normal, fully-supported state rather than an error to guard against.

// Signal names for the export-failure counter.
const (
	SignalTraces  = "traces"
	SignalMetrics = "metrics"
)

// Telemetry is one hosted server's telemetry: the tracer its spans come
// from, the domain metric recorder, and the SDK machinery to shut down.
//
// A nil *Telemetry is a valid, complete implementation of "telemetry is
// off". Every method below tolerates one.
type Telemetry struct {
	cfg     Resolved
	tracer  trace.Tracer
	metrics *Metrics
	log     *slog.Logger
	prop    propagation.TextMapPropagator

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	shutdown       []func(context.Context) error
	// restoreGlobals undoes the process-global OpenTelemetry providers
	// this instance installed. Nil when it installed none.
	restoreGlobals func()
}

// Options configures New. It mirrors refresh.Options' shape — a config,
// a registry, a logger, all optional — because it is the same kind of
// object: an opt-in subsystem the hosted server constructs
// unconditionally and then uses without checking.
type Options struct {
	// Config is the parsed observability: block. Nil is fine: the
	// environment alone can still enable tracing.
	Config *Config
	// Registry is the Prometheus registry the domain collectors are
	// registered on. Nil registers nowhere, which is what a test or a
	// deployment with metrics.prometheus: false wants.
	Registry *prometheus.Registry
	// Logger receives exporter lifecycle and (rate-limited) export
	// failure events. Nil discards them.
	Logger *slog.Logger
	// Version is stamped as service.version.
	Version string
	// SpanExporter overrides the exporter built from Config.
	//
	// It is THE test seam, and it is the reason no test in this repo
	// needs a collector, a socket or a network namespace: every test
	// passes sdk/trace/tracetest's in-memory exporter here. Production
	// never sets it — there is no configuration key that could.
	SpanExporter sdktrace.SpanExporter
	// SetGlobals installs this instance as the process-global
	// OpenTelemetry tracer provider and propagator.
	//
	// It exists so third-party clients that are instrumented against the
	// globals — the Google Cloud Storage client, above all — join
	// meerkat's traces instead of emitting into a void. It is off by
	// default so that a test, or a second server in the same process,
	// cannot reach through the globals into somebody else's exporter;
	// `mk mcp serve-http` turns it on, because there is exactly one
	// server in that process.
	SetGlobals bool
}

// New builds telemetry for opts, or returns (nil, nil) when nothing was
// configured.
//
// The (nil, nil) return is the load-bearing half of this function. A
// deployment with no observability configuration gets a nil *Telemetry,
// the hosted server never installs one into a request context, and every
// instrumented call site below the request path takes its nil branch.
// No SDK is constructed, no exporter dialled, no goroutine started, no
// global mutated — the process is byte-identical to one built before
// this package existed.
func New(ctx context.Context, opts Options) (*Telemetry, error) {
	cfg, err := Resolve(opts.Config)
	if err != nil {
		return nil, err
	}
	if !cfg.Active() {
		return nil, nil
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	t := &Telemetry{
		cfg: cfg,
		log: log,
		// TraceContext ONLY, deliberately. W3C baggage is not propagated
		// in either direction: meerkat never reads it (trace context is
		// correlation data, never authorization data — see
		// docs/SECURITY.md), and forwarding a caller's arbitrary baggage
		// onto meerkat's outbound OIDC discovery/JWKS calls would push
		// somebody else's key/value pairs to the identity provider. Not
		// carrying it is a stronger statement than carrying it carefully.
		prop: propagation.TraceContext{},
	}
	if cfg.PrometheusMetrics {
		t.metrics = newMetrics(opts.Registry)
	} else {
		t.metrics = newMetrics(nil)
	}

	res, err := buildResource(ctx, cfg, opts.Version)
	if err != nil {
		return nil, err
	}

	if cfg.TracesEnabled {
		if err := t.startTracing(ctx, res, opts.SpanExporter); err != nil {
			return nil, err
		}
	}
	if cfg.OTLPMetrics {
		if err := t.startMetrics(ctx, res); err != nil {
			_ = t.Shutdown(ctx)
			return nil, err
		}
	}

	// Route the SDK's own internal errors (an export that failed, a
	// malformed attribute) into the structured log and the local counter
	// instead of otel's default, which writes to the standard logger.
	// Diagnostics belong on stderr with everything else, never on stdout
	// — stdout is the stdio transport's wire.
	otel.SetErrorHandler(&errorHandler{log: log, metrics: t.metrics, limiter: newRateLimiter(time.Minute)})

	if opts.SetGlobals {
		t.installGlobals()
	}

	t.log.Info("telemetry started",
		"service", cfg.ServiceName,
		"traces", cfg.TracesEnabled,
		"otlp_metrics", cfg.OTLPMetrics,
		"protocol", cfg.Protocol,
		"exporting", cfg.Exports(),
		"sample_ratio", cfg.SampleRatio)
	return t, nil
}

// startTracing builds the tracer provider: an exporter (or none), a
// bounded batch processor, and a parent-based sampler.
func (t *Telemetry) startTracing(ctx context.Context, res *resource.Resource, override sdktrace.SpanExporter) error {
	exp := override
	if exp == nil && t.cfg.Exports() {
		built, err := t.buildSpanExporter(ctx)
		if err != nil {
			return err
		}
		exp = built
	}

	popts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		// ParentBased so a trace a gateway already sampled is not
		// re-sampled here: a caller who decided to keep this trace gets a
		// complete one, and a caller who decided to drop it does not get
		// an orphaned meerkat-only fragment. sample_ratio therefore
		// governs the traces this process STARTS.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(t.cfg.SampleRatio))),
	}
	if exp != nil {
		bp := newBatchProcessor(exp, batchOptions{
			queueSize:     t.cfg.QueueSize,
			batchSize:     t.cfg.BatchSize,
			batchTimeout:  t.cfg.BatchTimeout.Duration(),
			exportTimeout: t.cfg.ExportTimeout.Duration(),
			log:           t.log,
			metrics:       t.metrics,
		})
		popts = append(popts, sdktrace.WithSpanProcessor(bp))
	}
	tp := sdktrace.NewTracerProvider(popts...)
	t.tracerProvider = tp
	t.tracer = tp.Tracer(instrumentationScope)
	t.shutdown = append(t.shutdown, tp.Shutdown)
	return nil
}

// buildSpanExporter dials the collector.
//
// TLS is on unless the configuration says otherwise, in both protocols.
// The OTLP libraries default the other way for a bare host:port, so the
// insecure option is applied explicitly rather than left to the library.
func (t *Telemetry) buildSpanExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	endpoint := t.cfg.tracesTarget()
	timeout := t.cfg.ExportTimeout.Duration()
	switch t.cfg.Protocol {
	case ProtocolHTTP:
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(normalizeHTTPEndpoint(endpoint, t.cfg.Insecure)),
			otlptracehttp.WithTimeout(timeout),
		}
		if len(t.cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(t.cfg.Headers))
		}
		if t.cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otlp/http trace exporter: %w", err)
		}
		return exp, nil
	default:
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(stripScheme(endpoint)),
			otlptracegrpc.WithTimeout(timeout),
		}
		if len(t.cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(t.cfg.Headers))
		}
		if t.cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exp, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otlp/grpc trace exporter: %w", err)
		}
		return exp, nil
	}
}

// startMetrics adds an OTLP metric pipeline BESIDE the Prometheus
// registry, never instead of it. /metrics keeps serving every collector
// it served before; a deployment that turns OTLP metrics on gets a
// second copy of the OpenTelemetry-native ones, and one that turns it
// off loses nothing.
func (t *Telemetry) startMetrics(ctx context.Context, res *resource.Resource) error {
	if !t.cfg.Exports() && t.cfg.metricsTarget() == "" {
		return errors.New("observability.metrics.otlp is true but no OTLP endpoint is configured — set observability.otlp.endpoint or OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	exp, err := t.buildMetricExporter(ctx)
	if err != nil {
		return err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(t.cfg.MetricInterval.Duration()),
			sdkmetric.WithTimeout(t.cfg.ExportTimeout.Duration()),
		)),
	)
	t.meterProvider = mp
	t.shutdown = append(t.shutdown, mp.Shutdown)
	return nil
}

func (t *Telemetry) buildMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	endpoint := t.cfg.metricsTarget()
	timeout := t.cfg.ExportTimeout.Duration()
	switch t.cfg.Protocol {
	case ProtocolHTTP:
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(normalizeHTTPEndpoint(endpoint, t.cfg.Insecure)),
			otlpmetrichttp.WithTimeout(timeout),
		}
		if len(t.cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(t.cfg.Headers))
		}
		if t.cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otlp/http metric exporter: %w", err)
		}
		return exp, nil
	default:
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(stripScheme(endpoint)),
			otlpmetricgrpc.WithTimeout(timeout),
		}
		if len(t.cfg.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(t.cfg.Headers))
		}
		if t.cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exp, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otlp/grpc metric exporter: %w", err)
		}
		return exp, nil
	}
}

// installGlobals points the process-global OpenTelemetry providers at
// this instance and records how to put them back.
func (t *Telemetry) installGlobals() {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	if t.tracerProvider != nil {
		otel.SetTracerProvider(t.tracerProvider)
	}
	otel.SetTextMapPropagator(t.prop)
	t.restoreGlobals = func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
}

// buildResource assembles the resource attributes every exported record
// carries: what this process is, what version it runs, and where it
// runs. Nothing caller-derived reaches it.
func buildResource(ctx context.Context, cfg Resolved, version string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(cfg.Environment))
	}
	for k, v := range cfg.ResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	// resource.New with no detectors: meerkat does not probe the cloud
	// metadata server at startup. A detector that reaches the network is
	// a startup dependency, and this whole subsystem is explicitly not
	// allowed to be one.
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}
	return res, nil
}

// --- accessors, all nil-safe ------------------------------------------

// Enabled reports whether telemetry is doing anything.
func (t *Telemetry) Enabled() bool { return t != nil }

// Tracing reports whether spans are being created.
func (t *Telemetry) Tracing() bool { return t != nil && t.tracer != nil }

// Metrics returns the domain metric recorder, or nil.
func (t *Telemetry) Metrics() *Metrics {
	if t == nil {
		return nil
	}
	return t.metrics
}

// IncludeTraceContext reports whether logs should carry trace_id/span_id.
func (t *Telemetry) IncludeTraceContext() bool {
	return t != nil && t.cfg.IncludeTraceContext && t.tracer != nil
}

// Start begins a span. With no tracer (telemetry configured for metrics
// only, or no telemetry at all) it returns ctx unchanged and a
// non-recording span.
func (t *Telemetry) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if t == nil || t.tracer == nil {
		return ctx, nonRecording
	}
	return t.tracer.Start(ctx, name, opts...)
}

// Extract reads W3C trace context from inbound request headers.
//
// A MALFORMED traceparent is ignored: the W3C propagator returns a
// context with an invalid span context rather than an error, and the
// span started from it is simply a new root. That is the required
// behaviour — a caller must not be able to make a request fail, or a
// span disappear, by sending a broken header — and it costs nothing,
// because trace context is correlation data and meerkat never reads it
// for any decision.
func (t *Telemetry) Extract(ctx context.Context, h http.Header) context.Context {
	if t == nil {
		return ctx
	}
	return t.prop.Extract(ctx, propagation.HeaderCarrier(h))
}

// Inject writes this context's trace context into outbound headers.
func (t *Telemetry) Inject(ctx context.Context, h http.Header) {
	if t == nil {
		return
	}
	t.prop.Inject(ctx, propagation.HeaderCarrier(h))
}

// LogAttrs returns the trace correlation attributes for a log line: the
// trace and span IDs of the span active in ctx, or nothing.
//
// IDs only. A span's attributes are not copied into the log and the
// log's identity fields are not copied into the span; the two carry
// different data on purpose and this function is the whole of the bridge
// between them.
func (t *Telemetry) LogAttrs(ctx context.Context) []any {
	if !t.IncludeTraceContext() {
		return nil
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []any{"trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String()}
}

// HTTPClient returns base wrapped so that outbound requests carry this
// process's trace context and produce a client span.
//
// It is what internal/authn's OIDC discovery and JWKS fetches go
// through, which is what makes "the token verification took 400ms
// because the IdP was slow" visible in the same trace as the request
// that waited for it. A nil *Telemetry returns base untouched.
func (t *Telemetry) HTTPClient(base *http.Client) *http.Client {
	if t == nil || t.tracer == nil {
		return base
	}
	if base == nil {
		base = &http.Client{Timeout: 15 * time.Second}
	}
	clone := *base
	inner := clone.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	clone.Transport = &tracingTransport{inner: inner, tel: t}
	return &clone
}

// ForceFlush exports everything currently queued and waits for it,
// bounded by ctx.
//
// It exists for tests — an in-memory exporter is only inspectable once
// the batch has gone — and for a caller that wants a checkpoint. It is
// deliberately NOT called on the request path: flushing per request
// would make every request wait on the collector, which is the coupling
// this whole subsystem refuses.
func (t *Telemetry) ForceFlush(ctx context.Context) error {
	if t == nil || t.tracerProvider == nil {
		return nil
	}
	return t.tracerProvider.ForceFlush(ctx)
}

// Shutdown flushes and stops every pipeline, bounded by the configured
// shutdown timeout.
//
// It NEVER blocks shutdown on a collector. The timeout is applied here
// rather than inherited from the caller precisely so that a pod being
// terminated while its collector is already gone still terminates: the
// flush is attempted, it fails or expires, the error is returned for the
// log, and the process continues to exit.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if t.restoreGlobals != nil {
		t.restoreGlobals()
		t.restoreGlobals = nil
	}
	timeout := t.cfg.ShutdownTimeout.Duration()
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	var errs []error
	for _, fn := range t.shutdown {
		if err := fn(shutdownCtx); err != nil {
			errs = append(errs, err)
		}
	}
	t.shutdown = nil
	err := errors.Join(errs...)
	if err != nil {
		t.log.Warn("telemetry shutdown did not flush cleanly — continuing shutdown anyway", "error", err.Error())
	} else {
		t.log.Info("telemetry stopped")
	}
	return err
}

// tracingTransport injects trace context into an outbound request and
// wraps it in a client span.
//
// The span carries the request method and the URL's SCHEME AND HOST —
// which are meerkat's own configured issuer, not caller input — and
// never the path or query, because an OIDC discovery URL's path is
// derived from configuration and a JWKS URL's is derived from the
// issuer's own document. Host is what an operator needs to see "the IdP
// was slow"; the rest adds nothing and would be a second thing to audit.
type tracingTransport struct {
	inner http.RoundTripper
	tel   *Telemetry
}

func (rt *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := rt.tel.Start(req.Context(), "HTTP "+req.Method, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	span.SetAttributes(
		semconv.HTTPRequestMethodKey.String(req.Method),
		semconv.ServerAddress(req.URL.Hostname()),
		semconv.URLScheme(req.URL.Scheme),
	)
	// Clone before mutating headers: a RoundTripper must not modify the
	// request it was given.
	out := req.Clone(ctx)
	rt.tel.Inject(ctx, out.Header)
	resp, err := rt.inner.RoundTrip(out)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	return resp, nil
}

// stripScheme reduces an endpoint to the host:port form the gRPC
// exporter wants, so one `endpoint:` value works for both protocols.
func stripScheme(endpoint string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(endpoint) > len(prefix) && endpoint[:len(prefix)] == prefix {
			return endpoint[len(prefix):]
		}
	}
	return endpoint
}

// normalizeHTTPEndpoint gives the HTTP exporter a URL. A bare host:port
// (the natural thing to write, and what the gRPC form takes) is given
// the scheme the TLS setting implies rather than being rejected.
func normalizeHTTPEndpoint(endpoint string, insecure bool) string {
	if endpoint == "" {
		return ""
	}
	for _, prefix := range []string{"https://", "http://"} {
		if len(endpoint) > len(prefix) && endpoint[:len(prefix)] == prefix {
			return endpoint
		}
	}
	if insecure {
		return "http://" + endpoint
	}
	return "https://" + endpoint
}
