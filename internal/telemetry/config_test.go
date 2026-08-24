package telemetry

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// config_test.go pins the PRECEDENCE CONTRACT, which is the part of this
// package an operator has to be able to predict from the file in front
// of them:
//
//	explicit meerkat configuration  >  OTEL_* environment  >  default
//
// Every test below is one row of that table, plus the two deliberate
// inversions (OTEL_SDK_DISABLED beats an explicit enable; a stated TLS
// posture is not revoked by ambient environment) and the rule that
// credentials are named, never written.

// otelEnv is every environment variable Resolve consults. Tests clear
// the lot before setting the one they mean, so a developer machine that
// exports OTEL_EXPORTER_OTLP_ENDPOINT cannot make them pass or fail for
// the wrong reason.
var otelEnv = []string{
	envServiceName, envResourceAttributes, envEndpoint, envTracesEndpoint,
	envMetricsEndpoint, envProtocol, envInsecure, envHeaders, envTimeout,
	envSampler, envSamplerArg, envSDKDisabled, envMetricInterval, envTracesEnabled,
}

func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, name := range otelEnv {
		// t.Setenv first, purely to register the restore; then actually
		// UNSET. Set-but-empty is a different state from absent as far as
		// the OpenTelemetry SDK's own env parsing is concerned — it warns
		// about an empty OTEL_TRACES_SAMPLER — and "absent" is the state a
		// clean deployment is in, so it is the one to test against.
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

func ptrBool(v bool) *bool        { return &v }
func ptrFloat(v float64) *float64 { return &v }
func resolveOK(t *testing.T, cfg *Config) Resolved {
	t.Helper()
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return r
}

func TestResolve_NothingConfiguredIsInactive(t *testing.T) {
	clearOTelEnv(t)
	r := resolveOK(t, nil)
	if r.Active() {
		t.Fatal("no configuration and no environment must resolve to inactive telemetry — " +
			"an Active() resolution is what makes New build an SDK, and a deployment that " +
			"asked for nothing must get none")
	}
	if r.Exports() {
		t.Fatal("nothing configured must not resolve an export endpoint")
	}
}

func TestResolve_EmptyBlockIsStillInactive(t *testing.T) {
	clearOTelEnv(t)
	// An `observability:` key with nothing under it is a file an operator
	// started writing and did not finish. It must not silently start an
	// exporter.
	if r := resolveOK(t, &Config{}); r.Active() {
		t.Fatal("an empty observability: block must not activate telemetry")
	}
}

func TestResolve_ExplicitConfigurationBeatsTheEnvironment(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envServiceName, "from-env")
	t.Setenv(envEndpoint, "env-collector:4317")
	t.Setenv(envProtocol, ProtocolHTTP)
	t.Setenv(envSampler, "traceidratio")
	t.Setenv(envSamplerArg, "0.99")

	r := resolveOK(t, &Config{
		ServiceName: "from-file",
		Traces:      TraceConfig{Enabled: true, SampleRatio: ptrFloat(0.25)},
		OTLP:        OTLPConfig{Endpoint: "file-collector:4317", Protocol: ProtocolGRPC},
	})

	if r.ServiceName != "from-file" {
		t.Errorf("service name: got %q, want the file's value — the file is the artifact under review", r.ServiceName)
	}
	if r.Endpoint != "file-collector:4317" {
		t.Errorf("endpoint: got %q, want the file's value", r.Endpoint)
	}
	if r.Protocol != ProtocolGRPC {
		t.Errorf("protocol: got %q, want the file's value", r.Protocol)
	}
	if r.SampleRatio != 0.25 {
		t.Errorf("sample_ratio: got %v, want 0.25 — an operator who wrote a ratio must see it applied", r.SampleRatio)
	}
}

