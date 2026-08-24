package telemetry

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// config.go is the `observability:` block of content-source.yaml, and
// the rule for how it composes with the standard OTEL_* environment
// variables.
//
// # Precedence, stated once
//
//	explicit meerkat configuration  >  OTEL_* environment  >  default
//
// A field WRITTEN in the observability: block wins. A field left out
// falls back to the standard OpenTelemetry environment variable for the
// same setting, and only then to meerkat's default. The direction is
// deliberate and it is the one that makes both audiences right:
//
//   - The file is the artifact under review. An operator who wrote
//     `sample_ratio: 0.1` there and reads the deployment back should see
//     0.1, not whatever a base image happened to export. A file that can
//     be silently overridden by ambient environment is a file you cannot
//     audit.
//   - The environment is how a platform injects things nobody wants in a
//     repository — a collector address that differs per cluster, and
//     above all CREDENTIALS. Leaving a field out is how you say "the
//     platform decides this one", and it keeps working.
//
// Every fallback is per-FIELD, not per-block: a file that sets only
// `traces.enabled: true` still picks its endpoint up from
// OTEL_EXPORTER_OTLP_ENDPOINT.
//
// # Credentials are the exception, and they are not expressible here
//
// There is no `headers:` field. There is `headers_env:`, which NAMES an
// environment variable whose value is read at startup. A content-source
// .yaml is committed, shared between environments and frequently baked
// into an image; a bearer token for a hosted collector has no business
// in one. The indirection makes the wrong thing unwriteable rather than
// discouraged — the same reasoning that gives type: gcs no
// service-account-key field.

// Config is the optional `observability:` block.
//
// A nil *Config, and a *Config with nothing enabled, both mean "the
// server behaves exactly as it did before this package existed".
type Config struct {
	// ServiceName is the OpenTelemetry service.name resource attribute.
	// Empty falls back to OTEL_SERVICE_NAME, then "meerkat".
	ServiceName string `yaml:"service_name,omitempty"`
	// Environment is the deployment.environment.name resource attribute
	// (staging, production, ...). Empty means the attribute is omitted
	// rather than guessed.
	Environment string `yaml:"environment,omitempty"`

	Logs    LogConfig    `yaml:"logs,omitempty"`
	Metrics MetricConfig `yaml:"metrics,omitempty"`
	Traces  TraceConfig  `yaml:"traces,omitempty"`
	OTLP    OTLPConfig   `yaml:"otlp,omitempty"`
	Limits  ExportLimits `yaml:"limits,omitempty"`
}

// LogConfig tunes the structured logs that already exist. It cannot turn
// them off: stderr JSON logging is the hosted server's baseline and
// removing it is not an observability feature.
type LogConfig struct {
	// Level is debug | info | warn | error. Empty keeps the current
	// default (info).
	Level string `yaml:"level,omitempty"`
	// Format is json | text. Empty keeps json.
	Format string `yaml:"format,omitempty"`
	// IncludeTraceContext adds trace_id/span_id to access and auth logs
	// when a span is active. Nil (unset) means "yes, whenever tracing is
	// on" — the correlation is the entire point of turning tracing on,
	// and a deployment that wants the old log shape can say false.
	IncludeTraceContext *bool `yaml:"include_trace_context,omitempty"`
}

// MetricConfig selects where metrics go. Prometheus is not optional in
// the sense that matters: `prometheus: false` stops meerkat REGISTERING
// the OTLP-only extras nowhere useful, but /metrics stays mounted and
// keeps serving every collector that was there before, because a
// deployment scraping it must not lose its dashboards to an
// observability opt-in.
type MetricConfig struct {
	// Prometheus keeps the domain collectors on the /metrics registry.
	// Nil (unset) means true.
	Prometheus *bool `yaml:"prometheus,omitempty"`
	// OTLP additionally exports the OpenTelemetry runtime/domain meters
	// over OTLP. Off by default.
	OTLP bool `yaml:"otlp,omitempty"`
	// Interval is the OTLP metric export period. Zero falls back to
	// OTEL_METRIC_EXPORT_INTERVAL, then 60s.
	Interval Duration `yaml:"interval,omitempty"`
}

