package telemetry

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics.go is the bounded DOMAIN telemetry: index health, source
// resolution, cache behaviour, search shape, memory outcomes, and the
// exporter's own health.
//
// # Label discipline, inherited verbatim
//
// The rules internal/mcp/metrics.go and internal/refresh/metrics.go
// state apply here unchanged, because /metrics is the same
// unauthenticated endpoint:
//
//	NOT a label:  collection name, page ID, query text, memory key,
//	              bucket, object path, source generation or fingerprint,
//	              subject, session ID, request path, error text.
//	IS a label:   an outcome from a closed set, a source TYPE from a
//	              closed set of five, a memory scope from a closed set of
//	              three, a backend from a closed set of two, and a tool
//	              name clamped to the registered set.
//
// Every label value in this file comes from a constant in attrs.go.
// There is no WithLabelValues call anywhere below whose argument could
// be caller-supplied text, which is what makes the cardinality bound
// structural rather than a promise.
//
// # Why they live here and not in internal/mcp
//
// These are emitted from internal/search, internal/memory,
// internal/contentsource and internal/collections — packages the hosted
// server's own metrics struct is not reachable from without threading a
// handle through half the codebase. They ride the request context
// instead (see Record), which is also what keeps two hosted servers in
// one test binary from sharing counters.
//
// # Nil is a working recorder
//
// Every method tolerates a nil *Metrics. That is the disabled path: a
// process with no observability configuration has no *Telemetry in its
// contexts, so Record returns nil and each call below is a nil check
// and a return.
type Metrics struct {
	indexBuilds      *prometheus.CounterVec
	indexDuration    *prometheus.HistogramVec
	indexPages       prometheus.Gauge
	treeDepth        prometheus.Gauge
	outcomes         *prometheus.CounterVec
	cacheResident    prometheus.Gauge
	cacheFill        prometheus.Gauge
	cacheCollections prometheus.Gauge
	cacheCulls       *prometheus.CounterVec
	mounts           *prometheus.CounterVec
	mountDuration    *prometheus.HistogramVec
	pathTemperature  prometheus.Histogram

	retrievalFirstContext  *prometheus.HistogramVec
	retrievalFirstRelevant *prometheus.HistogramVec
	retrievalGiveUp        prometheus.Histogram
	retrievalHops          prometheus.Histogram
	retrievalSteps         prometheus.Histogram
	retrievalWrongTurns    prometheus.Histogram
	retrievalSessions      *prometheus.CounterVec
	retrievalAccuracy      *prometheus.HistogramVec
	retrievalCompleteness  *prometheus.HistogramVec
	retrievalAnswerQuality *prometheus.HistogramVec
	retrievalLimitReached  *prometheus.CounterVec
	sourceResolves         *prometheus.CounterVec
	sourceDuration         *prometheus.HistogramVec
	sourceCache            *prometheus.CounterVec
	sourceBytes            *prometheus.CounterVec
	searches               *prometheus.CounterVec
	searchDuration         *prometheus.HistogramVec
	searchResults          prometheus.Histogram
	ambiguous              prometheus.Counter
	memorySaves            *prometheus.CounterVec
	memoryDuration         *prometheus.HistogramVec
	memoryErrors           *prometheus.CounterVec
	toolPayload            *prometheus.HistogramVec
	exportFailures         *prometheus.CounterVec
	exportSpansDrop        prometheus.Counter
}

// Bucket sets. Named so the choice behind each is reviewable.
var (
	// indexBuckets span a rebuild: milliseconds for a handful of pages,
	// tens of seconds for a large collection. The Prometheus defaults top
	// out at 10s and would collapse every real rebuild into +Inf.
	indexBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	// sourceBuckets span a resolve: a cache hit is sub-millisecond, a
	// cold prefix mount of a few hundred objects is minutes.
	sourceBuckets = []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60, 300}
	// resultBuckets are RESULT COUNTS, not IDs. The point of the
	// histogram is "how many hits does a typical query get" — a
	// distribution that answers whether a limit is set sensibly — and it
	// is coarse because the interesting distinctions are none/few/many.
	resultBuckets = []float64{0, 1, 2, 5, 10, 25, 50, 100}
	// payloadBuckets are COARSE on purpose. A tool payload size is
	// operationally interesting at order-of-magnitude resolution ("are we
	// shipping megabyte responses?") and finer buckets would only add
	// series. 256B to 4MB.
	payloadBuckets = []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304}
)