func TestResolve_EnvironmentFillsFieldsTheFileLeftOut(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envServiceName, "from-env")
	t.Setenv(envEndpoint, "env-collector:4317")
	t.Setenv(envSampler, "traceidratio")
	t.Setenv(envSamplerArg, "0.1")

	// The file turns tracing on and says nothing else. That is how an
	// operator delegates the address and the sampling to the platform.
	r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}})

	if r.ServiceName != "from-env" {
		t.Errorf("service name: got %q, want the environment's value", r.ServiceName)
	}
	if r.Endpoint != "env-collector:4317" {
		t.Errorf("endpoint: got %q, want the environment's value", r.Endpoint)
	}
	if r.SampleRatio != 0.1 {
		t.Errorf("sample_ratio: got %v, want 0.1 from OTEL_TRACES_SAMPLER_ARG", r.SampleRatio)
	}
	if !r.Exports() {
		t.Error("an endpoint from the environment must still count as exporting")
	}
}

func TestResolve_FallbackIsPerFieldNotPerBlock(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envEndpoint, "env-collector:4317")
	// The file writes one field of otlp: and leaves the endpoint out. The
	// endpoint must still come from the environment — the fallback is per
	// field, not "the file wrote an otlp: block so the environment is
	// ignored".
	r := resolveOK(t, &Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Protocol: ProtocolHTTP},
	})
	if r.Endpoint != "env-collector:4317" {
		t.Errorf("endpoint: got %q, want the environment's — fallback is per-field", r.Endpoint)
	}
	if r.Protocol != ProtocolHTTP {
		t.Errorf("protocol: got %q, want the file's", r.Protocol)
	}
}

func TestResolve_DefaultsApplyWhenNeitherSaysAnything(t *testing.T) {
	clearOTelEnv(t)
	r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}})
	if r.ServiceName != defaultServiceName {
		t.Errorf("service name: got %q, want %q", r.ServiceName, defaultServiceName)
	}
	if r.Protocol != ProtocolGRPC {
		t.Errorf("protocol: got %q, want %q", r.Protocol, ProtocolGRPC)
	}
	if r.SampleRatio != 1.0 {
		t.Errorf("sample_ratio: got %v, want 1.0 — traces.enabled is already the off switch, "+
			"so an operator who typed only that wants to see traces, not a tenth of them", r.SampleRatio)
	}
	if r.Insecure {
		t.Error("TLS must be on by default: a plaintext export of request metadata is something an operator types, not something they inherit")
	}
	if !r.PrometheusMetrics {
		t.Error("Prometheus must stay on by default — an observability opt-in must not cost a deployment its existing dashboards")
	}
	if !r.IncludeTraceContext {
		t.Error("log/trace correlation must be on by default when tracing is: it is the point of turning tracing on")
	}
}

func TestResolve_SDKDisabledBeatsAnExplicitEnable(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envSDKDisabled, "true")
	// This is the ONE inversion of the precedence rule, and it is
	// deliberate: an off switch a config file can override is not an off
	// switch. An operator killing telemetry fleet-wide during an incident
	// must not have to find every file that opted in.
	r := resolveOK(t, &Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Endpoint: "collector:4317"},
	})
	if r.Active() {
		t.Fatal("OTEL_SDK_DISABLED=true must win over an explicit traces.enabled: true")
	}
}

func TestResolve_StatedTLSPostureIsNotRevokedByEnvironment(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envInsecure, "true")
	// The file wrote an otlp: block, so its TLS posture is stated —
	// `insecure:` absent means false, and ambient environment must not
	// quietly turn encryption off for a deployment whose author looked at
	// that block.
	r := resolveOK(t, &Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Endpoint: "collector.observability.svc:4317"},
	})
	if r.Insecure {
		t.Fatal("OTEL_EXPORTER_OTLP_INSECURE must not override a file that stated its otlp: configuration")
	}

	// With no otlp: block at all the file has stated nothing, so the
	// platform's variable is how the address AND its transport arrive.
	r2 := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}})
	if !r2.Insecure {
		t.Fatal("with no otlp: block, OTEL_EXPORTER_OTLP_INSECURE is the only statement there is and must apply")
	}
}

