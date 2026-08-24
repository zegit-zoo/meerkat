package telemetry

import "go.opentelemetry.io/otel/attribute"

// attrs.go is the `meerkat.*` attribute namespace and the span-name
// vocabulary, in one place so that "what may a span say" is answerable
// by reading one file.
//
// # Why a namespace of our own
//
// OpenTelemetry semantic conventions cover the HTTP server span
// (http.request.method, http.route, http.response.status_code) and
// meerkat uses them there — a collector's HTTP dashboards work without
// configuration. There is no ratified convention for MCP, for a
// knowledge-base search, or for a content-source reconciliation cycle,
// so those get a stable, documented `meerkat.` prefix rather than a
// guess at a future one. Every key here is part of the documented
// contract (docs/design/observability.md) and changing one is a
// breaking change to somebody's dashboard.
//
// # Every value below is bounded
//
// Attribute values are either integers/booleans/durations, or strings
// from a CLOSED SET declared in this file. There is no key here whose
// value is caller-supplied text, and none that names a collection, a
// page, a bucket, an object, a memory key or a principal. The one key
// that carries a high-cardinality string — meerkat.refresh.version — is
// deliberate and safe for a span in a way it is not for a metric label:
// a span is an event, not a time series, so a generation that
// increments forever adds no series.
const (
	// --- outcome, shared by most spans -------------------------------
	//
	// A closed vocabulary, so a consumer can group by it. See the
	// Outcome* constants for the values.
	KeyOutcome = attribute.Key("meerkat.outcome")

	// --- MCP ---------------------------------------------------------

	// KeyMCPTool is the invoked tool's name, clamped to the registered
	// set by BoundedTool. An unrecognised name reports "other" rather
	// than being echoed back.
	KeyMCPTool = attribute.Key("meerkat.mcp.tool")
	// KeyMCPMethod is the JSON-RPC method of an MCP request, from the
	// closed set the transport dispatches.
	KeyMCPMethod = attribute.Key("meerkat.mcp.method")
	// KeyMCPRequestBytes / KeyMCPResponseBytes are payload sizes. Sizes,
	// never payloads.
	KeyMCPRequestBytes  = attribute.Key("meerkat.mcp.request_bytes")
	KeyMCPResponseBytes = attribute.Key("meerkat.mcp.response_bytes")

	// --- authentication and authorization ----------------------------
	//
	// Note what is absent: no subject, no issuer, no tenant, no email,
	// no group, no audience, no token, no claim of any kind. The access
	// log carries the identity for audit; a span exported to a collector
	// does not. See the package comment.

	// KeyAuthnResult is why the gate let a request through or did not:
	// ok | missing_token | invalid_token | no_grants | disabled.
	KeyAuthnResult = attribute.Key("meerkat.authn.result")
	// KeyAuthnProviders is how many OIDC providers were configured — a
	// count, so a "which issuer" question is answerable as "there is
	// more than one" without naming any.
	KeyAuthnProviders = attribute.Key("meerkat.authn.providers")
	// KeyAuthzGranted reports whether the policy matched at all.
	KeyAuthzGranted = attribute.Key("meerkat.authz.granted")
	// KeyAuthzCollections is how many collections the caller ended up
	// able to read. A count; never the names.
	KeyAuthzCollections = attribute.Key("meerkat.authz.collections")
	// KeyAuthzRules is how many policy rules were evaluated.
	KeyAuthzRules = attribute.Key("meerkat.authz.rules")

	// --- collections and knowledge operations -------------------------

	// KeyCollectionOrdinal is a collection's index in configuration
	// order — the same bounded identifier the refresh metrics are
	// labelled by, and for the same reason (internal/refresh/metrics.go).
	KeyCollectionOrdinal = attribute.Key("meerkat.collection.ordinal")
	// KeyCollectionCount is how many collections an operation spanned.
	KeyCollectionCount = attribute.Key("meerkat.collection.count")
	// KeyCollectionQualified reports that the caller wrote
	// "<collection>:<page-id>" rather than a bare ID. A boolean about
	// the SHAPE of the request, not its content.
	KeyCollectionQualified = attribute.Key("meerkat.collection.qualified")
	// KeyCollectionNamed reports that the caller named a collection
	// explicitly. Again a boolean, never the name.
	KeyCollectionNamed = attribute.Key("meerkat.collection.named")

	// KeySearchLimit is the clamped result limit.
	KeySearchLimit = attribute.Key("meerkat.search.limit")
	// KeySearchResults is how many hits came back. A count, never an ID.
	KeySearchResults = attribute.Key("meerkat.search.results")
	// KeySearchQueryLength is the query's length in bytes. The LENGTH,
	// never the query: an operator debugging "queries over 4KB are slow"
	// gets their answer without the query text leaving the process.
	KeySearchQueryLength = attribute.Key("meerkat.search.query_length")
	// KeySearchFiltered reports that a per-page visibility clause was
	// conjoined into the query (i.e. the caller is a restricted viewer).
	KeySearchFiltered = attribute.Key("meerkat.search.filtered")

	// KeyPagesMatched is how many pages an operation resolved.
	KeyPagesMatched = attribute.Key("meerkat.pages.matched")
	// KeyPagesReturned is how many it returned after filtering.
	KeyPagesReturned = attribute.Key("meerkat.pages.returned")

	// --- indexing -----------------------------------------------------

	// KeyIndexPages is how many documents went into an index build.
	KeyIndexPages = attribute.Key("meerkat.index.pages")

	// --- content sources ----------------------------------------------

	// KeySourceType is the bounded source type: embedded | local | url |
	// gcs-object | gcs-prefix. See SourceType.
	KeySourceType = attribute.Key("meerkat.source.type")
	// KeyCacheResult is hit | miss.
	KeyCacheResult = attribute.Key("meerkat.source.cache")
	// KeySourceBytes is how many bytes were downloaded.
	KeySourceBytes = attribute.Key("meerkat.source.bytes")
	// KeySourceObjects is how many objects a prefix listing matched.
	KeySourceObjects = attribute.Key("meerkat.source.objects")
	// KeyGCSOperation is the storage operation: attrs | list | read.
	// Never the bucket, never the object name.
	KeyGCSOperation = attribute.Key("meerkat.gcs.operation")

	// --- memory --------------------------------------------------------

	// KeyMemoryScope is personal | team | global.
	KeyMemoryScope = attribute.Key("meerkat.memory.scope")
	// KeyMemoryBackend is local | gcs.
	KeyMemoryBackend = attribute.Key("meerkat.memory.backend")
	// KeyMemoryOperation is save | stage | load | stat | fingerprint.
	KeyMemoryOperation = attribute.Key("meerkat.memory.operation")
	// KeyMemoryRecords is how many memory documents a load returned.
	KeyMemoryRecords = attribute.Key("meerkat.memory.records")
	// KeyMemoryBytes is a stored document's size. The size, not the
	// document.
	KeyMemoryBytes = attribute.Key("meerkat.memory.bytes")

	// --- refresh / reconciliation ---------------------------------------

	// KeyRefreshKind is content | memory (refresh.KindContent /
	// KindMemory).
	KeyRefreshKind = attribute.Key("meerkat.refresh.kind")
	// KeyRefreshChanged reports whether a cycle actually swapped a new
	// snapshot in.
	KeyRefreshChanged = attribute.Key("meerkat.refresh.changed")
	// KeyRefreshVersion is the source version token now being served: an
	// object generation, a prefix fingerprint, or a memory store
	// fingerprint.
	//
	// This is the one unbounded-cardinality value in the namespace, and
	// it is here ON PURPOSE. internal/refresh/metrics.go refuses it as a
	// Prometheus label because a generation increments forever and each
	// value would mint a permanent time series. A span is not a time
	// series: the value is an attribute of one event, it costs one
	// string in one exported record, and it is precisely what an
	// operator asking "which generation did this replica pick up, and
	// when" needs.
	KeyRefreshVersion = attribute.Key("meerkat.refresh.version")
	// KeyRefreshTrigger is scheduled | admin.
	KeyRefreshTrigger = attribute.Key("meerkat.refresh.trigger")
	// KeyRefreshPolicy is the configured failure policy.
	KeyRefreshPolicy = attribute.Key("meerkat.refresh.policy")

	// --- readiness -------------------------------------------------------

	// KeyReady / KeyCollectionsReady / KeyCollectionsDegraded mirror what
	// /readyz's body says: counts and state, never names.
	KeyReady               = attribute.Key("meerkat.ready")
	KeyCollectionsReady    = attribute.Key("meerkat.collections.ready")
	KeyCollectionsDegraded = attribute.Key("meerkat.collections.degraded")
)

