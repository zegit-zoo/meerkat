# Spec: OpenTelemetry traces, OTLP export, and domain telemetry

**Status:** Implemented (`internal/telemetry`; instrumentation in `internal/mcp`, `internal/authn`, `internal/collections`, `internal/contentsource`, `internal/refresh`; wired into `mk mcp serve-http`) · **Builds on:** [hosted-mcp.md](hosted-mcp.md), [hot-reload.md](hot-reload.md) · **Issue:** #30

*(`docs/design/` is a design record. The up-to-date schema reference is
[content-source.example.yaml](../../content-source.example.yaml); the
user-facing reference is the [README](../../README.md).)*

## Summary

#9 gave the hosted server probes, bounded Prometheus metrics and
structured JSON logs. That answers *"is this replica healthy"* and *"was
that tool call slow"*. It cannot answer the question an operator
actually has when one call is slow:

> Where did the 900ms go — OIDC verification, collection routing, the
> bleve query, a GCS read, or the memory write?

This spec adds **distributed tracing**, **W3C trace-context
propagation**, **log/trace correlation**, optional **OTLP export**, and
a set of **bounded domain metrics** for content, indexing, search and
memory.

Two constraints shape all of it, and they are the same two every spec in
this directory is shaped by.

1. **A deployment that does not opt in is byte-identical.** No
   `observability:` block and no `OTEL_*` variable means no SDK is
   constructed, no exporter is built, no goroutine starts, no socket
   opens, and the process-global OpenTelemetry providers are left
   untouched. `/metrics` serves exactly the families it served before;
   the access log has exactly the fields it had before.
2. **Telemetry is never an availability dependency.** A collector that
   is down, unreachable, or answering 503 must not affect a search, a
   memory write, `/readyz`, or how long a pod takes to terminate.

## Non-goals

- **Shipping or operating a collector.** meerkat speaks OTLP. What
  receives it is the deployment's problem, exactly as Prometheus is.
- **Vendor code paths.** No Datadog, Grafana, Honeycomb, Jaeger or cloud
  tracing integration. Those all consume OTLP.