func TestResolve_PlaintextHTTPEndpointRequiresExplicitInsecure(t *testing.T) {
	clearOTelEnv(t)
	_, err := Resolve(&Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Endpoint: "http://collector:4318", Protocol: ProtocolHTTP},
	})
	if err == nil {
		t.Fatal("an http:// collector endpoint without insecure: true must be refused — " +
			"exporting request metadata unencrypted is a disclosure an operator should have to type")
	}
	if !strings.Contains(err.Error(), "insecure") {
		t.Errorf("the error must name the setting that would accept it, got: %v", err)
	}

	// Written out, it is accepted.
	r := resolveOK(t, &Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Endpoint: "http://collector:4318", Protocol: ProtocolHTTP, Insecure: true},
	})
	if !r.Insecure {
		t.Error("insecure: true must resolve as stated")
	}
}

func TestResolve_RejectsANonHTTPEndpointScheme(t *testing.T) {
	clearOTelEnv(t)
	if _, err := Resolve(&Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Endpoint: "gopher://collector:4318"},
	}); err == nil {
		t.Fatal("an endpoint with an unrecognised scheme must be refused rather than silently dialled")
	}
}

func TestResolve_HeadersAreNamedNeverWritten(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("MY_COLLECTOR_HEADERS", "authorization=Bearer%20abc,x-tenant=acme")

	r := resolveOK(t, &Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Endpoint: "collector:4317", HeadersEnv: "MY_COLLECTOR_HEADERS"},
	})
	if got := r.Headers["authorization"]; got != "Bearer abc" {
		t.Errorf("headers[authorization] = %q, want the percent-decoded value from the named variable", got)
	}
	if got := r.Headers["x-tenant"]; got != "acme" {
		t.Errorf("headers[x-tenant] = %q, want acme", got)
	}
}

func TestResolve_HeadersFallBackToTheStandardVariable(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envHeaders, "x-scope-orgid=team-a")
	r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}, OTLP: OTLPConfig{Endpoint: "c:4317"}})
	if got := r.Headers["x-scope-orgid"]; got != "team-a" {
		t.Errorf("headers: got %v, want the value of %s when headers_env is unset", r.Headers, envHeaders)
	}
}

func TestResolve_HeadersEnvMustNameAVariableNotCarryAValue(t *testing.T) {
	clearOTelEnv(t)
	// The failure this prevents is an operator pasting the HEADERS into
	// headers_env — which would put a bearer token in a committed file
	// and silently send no headers at all.
	_, err := Resolve(&Config{
		Traces: TraceConfig{Enabled: true},
		OTLP:   OTLPConfig{Endpoint: "c:4317", HeadersEnv: "authorization=Bearer abc"},
	})
	if err == nil {
		t.Fatal("headers_env carrying a value rather than a variable name must be refused")
	}
	if !strings.Contains(err.Error(), "environment variable") {
		t.Errorf("the error must explain that the field names a variable, got: %v", err)
	}
}

func TestConfig_HasNoFieldThatCouldHoldASecret(t *testing.T) {
	// A structural assertion, not a behavioural one: the schema must not
	// GAIN a literal `headers:` (or `token:`, or `api_key:`) later. YAML
	// marshalling an empty Config lists exactly the keys the schema
	// offers, so a field added without thinking about this shows up here.
	body, err := yaml.Marshal(&Config{
		OTLP: OTLPConfig{Endpoint: "c:4317", HeadersEnv: "X"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"headers:", "token:", "api_key:", "password:", "credential"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("the observability: schema must have no %q field — credentials come from the "+
				"environment via headers_env, so that the wrong thing is unwriteable rather than "+
				"merely discouraged. Got:\n%s", forbidden, body)
		}
	}
}

func TestResolve_RejectsASampleRatioOutsideZeroToOne(t *testing.T) {
	clearOTelEnv(t)
	for _, ratio := range []float64{-0.1, 1.5} {
		if _, err := Resolve(&Config{Traces: TraceConfig{Enabled: true, SampleRatio: ptrFloat(ratio)}}); err == nil {
			t.Errorf("sample_ratio %v must be refused", ratio)
		}
	}
}

func TestResolve_RejectsAnUnsupportedSampler(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envSampler, "jaeger_remote")
	// Silently falling back to always_on would hand a deployment a
	// sampling bill it did not agree to.
	if _, err := Resolve(&Config{Traces: TraceConfig{Enabled: true}}); err == nil {
		t.Fatal("an unsupported OTEL_TRACES_SAMPLER must be refused, not silently defaulted")
	}
}