// Span names. Constants rather than literals so the taxonomy is one
// grep away and a rename cannot silently fork a dashboard's grouping.
const (
	SpanMCPTool          = "meerkat.mcp.tool"
	SpanMCPSession       = "meerkat.mcp.session"
	SpanAuthnVerify      = "meerkat.authn.verify"
	SpanAuthzDecide      = "meerkat.authz.decide"
	SpanSearch           = "meerkat.search"
	SpanSearchCollection = "meerkat.search.collection"
	SpanShow             = "meerkat.show"
	SpanList             = "meerkat.list"
	SpanListCollections  = "meerkat.list_collections"
	SpanIndexBuild       = "meerkat.index.build"
	SpanIndexSwap        = "meerkat.index.swap"
	SpanSourceResolve    = "meerkat.source.resolve"
	SpanSourceProbe      = "meerkat.source.probe"
	SpanGCS              = "meerkat.gcs"
	SpanMemorySave       = "meerkat.memory.save"
	SpanMemoryStage      = "meerkat.memory.stage"
	SpanMemoryStore      = "meerkat.memory.store"
	SpanRefreshCycle     = "meerkat.refresh.cycle"
	SpanRefreshPhase     = "meerkat.refresh.phase"
	SpanReadiness        = "meerkat.readiness"
)