- **Logging prompts, queries, memory bodies or KB content.** Unchanged.
- **OTLP log export.** Deferred; see [Deferred](#deferred).

## The disclosure rule, stated first

Everything below follows from one decision, so it goes first.

**A span is exported OUT of the process. The access log is not.**

The access log deliberately carries `sub`, `issuer` and `tenant`,
because it is an audit trail on the operator's own stderr, inside their
own trust boundary. A span goes to a collector — frequently a shared
one, frequently a third party's — so it is held to a stricter standard
than the log beside it.

| | may appear on a span / metric | may appear in the log |
| --- | --- | --- |
| counts, durations, booleans | yes | yes |
| outcome from a closed set | yes | yes |
| matched route pattern | yes | yes |
| collection configuration **ordinal** | yes | yes |
| source generation / fingerprint | **span only** (see below) | yes |
| collection **name** | no | yes |
| page ID, memory key | no | no |
| query text, page/memory content, tags | no | no |
| bucket, object name, prefix | no | yes (log only) |
| bearer token, any OAuth claim | no | no |
| OIDC subject, email, groups, tenant | **no** | `sub`/`issuer`/`tenant` only |
| MCP session ID | no | yes |
| `r.URL.Path`, User-Agent | no | yes |

Three of those rows are worth their own sentence.

**The generation is a span attribute and not a metric label.** This is
the one place the span rule is *looser* than the metric rule, and it is
deliberate. A GCS generation increments forever; a Prometheus label
carrying one mints a permanent time series per publication, which is an
unbounded cardinality leak that outlives the deployment. A span is one
event: the same string costs one exported record and answers "which
generation did this replica pick up, and when" — which is the question.
`internal/refresh/metrics.go` refuses it; `meerkat.refresh.version`
carries it.

**Identity is not on a span, at all, by default.** Not the subject, not
a hash of it. If trace/principal correlation is ever wanted it should be
an explicitly documented privacy option using a keyed pseudonymous
identifier, and it is not this change.

**An error's text is not recorded.** meerkat's error messages are
written to be actionable, which means they quote things: `search %q`
quotes the caller's query, `collection %q` quotes a collection name, a
GCS error quotes bucket and object. So spans record a **classified
outcome** from a closed set (`telemetry.Fail`) and leave the full text
to the log. `telemetry.End`, which does record the error, is used only
where the error is known to be meerkat's own prose.

The enforcement is a test, not a promise:
`TestObservability_NoSpanOrMetricCarriesAForbiddenValue` drives every
read surface plus an ambiguity, a not-found and a scanner probe, then
walks every recorded span (name, attributes, events, status description,
resource) and every gathered metric label, failing on any of the value
classes above.

## Provider wiring

```
telemetry.New(ctx, Options{Config, Registry, Logger, Version, SpanExporter, SetGlobals})
  -> nil, nil          when nothing was configured
  -> *Telemetry        tracer + domain metric recorder + bounded batch pipeline
```

The hosted server holds one `*telemetry.Telemetry`, and **a nil one is a
working implementation of "off"** — the same idiom
`refresh.Options`/`refresh.New` established in #28, where `New` returns
`nil` for an empty target set and every method tolerates one.

### How instrumentation reaches the leaf packages

Through the **request context**, exactly as authorization does
(`authz.FromContext`):

```go
telemetry.NewContext(ctx, tel)     // installed once, by the trace middleware
telemetry.FromContext(ctx)         // nil when nothing was installed
telemetry.Span(ctx, name, attrs…)  // the leaf-package entry point
telemetry.Record(ctx)              // the domain metric recorder, nil-safe
```

`Span` on an uninstrumented context returns **the caller's own context**
and a non-recording span: no derived context, no allocation, no
measurable cost. That is what makes it safe to call from paths the CLI
also walks (`collections.Registry.Search`, `contentsource.FetchGCS`,
`memory` writes), and it is why no signature below `internal/mcp` grew a
provider parameter.

The alternatives were both worse. A package-level default — or otel's
own global — would make two hosted servers in one test binary share one
tracer and one set of domain counters. Threading a provider through
every leaf would have changed a dozen public shapes for a feature that
is off by default.

`SetGlobals` is the one exception, and it is opt-in per server: `mk mcp
serve-http` sets it, because that process runs exactly one server, and
it is what makes the Google Cloud Storage client's own OpenTelemetry
instrumentation join meerkat's traces instead of emitting nowhere. A
test binary sets neither.

### Middleware order

```
traceHTTP( accessLog( instrumentHTTP( mux ) ) )
```

The root span is created **outermost**, above the access log, for two
reasons. The access log can then read the trace it belongs to straight
out of its own request context — no repeat of the `identityHolder`
trick #9 needed to carry a value back *up* the stack. And a `401`
refused by the authentication gate is still a complete trace, which is
the line an operator most wants.

With telemetry off, `traceHTTP` returns its argument **unchanged** —
not a pass-through wrapper — so there is not even an extra stack frame.

## Span taxonomy

Semantic conventions where they exist (HTTP server and client spans:
`http.request.method`, `http.route`, `http.response.status_code`,
`server.address`, `url.scheme`), a stable documented `meerkat.*`
namespace where they do not. Every key is listed in
`internal/telemetry/attrs.go`; changing one is a breaking change to
somebody's dashboard.

| span | created in | notable attributes |
| --- | --- | --- |
| `{METHOD} {route}` (root, server) | `internal/mcp` traceHTTP | `http.request.method`, `http.route`, `http.response.status_code` |
| `meerkat.authn.verify` | `internal/authn` gate | `meerkat.authn.result`, `meerkat.authn.providers` |
| `meerkat.authz.decide` | `internal/authn` gate | `meerkat.authz.granted`, `meerkat.authz.collections`, `meerkat.authz.rules` |
| `meerkat.mcp.tool` | `internal/mcp` tool middleware | `meerkat.mcp.tool`, `meerkat.mcp.method`, `meerkat.mcp.request_bytes`, `meerkat.mcp.response_bytes`, `meerkat.outcome` |
| `meerkat.search` | `mk_search` handler | `meerkat.search.query_length`, `.limit`, `.results`, `meerkat.collection.count` |
| `meerkat.search.collection` | `collections.Registry.Search` | `meerkat.search.filtered`, `meerkat.collection.count` |
| `meerkat.show` | `mk_show` handler | `meerkat.collection.qualified`, `.named`, `.count` |
| `meerkat.list` | `mk_list` handler | `meerkat.pages.matched`, `meerkat.pages.returned` |
| `meerkat.list_collections` | `mk_list_collections` handler | `meerkat.collection.count` |
| `meerkat.index.build` | `collections` (mount + reload) | `meerkat.index.pages` |
| `meerkat.source.resolve` | `contentsource.resolveSource` | `meerkat.source.type` |
| `meerkat.gcs` | `contentsource` GCS calls | `meerkat.gcs.operation`, `meerkat.source.objects` |
| `meerkat.memory.save` / `.stage` | `internal/mcp` memory tool | `meerkat.memory.scope`, `.bytes`, `meerkat.outcome` |
| `meerkat.memory.store` | `collections` store calls | `meerkat.memory.backend`, `.operation` |
| `meerkat.refresh.cycle` | `refresh.Controller.runOnce` | `meerkat.collection.ordinal`, `.kind`, `.changed`, `.version`, `.policy` |
| `meerkat.refresh.{probe,resolve,mount,enumerate,build,commit}` | `collections` reload path | durations and counts |
| `meerkat.readiness` | `/readyz` | `meerkat.ready`, `.collections.ready`, `.collections.degraded`, `meerkat.index.pages` |
| `HTTP {METHOD}` (client) | `telemetry.HTTPClient` | method, `server.address`, `url.scheme` — never path or query |

Two shaping decisions inside that table.

**The tool span is created in the shared middleware**, beside the
existing Prometheus counter, not in each handler — so a tool added later
is instrumented by existing, exactly as it is counted by existing. Its
name is clamped through `telemetry.BoundedTool` to the registered set;
an unrecognised name reports `other`. mcp-go only dispatches to
registered handlers today, but "already bounded in practice" is the
assumption that becomes a cardinality incident when a later transport
dispatches differently.

**The refresh cycle span is created in `runOnce`**, the single funnel
every reconciliation goes through — scheduled loop and `SIGHUP` admin
trigger alike. There is no second path that could produce an untraced
refresh, for the same reason there is no second path that could skip the
staging discipline.

## Context propagation

- **Inbound.** W3C `traceparent` is extracted from every request. A
  malformed header yields an invalid parent span context, which is the
  same thing as none: the request is served identically and a fresh root
  trace is started. A caller cannot change a response, or suppress
  instrumentation, with a broken header.
- **Outbound.** OIDC discovery and JWKS fetches go through
  `Telemetry.HTTPClient`, which injects `traceparent` and emits a client
  span — so "the IdP took 400ms" and "meerkat took 400ms" stop looking
  alike. The client span records method, scheme and host, never path or
  query.
- **Baggage is not propagated in either direction.** meerkat never reads
  it (trace context is correlation data, never authorization data), and
  forwarding a caller's arbitrary baggage onto meerkat's outbound calls
  would push their key/value pairs to the identity provider. Not
  carrying it is a stronger statement than carrying it carefully.
- **Sampling is parent-based.** `sample_ratio` governs traces this
  process *starts*. A trace a gateway already sampled is not re-sampled
  here, because half a trace is worse than none.

## Configuration and precedence

```yaml
observability:
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
    protocol: grpc              # grpc | http/protobuf
    insecure: false
    headers_env: OTEL_EXPORTER_OTLP_HEADERS

  limits:
    queue_size: 2048
    batch_size: 512
    batch_timeout: 5s
    shutdown_timeout: 5s
```

### The rule

```
explicit meerkat configuration  >  OTEL_* environment  >  default
```

A field **written** in the block wins. A field **left out** falls back
to the standard OpenTelemetry variable for that setting, then to
meerkat's default. The fallback is per **field**, not per block: a file
that sets only `traces.enabled: true` still picks its endpoint up from
`OTEL_EXPORTER_OTLP_ENDPOINT`.

The direction makes both audiences right. The file is the artifact under
review — an operator who wrote `sample_ratio: 0.1` and reads the
deployment back should see `0.1`, not whatever a base image exported; a
file that ambient environment can silently override is a file you cannot
audit. The environment is how a platform injects what nobody wants in a
repository: a per-cluster collector address, and above all credentials.

Recognised variables: `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`,
`OTEL_EXPORTER_OTLP_ENDPOINT` (+ `_TRACES_`/`_METRICS_` variants),
`OTEL_EXPORTER_OTLP_PROTOCOL`, `_INSECURE`, `_HEADERS`, `_TIMEOUT`,
`OTEL_TRACES_SAMPLER` / `_ARG`, `OTEL_METRIC_EXPORT_INTERVAL`,
`OTEL_SDK_DISABLED`, and meerkat's own `MEERKAT_TRACES_ENABLED` (there
is no standard variable for "does the application create spans";
`OTEL_SDK_DISABLED` is only an off switch, and a container with no
config file still needs a way in).