// TraceConfig turns tracing on and decides how much of it is kept.
type TraceConfig struct {
	// Enabled turns span creation on. Off by default: with it off, no
	// SDK tracer provider is built at all.
	Enabled bool `yaml:"enabled,omitempty"`
	// SampleRatio is the head-sampling ratio for traces this process
	// STARTS, between 0 and 1. A trace continued from a valid inbound
	// traceparent inherits the caller's decision (parent-based), so a
	// gateway that already sampled is not re-sampled here.
	//
	// Nil (unset) falls back to OTEL_TRACES_SAMPLER/_ARG, then to 1.0 —
	// which is right because Enabled is already the off switch: an
	// operator who typed `enabled: true` and nothing else wants to see
	// traces, not a tenth of them.
	SampleRatio *float64 `yaml:"sample_ratio,omitempty"`
}

// OTLPConfig addresses the collector.
type OTLPConfig struct {
	// Endpoint is host:port for grpc, or a base URL for http. Empty
	// falls back to OTEL_EXPORTER_OTLP_ENDPOINT; with neither, tracing
	// still runs (spans are created, logs correlate) and nothing is
	// exported — useful for the correlation alone, and the state every
	// test runs in.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Protocol is grpc | http/protobuf. Empty falls back to
	// OTEL_EXPORTER_OTLP_PROTOCOL, then grpc.
	Protocol string `yaml:"protocol,omitempty"`
	// Insecure disables TLS to the collector. It defaults to FALSE and
	// has to be written out, because an in-cluster collector address
	// looks local and a plaintext export of request metadata across a
	// cluster network is a disclosure an operator should have to type.
	Insecure bool `yaml:"insecure,omitempty"`
	// HeadersEnv names an environment variable holding OTLP headers in
	// the standard W3C-baggage-ish `k1=v1,k2=v2` form. The VARIABLE NAME
	// lives in the file; the value never does. Empty falls back to
	// OTEL_EXPORTER_OTLP_HEADERS.
	HeadersEnv string `yaml:"headers_env,omitempty"`
	// Timeout bounds one export attempt. Zero falls back to
	// OTEL_EXPORTER_OTLP_TIMEOUT, then 10s.
	Timeout Duration `yaml:"timeout,omitempty"`
}

// ExportLimits bounds what an unreachable collector can cost. Every one
// of these has a default; the block exists so an operator with a tight
// memory budget can lower them, not so anybody has to think about them.
type ExportLimits struct {
	// QueueSize is the maximum number of spans held for export. Beyond
	// it, spans are DROPPED and counted
	// (meerkat_otel_spans_dropped_total) — never buffered without bound,
	// because an exporter outage must cost a fixed amount of memory
	// rather than the process.
	QueueSize int `yaml:"queue_size,omitempty"`
	// BatchSize is the maximum spans per export request.
	BatchSize int `yaml:"batch_size,omitempty"`
	// BatchTimeout is how long a partial batch waits before going.
	BatchTimeout Duration `yaml:"batch_timeout,omitempty"`
	// ShutdownTimeout bounds the final flush. Shutdown continues when it
	// expires: a collector that is down must not hold a pod in
	// Terminating.
	ShutdownTimeout Duration `yaml:"shutdown_timeout,omitempty"`
}

// Standard OpenTelemetry environment variables, named as constants
// because they are part of the documented precedence contract.
const (
	envServiceName        = "OTEL_SERVICE_NAME"
	envResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"
	envEndpoint           = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint     = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envMetricsEndpoint    = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envProtocol           = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envInsecure           = "OTEL_EXPORTER_OTLP_INSECURE"
	envHeaders            = "OTEL_EXPORTER_OTLP_HEADERS"
	envTimeout            = "OTEL_EXPORTER_OTLP_TIMEOUT"
	envSampler            = "OTEL_TRACES_SAMPLER"
	envSamplerArg         = "OTEL_TRACES_SAMPLER_ARG"
	envSDKDisabled        = "OTEL_SDK_DISABLED"
	envMetricInterval     = "OTEL_METRIC_EXPORT_INTERVAL"
	// envTracesEnabled is meerkat's own switch for turning tracing on
	// with no config file at all — a container image with no
	// content-source.yaml still needs a way in. It is namespaced under
	// MEERKAT_ rather than OTEL_ because there is no standard variable
	// for "does the application create spans"; OTEL_SDK_DISABLED is only
	// an off switch.
	envTracesEnabled = "MEERKAT_TRACES_ENABLED"
)