// Refresh phase span names. The six steps ReloadContent/ReloadMemory
// walk, so a slow reconciliation is attributable to one of them rather
// than to "the cycle".
const (
	PhaseProbe     = "meerkat.refresh.probe"
	PhaseResolve   = "meerkat.refresh.resolve"
	PhaseMount     = "meerkat.refresh.mount"
	PhaseEnumerate = "meerkat.refresh.enumerate"
	PhaseBuild     = "meerkat.refresh.build"
	PhaseCommit    = "meerkat.refresh.commit"
)

// Outcome values. The closed set KeyOutcome ranges over, shared with the
// existing Prometheus outcome labels so a metric and a span agree on the
// vocabulary.
const (
	OutcomeOK           = "ok"
	OutcomeToolError    = "tool_error"
	OutcomeError        = "error"
	OutcomeConflict     = "conflict"
	OutcomeStaged       = "staged"
	OutcomeSaved        = "saved"
	OutcomeNotFound     = "not_found"
	OutcomeAmbiguous    = "ambiguous"
	OutcomeInvalidQuery = "invalid_query"
	OutcomeTimeout      = "timeout"
	OutcomeUnchanged    = "unchanged"
	OutcomeBusy         = "busy"
	OutcomeCancelled    = "cancelled"
)

// Source type values — the bounded vocabulary KeySourceType and the
// source-resolution metrics share.
const (
	SourceEmbedded  = "embedded"
	SourceLocal     = "local"
	SourceURL       = "url"
	SourceGCSObject = "gcs-object"
	SourceGCSPrefix = "gcs-prefix"
	SourceOther     = "other"
)

// Cache results.
const (
	CacheHit  = "hit"
	CacheMiss = "miss"
)

// Memory backends.
const (
	BackendLocal = "local"
	BackendGCS   = "gcs"
	BackendOther = "other"
)

// Memory store operations — the closed set KeyMemoryOperation and the
// backend duration/error metrics range over.
const (
	MemorySave        = "save"
	MemoryStage       = "stage"
	MemoryLoad        = "load"
	MemoryStat        = "stat"
	MemoryFingerprint = "fingerprint"
)

// Outcome builds the shared outcome attribute.
func Outcome(v string) attribute.KeyValue { return KeyOutcome.String(v) }

// registeredTools is the closed set of tool names a span or a
// coarse-payload metric may carry.
//
// mcp-go dispatches a tool call only to a REGISTERED handler, so the
// name reaching the instrumentation is already one of these in
// practice. The clamp is here anyway, because "already bounded in
// practice" is exactly the assumption that turns into a cardinality
// incident when a later transport dispatches differently. An unknown
// name reports "other" — one extra value, forever.
var registeredTools = map[string]bool{
	"mk_search":           true,
	"mk_show":             true,
	"mk_list":             true,
	"mk_list_collections": true,
	"mk_save_memory":      true,
}

// BoundedTool clamps a tool name to the registered set.
func BoundedTool(name string) string {
	if registeredTools[name] {
		return name
	}
	return "other"
}

// SourceType maps a content-source type plus its GCS mode to the bounded
// vocabulary above. It takes the raw strings rather than importing
// internal/contentsource, which would be a dependency cycle (that
// package embeds this one's Config).
func SourceType(typ string, hasObject, hasPrefix bool) string {
	switch typ {
	case "none", "":
		return SourceEmbedded
	case "local":
		return SourceLocal
	case "url":
		return SourceURL
	case "gcs":
		switch {
		case hasObject:
			return SourceGCSObject
		case hasPrefix:
			return SourceGCSPrefix
		}
		return SourceOther
	}
	return SourceOther
}