### Two deliberate inversions

**`OTEL_SDK_DISABLED=true` beats an explicit `traces.enabled: true`.** An
off switch that a config file can override is not an off switch. An
operator killing telemetry fleet-wide during an incident must not have
to find every file that opted in.

**A stated TLS posture is not revoked by the environment.** If the file
wrote an `otlp:` block at all, its author owns that block's transport
security, and `OTEL_EXPORTER_OTLP_INSECURE` does not apply. With no
`otlp:` block the file has stated nothing and the variable is the only
statement there is.

All of it is tested, one test per row, in
`internal/telemetry/config_test.go`.

### Credentials are named, never written

There is no `headers:` field. There is `headers_env:`, which names an
environment variable whose value is read at startup. A
`content-source.yaml` is committed, shared between environments and
frequently baked into an image; a bearer token for a hosted collector
has no business in one. The indirection makes the wrong thing
**unwriteable** rather than discouraged — the same reasoning that gives
`type: gcs` no service-account-key field. A test asserts the schema
marshals no `headers:`/`token:`/`api_key:`/`password:` key, so a field
added later without thinking about this trips it.

`headers_env` must be a plain environment-variable name; pasting the
headers into it (which would put the token in the file *and* send no
headers) is refused at config load.

### Validation runs at load

`observability:` is validated in `parseConfig`, beside `auth:`, so a
plaintext `http://` endpoint with no explicit `insecure: true`, an
unsupported protocol, a sample ratio outside `[0,1]`, an unsupported
`OTEL_TRACES_SAMPLER`, or a malformed `headers_env` all fail the process
where an operator is looking at the file — rather than producing a
server that exports nothing and says so nowhere.