// Protocol values.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// Defaults, all of them overridable per the precedence rule above.
const (
	defaultServiceName     = "meerkat"
	defaultQueueSize       = 2048
	defaultBatchSize       = 512
	defaultBatchTimeout    = 5_000_000_000  // 5s
	defaultExportTimeout   = 10_000_000_000 // 10s
	defaultShutdownTimeout = 5_000_000_000  // 5s
	defaultMetricInterval  = 60_000_000_000 // 60s
)

// Resolved is a Config with the environment folded in and every default
// applied — the shape the rest of the package works from, so nothing
// below this file has to re-answer "was that set?".
type Resolved struct {
	ServiceName        string
	Environment        string
	ResourceAttributes map[string]string

	LogLevel            string
	LogFormat           string
	IncludeTraceContext bool

	PrometheusMetrics bool
	OTLPMetrics       bool
	MetricInterval    Duration

	TracesEnabled bool
	SampleRatio   float64

	Endpoint        string
	TracesEndpoint  string
	MetricsEndpoint string
	Protocol        string
	Insecure        bool
	Headers         map[string]string
	ExportTimeout   Duration

	QueueSize       int
	BatchSize       int
	BatchTimeout    Duration
	ShutdownTimeout Duration
}

// Exports reports whether a collector address was resolved at all. With
// no endpoint, tracing can still be enabled: spans are created and
// correlate the logs, and nothing leaves the process.
func (r Resolved) Exports() bool { return r.tracesTarget() != "" }

func (r Resolved) tracesTarget() string {
	if r.TracesEndpoint != "" {
		return r.TracesEndpoint
	}
	return r.Endpoint
}

func (r Resolved) metricsTarget() string {
	if r.MetricsEndpoint != "" {
		return r.MetricsEndpoint
	}
	return r.Endpoint
}

// Active reports whether anything at all should be constructed. A
// Resolved that is not Active is indistinguishable from no
// configuration: New returns nil and the process is byte-identical to
// one built before this package existed.
func (r Resolved) Active() bool { return r.TracesEnabled || r.OTLPMetrics }