// newMetrics builds the domain collectors and registers them on reg.
//
// A nil registry produces working, UNREGISTERED collectors. That is not
// a degenerate case: it is what a deployment that wrote
// `metrics.prometheus: false` gets, and what a test that only cares
// about spans gets — the recording still happens, it simply is not
// scraped.
func newMetrics(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		indexBuilds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_index_builds_total",
			Help: "Search index builds, by outcome (ok, error).",
		}, []string{"outcome"}),
		indexDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_index_build_duration_seconds",
			Help:    "Time to build one collection's search index, including page enumeration.",
			Buckets: indexBuckets,
		}, []string{"outcome"}),
		indexPages: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_index_pages",
			Help: "Pages indexed across every mounted collection (a total, not a per-collection series).",
		}),
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_retrieval_outcomes_total",
			Help: "Retrieval sessions reported through mk_report_outcome, by outcome (found, not_found, gave_up), fallback kind (none, web, source, human) and whether the report itself was recorded (ok, error).",
		}, []string{"outcome", "fallback", "recorded"}),
		cacheResident: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_cache_resident_bytes",
			Help: "Estimated bytes held by lazily mounted knowledge bases (page bytes plus an index estimate); eager collections are not counted.",
		}),
		cacheFill: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_cache_fill_ratio",
			Help: "meerkat_cache_resident_bytes over cache.max_bytes; 0 when the budget is unbounded. Culling starts at cache.high_watermark.",
		}),
		cacheCollections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_collections_resident",
			Help: "Lazily mounted knowledge bases currently resident.",
		}),
		cacheCulls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_cache_cull_total",
			Help: "Knowledge bases culled from the cache, by reason (temperature: coldest first; age: oldest first traversal; evict: explicit).",
		}, []string{"reason"}),
		mounts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_mount_total",
			Help: "Knowledge-base mounts, by trigger (lazy, eager, warmstart) and outcome (ok, error).",
		}, []string{"trigger", "outcome"}),
		mountDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_mount_duration_seconds",
			Help:    "Time to resolve, enumerate and index a knowledge base on mount, by tree tier.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"tier"}),
		pathTemperature: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "meerkat_path_temperature",
			Help:    "Distribution of collection temperatures (traversals since mount) at each flush.",
			Buckets: []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000},
		}),
		retrievalFirstContext: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_time_to_first_context_seconds",
			Help:    "Seconds from a retrieval session's first call to its first search hit or shown page, by deepest tier reached and whether every path was already resident (hot).",
			Buckets: latencyBuckets,
		}, []string{"tier_reached", "hot"}),
		retrievalFirstRelevant: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_time_to_first_relevant_context_seconds",
			Help:    "Seconds from a retrieval session's first call to the shown page that answered it (the last show not followed by a search, confirmed by a found report), by tier reached and hot.",
			Buckets: latencyBuckets,
		}, []string{"tier_reached", "hot"}),
		retrievalGiveUp: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_time_to_give_up_seconds",
			Help:    "Seconds from a retrieval session's first call to its end when nothing was found (not_found, gave_up, timeout).",
			Buckets: latencyBuckets,
		}),
		retrievalHops: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_collection_hops",
			Help:    "Collection boundaries crossed per retrieval session.",
			Buckets: []float64{0, 1, 2, 3, 4, 5, 8, 12, 20},
		}),
		retrievalSteps: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_steps",
			Help:    "Tool calls per retrieval session.",
			Buckets: []float64{1, 2, 3, 5, 8, 12, 20, 40, 80},
		}),
		retrievalWrongTurns: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_wrong_turns",
			Help:    "Hops into a collection that was never shown from, per retrieval session.",
			Buckets: []float64{0, 1, 2, 3, 5, 8, 12},
		}),
		retrievalSessions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_retrieval_sessions_total",
			Help: "Retrieval sessions ended, by outcome (found, not_found, gave_up, timeout).",
		}, []string{"outcome"}),
		retrievalAccuracy: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_accuracy",
			Help:    "Consumer-reported accuracy (0..1) of a retrieval session's answer, by deepest tier reached.",
			Buckets: qualityBuckets,
		}, []string{"tier"}),
		retrievalCompleteness: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_completeness",
			Help:    "Consumer-reported completeness (0..1), by deepest tier reached.",
			Buckets: qualityBuckets,
		}, []string{"tier"}),
		retrievalAnswerQuality: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_retrieval_answer_quality",
			Help:    "Consumer-reported answer quality (0..1), by deepest tier reached.",
			Buckets: qualityBuckets,
		}, []string{"tier"}),
		retrievalLimitReached: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_retrieval_limit_reached_total",
			Help: "Retrieval sessions that hit a traversal limit, by limit (hops, steps, attempts).",
		}, []string{"limit"}),
		treeDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "meerkat_tree_depth",
			Help: "Deepest knowledge base declared in the tree this process serves (root = 0; 0 for a flat deployment; the hard cap is 5).",
		}),
		sourceResolves: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_source_resolves_total",
			Help: "Content-source resolutions, by bounded source type (embedded, local, url, gcs-object, gcs-prefix, s3-object, s3-prefix) and outcome.",
		}, []string{"type", "outcome"}),
		sourceDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_source_resolve_duration_seconds",
			Help:    "Time to resolve a content source to a servable directory.",
			Buckets: sourceBuckets,
		}, []string{"type"}),
		sourceCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_source_cache_total",
			Help: "Content-cache lookups, by bounded source type and result (hit, miss).",
		}, []string{"type", "result"}),
		sourceBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_source_downloaded_bytes_total",
			Help: "Bytes downloaded while resolving content sources, by bounded source type.",
		}, []string{"type"}),
		searches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_search_total",
			Help: "Search executions, by outcome (ok, invalid_query, timeout, error) and planner stage (exact, fuzzy, prefix): the fallback rate is fuzzy+prefix over the total.",
		}, []string{"outcome", "stage"}),
		searchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_search_duration_seconds",
			Help:    "Search execution latency, across every collection in the caller's view.",
			Buckets: prometheus.DefBuckets,
		}, []string{"outcome"}),
		searchResults: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "meerkat_search_results",
			Help:    "Distribution of result COUNTS per search. Counts only; result IDs are never recorded.",
			Buckets: resultBuckets,
		}),
		ambiguous: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "meerkat_show_ambiguous_total",
			Help: "mk_show lookups that matched a page ID in more than one collection in the caller's view.",
		}),
		memorySaves: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_memory_saves_total",
			Help: "Memory save attempts, by bounded scope (personal, team, global) and outcome (saved, staged, conflict, error).",
		}, []string{"scope", "outcome"}),
		memoryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_memory_backend_duration_seconds",
			Help:    "Memory store latency, by bounded backend (local, gcs, s3) and operation (save, stage, load, stat, fingerprint).",
			Buckets: prometheus.DefBuckets,
		}, []string{"backend", "operation"}),
		memoryErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_memory_backend_errors_total",
			Help: "Memory store failures, by bounded backend and operation.",
		}, []string{"backend", "operation"}),
		toolPayload: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "meerkat_mcp_tool_payload_bytes",
			Help:    "Coarse MCP tool payload sizes, by tool and direction (request, response). Sizes only; no payload is recorded.",
			Buckets: payloadBuckets,
		}, []string{"tool", "direction"}),
		exportFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "meerkat_otel_export_failures_total",
			Help: "OTLP export attempts that failed, by signal (traces, metrics). An export failure never affects serving or readiness.",
		}, []string{"signal"}),
		exportSpansDrop: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "meerkat_otel_spans_dropped_total",
			Help: "Spans discarded because the bounded export queue was full. A non-zero value means the collector is not keeping up; it is not an error in serving.",
		}),
	}
	if reg != nil {
		reg.MustRegister(
			m.indexBuilds, m.indexDuration, m.indexPages, m.treeDepth, m.outcomes,
			m.cacheResident, m.cacheFill, m.cacheCollections, m.cacheCulls, m.mounts, m.mountDuration, m.pathTemperature,
			m.retrievalFirstContext, m.retrievalFirstRelevant, m.retrievalGiveUp, m.retrievalHops, m.retrievalSteps,
			m.retrievalWrongTurns, m.retrievalSessions, m.retrievalAccuracy, m.retrievalCompleteness,
			m.retrievalAnswerQuality, m.retrievalLimitReached,
			m.sourceResolves, m.sourceDuration, m.sourceCache, m.sourceBytes,
			m.searches, m.searchDuration, m.searchResults, m.ambiguous,
			m.memorySaves, m.memoryDuration, m.memoryErrors,
			m.toolPayload, m.exportFailures, m.exportSpansDrop,
		)
	}
	return m
}