## Metrics

Every existing series is unchanged. Added, all bounded by the same label
discipline (`internal/telemetry/metrics.go`):

```
meerkat_index_builds_total{outcome}
meerkat_index_build_duration_seconds{outcome}
meerkat_index_pages
meerkat_source_resolves_total{type,outcome}          # type: embedded|local|url|gcs-object|gcs-prefix
meerkat_source_resolve_duration_seconds{type}
meerkat_source_cache_total{type,result}              # result: hit|miss
meerkat_source_downloaded_bytes_total{type}
meerkat_search_total{outcome}                        # ok|invalid_query|timeout|error
meerkat_search_duration_seconds{outcome}
meerkat_search_results                               # a histogram of result COUNTS
meerkat_show_ambiguous_total
meerkat_memory_saves_total{scope,outcome}            # personal|team|global × saved|staged|conflict|error
meerkat_memory_backend_duration_seconds{backend,operation}
meerkat_memory_backend_errors_total{backend,operation}
meerkat_mcp_tool_payload_bytes{tool,direction}       # coarse buckets, 256B–4MB
meerkat_otel_export_failures_total{signal}
meerkat_otel_spans_dropped_total
```

Every label value in that list comes from a constant in `attrs.go`.
There is no `WithLabelValues` call in the package whose argument could
be caller-supplied text, which is what makes the cardinality bound
structural rather than a promise.

`meerkat_index_pages` is a **total** across mounted collections, not a
per-collection gauge — the same reason `meerkat_collections_ready` is a
count. It rides the readiness sweep, because that is where the pages
were just enumerated.

With `metrics.prometheus: false` the collectors still record and are
simply not registered; `/metrics` keeps every series it had. Turning
observability on cannot cost a deployment its dashboards.

## Export and failure behaviour

`internal/telemetry/export.go` is a bounded, non-blocking
`sdktrace.SpanProcessor`. The SDK ships one that does most of this;
meerkat has its own because the parts that matter — how many spans were
dropped, whether the queue bound holds, what a failing exporter does —
are not observable from outside the SDK's, so they could be documented
but not tested.

| promise | mechanism | test |
| --- | --- | --- |
| bounded memory | buffered channel of exactly `queue_size`; a full queue **drops and counts**, never blocks the goroutine serving the request | `TestBatchProcessor_FullQueueDropsAndCountsRatherThanGrowing` |
| visible failure | `meerkat_otel_export_failures_total` + one log line per minute | `…ExportFailureIsCountedAndNeverPropagates`, `…LoggingIsRateLimited` |
| bounded shutdown | final flush gets `shutdown_timeout`, then the process continues | `…ShutdownIsBoundedWhenTheExporterHangs` |
| no retry queue | a failed batch is dropped | (a retry queue is an unbounded one wearing a disguise) |