// Resolve folds cfg together with the process environment and the
// defaults, per the precedence rule documented at the top of this file.
//
// A nil cfg is a valid input and means "the file said nothing": the
// environment alone can still turn tracing on, which is what a
// deployment with no content-source.yaml needs.
func Resolve(cfg *Config) (Resolved, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	var r Resolved

	// OTEL_SDK_DISABLED is the standard kill switch and it beats
	// everything, including an explicit `enabled: true`. That inversion
	// is deliberate: an off switch that a config file can override is not
	// an off switch, and an operator disabling telemetry fleet-wide in an
	// incident should not have to find every file that opted in.
	if boolEnv(envSDKDisabled) {
		return Resolved{}, nil
	}

	r.ServiceName = first(cfg.ServiceName, os.Getenv(envServiceName), defaultServiceName)
	r.Environment = cfg.Environment
	attrs, err := parseKeyValues(os.Getenv(envResourceAttributes))
	if err != nil {
		return Resolved{}, fmt.Errorf("%s: %w", envResourceAttributes, err)
	}
	r.ResourceAttributes = attrs

	r.LogLevel = cfg.Logs.Level
	r.LogFormat = cfg.Logs.Format
	r.IncludeTraceContext = cfg.Logs.IncludeTraceContext == nil || *cfg.Logs.IncludeTraceContext

	r.PrometheusMetrics = cfg.Metrics.Prometheus == nil || *cfg.Metrics.Prometheus
	r.OTLPMetrics = cfg.Metrics.OTLP
	r.MetricInterval = firstDuration(cfg.Metrics.Interval, durationEnv(envMetricInterval), defaultMetricInterval)

	r.TracesEnabled = cfg.Traces.Enabled || boolEnv(envTracesEnabled)
	switch {
	case cfg.Traces.SampleRatio != nil:
		r.SampleRatio = *cfg.Traces.SampleRatio
	default:
		r.SampleRatio, err = samplerFromEnv()
		if err != nil {
			return Resolved{}, err
		}
	}
	if r.SampleRatio < 0 || r.SampleRatio > 1 {
		return Resolved{}, fmt.Errorf("observability.traces.sample_ratio must be between 0 and 1, got %v", r.SampleRatio)
	}

	r.Endpoint = first(cfg.OTLP.Endpoint, os.Getenv(envEndpoint))
	r.TracesEndpoint = os.Getenv(envTracesEndpoint)
	r.MetricsEndpoint = os.Getenv(envMetricsEndpoint)
	r.Protocol = first(cfg.OTLP.Protocol, os.Getenv(envProtocol), ProtocolGRPC)
	switch r.Protocol {
	case ProtocolGRPC, ProtocolHTTP:
	case "http":
		// The standard spells it http/protobuf; "http" is the obvious
		// thing to write and meaning it is unambiguous, so accept it
		// rather than failing a deployment over a slash.
		r.Protocol = ProtocolHTTP
	default:
		return Resolved{}, fmt.Errorf("observability.otlp.protocol must be %q or %q, got %q", ProtocolGRPC, ProtocolHTTP, r.Protocol)
	}
	// TLS is on unless the file says otherwise. The environment can only
	// turn it off when the file said nothing at all — an operator who
	// wrote `insecure: false` has stated a security posture, and ambient
	// environment must not quietly revoke it.
	r.Insecure = cfg.OTLP.Insecure || (!cfg.otlpTLSStated() && boolEnv(envInsecure))
	r.ExportTimeout = firstDuration(cfg.OTLP.Timeout, durationEnv(envTimeout), defaultExportTimeout)

	headers, err := resolveHeaders(cfg.OTLP.HeadersEnv)
	if err != nil {
		return Resolved{}, err
	}
	r.Headers = headers

	r.QueueSize = firstInt(cfg.Limits.QueueSize, defaultQueueSize)
	r.BatchSize = firstInt(cfg.Limits.BatchSize, defaultBatchSize)
	r.BatchTimeout = firstDuration(cfg.Limits.BatchTimeout, 0, defaultBatchTimeout)
	r.ShutdownTimeout = firstDuration(cfg.Limits.ShutdownTimeout, 0, defaultShutdownTimeout)

	if err := r.validateEndpoints(); err != nil {
		return Resolved{}, err
	}
	return r, nil
}

// otlpTLSStated reports whether the file wrote `insecure:` at all. YAML
// gives an absent bool and an explicit `false` the same zero value, so
// the question is answered by whether anything else in the otlp: block
// was written: a block an operator authored is one whose TLS posture
// they own.
func (c *Config) otlpTLSStated() bool {
	return c.OTLP.Insecure || c.OTLP.Endpoint != "" || c.OTLP.Protocol != ""
}