// IndexBuilt records one search index build.
func (m *Metrics) IndexBuilt(outcome string, seconds float64) {
	if m == nil {
		return
	}
	m.indexBuilds.WithLabelValues(outcome).Inc()
	m.indexDuration.WithLabelValues(outcome).Observe(seconds)
}

// SetIndexedPages publishes the total indexed page count across every
// mounted collection. A total, not a per-collection series — the same
// reason meerkat_collections_ready is a count.
func (m *Metrics) SetIndexedPages(n int) {
	if m == nil {
		return
	}
	m.indexPages.Set(float64(n))
}

// latencyBuckets span a fast hot path (tens of ms) to a slow give-up
// (minutes).
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}

// qualityBuckets cover a 0..1 score.
var qualityBuckets = []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1}

// RetrievalSession observes one ended session. Durations are seconds;
// a zero first-context or first-relevant duration means "never" and is
// not observed.
func (m *Metrics) RetrievalSession(outcome string, tier int, hot bool, hops, steps, wrongTurns int, firstContext, firstRelevant, total float64) {
	if m == nil {
		return
	}
	outcome = boundedRetrievalOutcome(outcome)
	t, h := fmt.Sprintf("%d", tier), fmt.Sprintf("%t", hot)
	m.retrievalSessions.WithLabelValues(outcome).Inc()
	m.retrievalHops.Observe(float64(hops))
	m.retrievalSteps.Observe(float64(steps))
	m.retrievalWrongTurns.Observe(float64(wrongTurns))
	if firstContext > 0 {
		m.retrievalFirstContext.WithLabelValues(t, h).Observe(firstContext)
	}
	if firstRelevant > 0 {
		m.retrievalFirstRelevant.WithLabelValues(t, h).Observe(firstRelevant)
	}
	if outcome != "found" {
		m.retrievalGiveUp.Observe(total)
	}
}