Startup never waits on a collector: OTLP/gRPC dials lazily, and
`telemetry.New` against an address nothing is listening on returns
promptly. Exporter lifecycle events (`telemetry started`,
`telemetry stopped`, export failures) go to the structured logger on
**stderr** — stdout is the stdio MCP transport's wire and nothing here
may touch it. `otel.SetErrorHandler` routes the SDK's own internal
errors to the same place, rate-limited and counted, instead of the
standard logger.

Shutdown order in the hosted server: drain HTTP → close MCP sessions →
flush telemetry, with the flush's error logged and discarded. A
collector that is already gone must not turn a clean shutdown into a
failed one.

## Logs

The access and auth logs are unchanged in every field they had, and gain
`trace_id` / `span_id` when a span is active. That is the whole of the
bridge between the two surfaces and it goes **one way**: IDs come into
the log, and nothing goes from the log into the span.
`logs.include_trace_context: false` keeps the old shape for a deployment
whose log pipeline has a fixed schema.

## Zero-configuration invariance, and how it is proven

Four assertions, in `internal/mcp/observability_test.go` and
`internal/telemetry/telemetry_test.go`:

1. `telemetry.New` with no config and no environment returns
   **`(nil, nil)`** — no SDK, no exporter, no goroutine, no socket.
2. A `HostedServer` built with no `Observability` holds `tel == nil`,
   and `traceHTTP` returns the handler it was given rather than a
   wrapper.
3. `/metrics` on such a server publishes **no family outside the
   pre-#30 set**. (The assertion is "nothing new", because Prometheus
   omits a `*Vec` with no children, so which subset appears depends on
   traffic.)
4. The process-global OpenTelemetry tracer provider is **unchanged** by
   building one, and the access log carries no `trace_id`/`span_id`.

Below that, the property is structural: with no `*Telemetry` in the
request context, every `telemetry.Span` call in every leaf package
returns the caller's own context and a non-recording span.

## Testing strategy

- **In-memory exporters only** (`sdk/trace/tracetest`), injected through
  `HostedConfig.Telemetry` / `telemetry.Options.SpanExporter`. No test
  in this repo needs a collector, a socket or a name resolution.
- **End to end over the wire** (`internal/mcp/observability_test.go`): a
  real mcp-go client against a real hosted server — one trace from the
  root HTTP span through the MCP tool span to the search span, the
  `tool_error`/`error` split, trace-context continuation, five shapes of
  malformed context, log correlation on both the access and the denial
  line, `/metrics` back-compat, an exporter outage, `sample_ratio: 0`,
  twelve concurrent sessions producing twelve independent traces, memory
  save outcomes, and the attribute-hygiene sweep.
- **The pipeline** (`internal/telemetry/export_test.go`): queue bounds
  under a wedged exporter, export failure, log rate limiting, bounded
  shutdown, unsampled spans never queued.
- **Precedence** (`internal/telemetry/config_test.go`): one test per row
  of the table above, plus both inversions and the credential rule.
- **The refresh cycle** (`internal/refresh/telemetry_test.go`): ordinal
  and kind present, name absent, version present (the deliberate
  span/label asymmetry), every outcome classified, error text not
  recorded.

## Deferred

- **OTLP log export.** Traces and metrics export; logs stay on stderr as
  JSON. The correlation acceptance criterion (`trace_id`/`span_id` in
  the access log) is met, and shipping the logs themselves needs the
  OTel log SDK plus an slog bridge — a materially larger dependency and
  test surface for a signal most deployments already collect from
  stderr. The `logs:` config block is present and honoured for level,
  format and correlation.
- **Per-collection operational spans.** A search fans out across
  collections, and the only per-collection thing a span could add is the
  collection's name, which may not be exported. Per-collection detail
  stays in authenticated `mk_list_collections` output and in the log.
- **`memory.Store.Stat` timing.** Instrumented at save/stage/load/
  fingerprint; the `Stat` call inside the optimistic-locking
  precondition is not separately timed.
- **Trace/principal correlation.** Deliberately absent, and if ever
  added should be an explicit privacy option with a keyed pseudonymous
  identifier rather than a subject.