// validateEndpoints refuses an endpoint meerkat cannot safely use.
//
// The rule that matters: over OTLP/HTTP the endpoint is a URL, and an
// http:// one is a plaintext export of request metadata. It is allowed
// only alongside an explicit `insecure: true`, so the disclosure is
// something an operator typed rather than something a scheme implied.
func (r Resolved) validateEndpoints() error {
	for _, ep := range []string{r.Endpoint, r.TracesEndpoint, r.MetricsEndpoint} {
		if ep == "" {
			continue
		}
		if !strings.Contains(ep, "://") {
			// grpc's host:port form. Nothing to check beyond non-emptiness;
			// TLS is decided by Insecure, not by the string.
			continue
		}
		u, err := url.Parse(ep)
		if err != nil {
			return fmt.Errorf("observability.otlp.endpoint %q is not a valid URL: %w", ep, err)
		}
		switch u.Scheme {
		case "https":
		case "http":
			if !r.Insecure {
				return fmt.Errorf("observability.otlp.endpoint %q is plaintext http:// — set observability.otlp.insecure: true to accept exporting request metadata unencrypted, or use https://", ep)
			}
		default:
			return fmt.Errorf("observability.otlp.endpoint %q must be an http:// or https:// URL (or a bare host:port for grpc)", ep)
		}
	}
	return nil
}

// resolveHeaders reads OTLP headers out of the environment.
//
// headersEnv names the variable; when it is empty the standard
// OTEL_EXPORTER_OTLP_HEADERS is read instead. Either way the VALUE comes
// from the environment and never from a file — see the top of this file.
func resolveHeaders(headersEnv string) (map[string]string, error) {
	name := headersEnv
	if name == "" {
		name = envHeaders
	}
	if err := validEnvName(name); err != nil {
		return nil, fmt.Errorf("observability.otlp.headers_env: %w", err)
	}
	kv, err := parseKeyValues(os.Getenv(name))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return kv, nil
}

// validEnvName refuses anything that is not a plain environment
// variable name. headers_env is read out of the process environment, so
// the value is a NAME and nothing else — refusing the rest keeps a
// config typo from becoming a confusing empty header set.
func validEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("must name an environment variable")
	}
	for i, c := range name {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return fmt.Errorf("%q is not a valid environment variable name — it names the variable holding the headers, not the headers themselves", name)
		}
	}
	return nil
}

// samplerFromEnv reads OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG.
//
// Only the ratio-bearing samplers are honoured, because only they map
// onto a single number meerkat then wraps in ParentBased. An unknown
// sampler name is an error rather than a silent fallback: a deployment
// that asked for jaeger_remote and quietly got always_on has a sampling
// bill it did not agree to.
func samplerFromEnv() (float64, error) {
	name := strings.TrimSpace(os.Getenv(envSampler))
	if name == "" {
		return 1.0, nil
	}
	arg := strings.TrimSpace(os.Getenv(envSamplerArg))
	switch name {
	case "always_on", "parentbased_always_on":
		return 1.0, nil
	case "always_off", "parentbased_always_off":
		return 0.0, nil
	case "traceidratio", "parentbased_traceidratio":
		if arg == "" {
			return 1.0, nil
		}
		ratio, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return 0, fmt.Errorf("%s=%q is not a number", envSamplerArg, arg)
		}
		return ratio, nil
	default:
		return 0, fmt.Errorf("%s=%q is not supported — meerkat honours always_on, always_off, traceidratio and their parentbased_ forms; set observability.traces.sample_ratio instead", envSampler, name)
	}
}

// parseKeyValues parses the `k1=v1,k2=v2` form OTEL_RESOURCE_ATTRIBUTES
// and OTEL_EXPORTER_OTLP_HEADERS both use.
func parseKeyValues(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not a key=value pair", pair)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("%q has an empty key", pair)
		}
		// Values arrive percent-encoded per the OTel spec's baggage
		// grammar; tolerate both forms rather than mangling a literal %.
		if decoded, derr := url.QueryUnescape(v); derr == nil {
			v = decoded
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

func boolEnv(name string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && v
}

func durationEnv(name string) Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	// The OTel spec says these are milliseconds. A bare number is
	// therefore read as milliseconds; a Go duration string ("5s") is
	// accepted too, because that is what somebody who has read the
	// meerkat schema will type.
	if ms, err := strconv.Atoi(raw); err == nil && ms >= 0 {
		return Duration(ms) * Duration(1_000_000)
	}
	var d Duration
	if err := d.parse(raw); err != nil {
		return 0
	}
	return d
}

func first(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func firstInt(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func firstDuration(values ...Duration) Duration {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}