// RetrievalQuality observes the consumer-reported quality of a session
// by the deepest tier it reached.
func (m *Metrics) RetrievalQuality(tier int, accuracy, completeness, answerQuality float64) {
	if m == nil {
		return
	}
	t := fmt.Sprintf("%d", tier)
	m.retrievalAccuracy.WithLabelValues(t).Observe(accuracy)
	m.retrievalCompleteness.WithLabelValues(t).Observe(completeness)
	m.retrievalAnswerQuality.WithLabelValues(t).Observe(answerQuality)
}

// RetrievalLimitReached counts a session hitting a traversal limit.
func (m *Metrics) RetrievalLimitReached(limit string) {
	if m == nil {
		return
	}
	switch limit {
	case "hops", "steps", "attempts":
	default:
		limit = "other"
	}
	m.retrievalLimitReached.WithLabelValues(limit).Inc()
}

func boundedRetrievalOutcome(s string) string {
	switch s {
	case "found", "not_found", "gave_up", "timeout":
		return s
	}
	return "other"
}

// Mounted records one knowledge-base mount by trigger, outcome and
// tier.
func (m *Metrics) Mounted(trigger, outcome string, tier int, seconds float64) {
	if m == nil {
		return
	}
	m.mounts.WithLabelValues(boundedTrigger(trigger), outcome).Inc()
	if outcome == OutcomeOK {
		m.mountDuration.WithLabelValues(fmt.Sprintf("%d", tier)).Observe(seconds)
	}
}

// Culled counts one cache cull by reason.
func (m *Metrics) Culled(reason string) {
	if m == nil {
		return
	}
	m.cacheCulls.WithLabelValues(boundedCull(reason)).Inc()
}

// SetCache publishes the cache's resident bytes, fill ratio and
// resident collection count.
func (m *Metrics) SetCache(resident, max int64, collections int) {
	if m == nil {
		return
	}
	m.cacheResident.Set(float64(resident))
	if max > 0 {
		m.cacheFill.Set(float64(resident) / float64(max))
	} else {
		m.cacheFill.Set(0)
	}
	m.cacheCollections.Set(float64(collections))
}