func TestResolve_SamplerAliases(t *testing.T) {
	cases := map[string]float64{
		"always_on":                1.0,
		"always_off":               0.0,
		"parentbased_always_off":   0.0,
		"parentbased_traceidratio": 0.5,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			clearOTelEnv(t)
			t.Setenv(envSampler, name)
			t.Setenv(envSamplerArg, "0.5")
			r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}})
			if r.SampleRatio != want {
				t.Errorf("%s: got %v, want %v", name, r.SampleRatio, want)
			}
		})
	}
}

func TestResolve_MeerkatEnvVariableEnablesTracesWithNoFile(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envTracesEnabled, "true")
	// A container image with no content-source.yaml still needs a way in.
	r := resolveOK(t, nil)
	if !r.TracesEnabled || !r.Active() {
		t.Fatalf("%s=true must enable tracing with no configuration file at all", envTracesEnabled)
	}
}

func TestResolve_PrometheusCanBeTurnedOffWithoutTurningMetricsOff(t *testing.T) {
	clearOTelEnv(t)
	r := resolveOK(t, &Config{
		Traces:  TraceConfig{Enabled: true},
		Metrics: MetricConfig{Prometheus: ptrBool(false)},
	})
	if r.PrometheusMetrics {
		t.Fatal("metrics.prometheus: false must resolve as stated")
	}
}

func TestResolve_MetricIntervalFromEnvironmentIsMilliseconds(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv(envMetricInterval, "15000")
	r := resolveOK(t, &Config{Traces: TraceConfig{Enabled: true}})
	if got := r.MetricInterval.Duration().String(); got != "15s" {
		t.Errorf("OTEL_METRIC_EXPORT_INTERVAL=15000 must be read as 15s per the OTel spec, got %s", got)
	}
}

func TestDuration_RequiresAUnit(t *testing.T) {
	// A bare number decoded into a plain time.Duration would be
	// NANOSECONDS: `timeout: 5` would mean an export timeout that expires
	// before the connection is made. Refusing it is the whole reason this
	// type exists (and internal/refresh's twin).
	var cfg Config
	err := yaml.Unmarshal([]byte("otlp:\n  timeout: 5\n"), &cfg)
	if err == nil {
		t.Fatal("a bare number must be refused as a duration")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Errorf("the error must name the missing unit, got: %v", err)
	}

	if err := yaml.Unmarshal([]byte("otlp:\n  timeout: 5s\n"), &cfg); err != nil {
		t.Fatalf("a unit-bearing duration must parse: %v", err)
	}
	if cfg.OTLP.Timeout.Duration().String() != "5s" {
		t.Errorf("timeout: got %s, want 5s", cfg.OTLP.Timeout)
	}
}

func TestConfig_ParsesTheDocumentedBlock(t *testing.T) {
	// The exact YAML from the issue and from content-source.example.yaml,
	// so a documentation drift shows up as a test failure.
	const doc = `
service_name: meerkat
environment: production
logs:
  level: info
  format: json
  include_trace_context: true
metrics:
  prometheus: true
  otlp: false
traces:
  enabled: true
  sample_ratio: 0.10
otlp:
  endpoint: otel-collector.observability.svc:4317
  protocol: grpc
  insecure: false
  headers_env: OTEL_EXPORTER_OTLP_HEADERS
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("the documented observability: block must parse: %v", err)
	}
	if cfg.ServiceName != "meerkat" || cfg.Environment != "production" {
		t.Errorf("resource fields: %+v", cfg)
	}
	if !cfg.Traces.Enabled || cfg.Traces.SampleRatio == nil || *cfg.Traces.SampleRatio != 0.10 {
		t.Errorf("traces: %+v", cfg.Traces)
	}
	if cfg.OTLP.Endpoint == "" || cfg.OTLP.Protocol != ProtocolGRPC || cfg.OTLP.Insecure {
		t.Errorf("otlp: %+v", cfg.OTLP)
	}
	if cfg.Metrics.Prometheus == nil || !*cfg.Metrics.Prometheus || cfg.Metrics.OTLP {
		t.Errorf("metrics: %+v", cfg.Metrics)
	}
}