// ObserveTemperature records one collection's temperature at a flush.
func (m *Metrics) ObserveTemperature(t int64) {
	if m == nil {
		return
	}
	m.pathTemperature.Observe(float64(t))
}

func boundedTrigger(s string) string {
	switch s {
	case MountLazy, MountEager, MountWarmStart:
		return s
	}
	return "other"
}

func boundedCull(s string) string {
	switch s {
	case CullTemperature, CullAge, CullEvict:
		return s
	}
	return "other"
}

// RetrievalOutcome counts one mk_report_outcome call. outcome and
// fallback are bounded to their closed sets before they become labels.
func (m *Metrics) RetrievalOutcome(outcome, fallback, recorded string) {
	if m == nil {
		return
	}
	m.outcomes.WithLabelValues(boundedOutcome(outcome), boundedFallback(fallback), recorded).Inc()
}

func boundedOutcome(s string) string {
	switch s {
	case "found", "not_found", "gave_up":
		return s
	}
	return "other"
}

func boundedFallback(s string) string {
	switch s {
	case "none", "web", "source", "human":
		return s
	}
	return "other"
}

// SetTreeDepth publishes the deepest knowledge base in the tree this
// process serves (root = 0). A depth, never a name.
func (m *Metrics) SetTreeDepth(depth int) {
	if m == nil {
		return
	}
	m.treeDepth.Set(float64(depth))
}

// SourceResolved records one content-source resolution. sourceType must
// come from attrs.go's bounded vocabulary.
func (m *Metrics) SourceResolved(sourceType, outcome string, seconds float64) {
	if m == nil {
		return
	}
	m.sourceResolves.WithLabelValues(sourceType, outcome).Inc()
	m.sourceDuration.WithLabelValues(sourceType).Observe(seconds)
}

// CacheLookup records a content-cache hit or miss.
func (m *Metrics) CacheLookup(sourceType, result string) {
	if m == nil {
		return
	}
	m.sourceCache.WithLabelValues(sourceType, result).Inc()
}

// Downloaded records bytes pulled from a remote content source.
func (m *Metrics) Downloaded(sourceType string, bytes int64) {
	if m == nil || bytes <= 0 {
		return
	}
	m.sourceBytes.WithLabelValues(sourceType).Add(float64(bytes))
}

// Searched records one search execution: how long it took, how it ended,
// and how many results came back. The COUNT, never the results.
func (m *Metrics) Searched(outcome string, seconds float64, results int, stage string) {
	if m == nil {
		return
	}
	m.searches.WithLabelValues(outcome, SearchStage(stage)).Inc()
	m.searchDuration.WithLabelValues(outcome).Observe(seconds)
	if outcome == OutcomeOK {
		m.searchResults.Observe(float64(results))
	}
}

// Ambiguous records a page ID that resolved in more than one collection.
func (m *Metrics) Ambiguous() {
	if m == nil {
		return
	}
	m.ambiguous.Inc()
}

// MemorySaved records the outcome of one mk_save_memory call.
func (m *Metrics) MemorySaved(scope, outcome string) {
	if m == nil {
		return
	}
	m.memorySaves.WithLabelValues(scope, outcome).Inc()
}

// MemoryBackend records one memory store operation.
func (m *Metrics) MemoryBackend(backend, operation string, seconds float64, failed bool) {
	if m == nil {
		return
	}
	m.memoryDuration.WithLabelValues(backend, operation).Observe(seconds)
	if failed {
		m.memoryErrors.WithLabelValues(backend, operation).Inc()
	}
}

// ToolPayload records a coarse payload size. direction is "request" or
// "response"; tool is clamped by the caller through BoundedTool.
func (m *Metrics) ToolPayload(tool, direction string, bytes int) {
	if m == nil || bytes < 0 {
		return
	}
	m.toolPayload.WithLabelValues(tool, direction).Observe(float64(bytes))
}

// ExportFailed counts one failed OTLP export. It is the local,
// always-available record of an exporter outage: the collector cannot
// tell you it is down, so this counter (and a rate-limited log line) is
// how an operator finds out.
func (m *Metrics) ExportFailed(signal string) {
	if m == nil {
		return
	}
	m.exportFailures.WithLabelValues(signal).Inc()
}

// SpansDropped counts spans discarded by the bounded queue.
func (m *Metrics) SpansDropped(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.exportSpansDrop.Add(float64(n))
}
