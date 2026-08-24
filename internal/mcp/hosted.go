package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zegit-zoo/meerkat/internal/authn"
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/refresh"
	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// hosted.go is the Streamable HTTP transport: `mk mcp serve-http`.
//
// It serves the same three tools ServeStdio does, over the MCP
// Streamable HTTP transport, with OIDC bearer authentication,
// collection-level authorization, RFC 9728 protected-resource metadata,
// liveness/readiness probes, Prometheus metrics and structured access
// logging. Design: docs/design/hosted-mcp.md.

// Default paths and timings. Every one is overridable through
// HostedConfig; these are the values a deployment gets by saying
// nothing.
const (
	// DefaultEndpointPath is where the MCP Streamable HTTP endpoint is
	// mounted. "/mcp" is the de-facto convention clients guess.
	DefaultEndpointPath = "/mcp"
	// DefaultAddr binds loopback, like `mk http serve`: exposing an MCP
	// server to a network is a decision an operator makes explicitly.
	DefaultAddr = ":4005"

	// LivenessPath answers "is this process running": it never touches
	// content, so a restart loop can't be caused by a bad KB mount.
	LivenessPath = "/livez"
	// ReadinessPath answers "can this process serve requests right now":
	// it reflects content resolution and per-collection index health.
	ReadinessPath = "/readyz"
	// MetricsPath is the Prometheus scrape endpoint.
	MetricsPath = "/metrics"

	defaultReadTimeout  = 15 * time.Second
	defaultIdleTimeout  = 120 * time.Second
	defaultHeartbeat    = 30 * time.Second
	defaultSessionIdle  = 30 * time.Minute
	defaultReadinessTTL = 5 * time.Second
	shutdownGrace       = 10 * time.Second
)

// HostedConfig configures the Streamable HTTP MCP server. The zero
// value is usable: it serves the embedded KB on DefaultAddr with no
// authentication, which is the local-development shape.
type HostedConfig struct {
	// Addr is the bind address ("host:port"). Empty means DefaultAddr.
	Addr string
	// EndpointPath is the MCP endpoint path. Empty means
	// DefaultEndpointPath.
	EndpointPath string
	// Collections is the set of collections served. Nil falls back to a
	// single collection over the process-global KB filesystem.
	Collections *collections.Registry
	// Auth is the parsed auth: block. Nil or disabled leaves the server
	// unauthenticated — the back-compat shape, and the only shape a
	// deployment that hasn't opted in can get.
	Auth *authz.Config
	// Version is the meerkat version reported in the banner and in the
	// meerkat_build_info metric.
	Version string
	// Logger receives structured access and lifecycle logs. Nil uses a
	// JSON slog handler on stderr. Access logs never carry the bearer
	// token or the caller's group membership.
	Logger *slog.Logger
	// Metrics is the Prometheus registry to register meerkat's
	// collectors on. Nil creates a private one, which is what keeps two
	// servers in the same test binary from colliding on the default
	// registry.
	Metrics *prometheus.Registry
	// HTTPClient is used for OIDC discovery and JWKS fetches. Nil uses a
	// client with a 15s timeout. Tests point it at a fake issuer.
	HTTPClient *http.Client
	// ReadTimeout / WriteTimeout / IdleTimeout are passed to
	// http.Server. WriteTimeout defaults to 0 (no limit) because a
	// Streamable HTTP GET is a long-lived SSE stream — a write deadline
	// would sever it mid-session. Per-request work is bounded by
	// QueryTimeout instead.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	// Stateful keeps per-session state in this process (mcp-go's
	// InsecureStatefulSessionIdManager) instead of accepting any
	// well-formed session ID. It requires sticky routing when more than
	// one replica is behind a load balancer, which is why it is off by
	// default.
	Stateful bool
	// HeartbeatInterval keeps idle SSE streams alive through proxies
	// that reap quiet connections. Zero means DefaultHeartbeat; a
	// negative value disables heartbeats.
	HeartbeatInterval time.Duration
	// SessionIdleTTL reclaims per-session transport state for clients
	// that vanish without sending DELETE. Zero means defaultSessionIdle;
	// negative disables the sweeper.
	SessionIdleTTL time.Duration
	// ReadinessTTL caches the readiness check's result. Readiness
	// re-enumerates every collection, which is real work on a large KB
	// and is not worth doing once per second. Zero means
	// defaultReadinessTTL.
	ReadinessTTL time.Duration
	// DisableDNSRebindingProtection turns off mcp-go's rejection of
	// loopback requests whose Host header is not a localhost value. Set
	// it only when a same-host reverse proxy forwards with the original
	// Host header preserved; prefer rewriting Host at the proxy.
	DisableDNSRebindingProtection bool
	// Observability is the parsed `observability:` block. Nil — which is
	// every deployment that has not written one — means no OpenTelemetry
	// SDK is constructed at all: no spans, no exporter, no goroutine, no
	// socket, and the Prometheus and slog behaviour of this server is
	// byte-identical to what it was before tracing existed. See
	// internal/telemetry and docs/design/observability.md.
	Observability *telemetry.Config
	// Telemetry injects an already-built telemetry instance, bypassing
	// Observability.
	//
	// It is the test seam, and the reason no test in this repo needs a
	// collector or a network: a test builds a *telemetry.Telemetry over
	// sdk/trace/tracetest's in-memory exporter and hands it here.
	// Production sets Observability instead; there is no configuration
	// key that could reach this field.
	Telemetry *telemetry.Telemetry
	// SetOTelGlobals installs this server's tracer provider as the
	// process-global one, so third-party clients instrumented against
	// the OpenTelemetry globals — the Google Cloud Storage client above
	// all — join meerkat's traces instead of emitting nowhere.
	//
	// Off by default because a test binary, or an embedding host, may
	// run several servers in one process and exactly one of them can own
	// a global. `mk mcp serve-http` sets it, because there it is the only
	// server there is.
	SetOTelGlobals bool
}

// HostedServer is a running (or runnable) Streamable HTTP MCP server.
type HostedServer struct {
	cfg     HostedConfig
	reg     *collections.Registry
	srv     *http.Server
	handler http.Handler
	stream  *mcpserver.StreamableHTTPServer
	log     *slog.Logger
	metrics *metrics
	gate    *authn.Gate
	auth    *authz.Config
	// refresh reconciles mutable GCS sources and GCS memory stores on
	// their configured schedules. nil when nothing opted in, which is
	// every deployment that has not written a `refresh:` block; every
	// call site tolerates a nil controller.
	refresh *refresh.Controller
	// tel is the OpenTelemetry layer. nil when no `observability:` block
	// opted in — which is every deployment that has not written one —
	// and every call site tolerates a nil, exactly as the refresh
	// controller above does. A nil tel is not a degraded mode; it is the
	// server this file described before tracing existed.
	tel *telemetry.Telemetry

	ready readinessCache
}

// NewHosted builds the server: it resolves OIDC discovery for every
// configured provider, validates the authorization policy, indexes
// every mounted collection, and wires the routes.
//
// Everything that can fail happens here rather than on the first
// request. An unreachable issuer, an unparseable policy or an
// unindexable collection fails the process at startup, where a
// deployment's own health gate catches it.
func NewHosted(ctx context.Context, cfg HostedConfig) (*HostedServer, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	reg := cfg.Collections
	if reg == nil {
		reg = collections.Global(kbdir.SourceEmbedded)
	}

	s := &HostedServer{
		cfg:  cfg,
		reg:  reg,
		log:  cfg.Logger,
		auth: cfg.Auth,
	}
	s.metrics = newMetrics(cfg.Metrics, cfg.Version, reg.Len())

	// Telemetry is constructed BEFORE the startup index build, so the
	// build is the first thing traced: a slow start on a large collection
	// is exactly the kind of thing an operator turns tracing on to see,
	// and it would be invisible if the SDK came up afterwards.
	//
	// It is also constructed AFTER the metrics registry, because the
	// domain collectors go on that same registry — /metrics gains series,
	// never a second endpoint.
	tel := cfg.Telemetry
	// abandon releases telemetry on a startup failure — but only the
	// instance THIS call built. An injected one (a test's) belongs to its
	// caller, who will shut it down themselves; tearing down somebody
	// else's exporter because our indexing failed would be a surprising
	// thing for a constructor to do.
	abandon := func() {}
	if tel == nil {
		built, err := telemetry.New(ctx, telemetry.Options{
			Config:     cfg.Observability,
			Registry:   s.metrics.reg,
			Logger:     s.log,
			Version:    cfg.Version,
			SetGlobals: cfg.SetOTelGlobals,
		})
		if err != nil {
			return nil, fmt.Errorf("observability: %w", err)
		}
		tel = built
		abandon = func() { _ = built.Shutdown(ctx) }
	}
	s.tel = tel
	// Everything below this line runs with telemetry in its context, so
	// startup work is traced by the same code that traces a request.
	ctx = telemetry.NewContext(ctx, s.tel)

	if err := indexAll(ctx, reg); err != nil {
		abandon()
		return nil, err
	}
	s.warnCollectionWidePersonalMemories()

	// Runtime reconciliation is opt-in and configuration-driven: the
	// targets come from the collections' own `refresh:` blocks, so there
	// is no server-level switch that could turn it on for a deployment
	// that did not ask, and none that could turn it off for one that did.
	// The loops do not start here — see ListenAndServe.
	s.refresh = refresh.New(refresh.Options{
		Targets:  reg.RefreshTargets(),
		Logger:   s.log,
		Registry: s.metrics.reg,
	})

	// The OIDC client is wrapped so discovery and JWKS fetches carry this
	// process's trace context outbound and appear as client spans. That
	// is the one place meerkat makes an outbound HTTP call on a request's
	// behalf, and "the IdP was slow" is otherwise indistinguishable from
	// "meerkat was slow" in a trace. A nil telemetry returns the client
	// untouched.
	verifier, err := authn.NewVerifier(ctx, authn.Options{
		Config:     cfg.Auth,
		HTTPClient: s.tel.HTTPClient(cfg.HTTPClient),
	})
	if err != nil {
		abandon()
		return nil, fmt.Errorf("oidc: %w", err)
	}
	metadataURL := ""
	if cfg.Auth != nil {
		metadataURL = authn.MetadataURL(cfg.Auth.Resource)
	}
	s.gate = authn.NewGate(verifier, metadataURL,
		authn.WithDenyHook(s.onDeny),
		authn.WithAnonymousHook(s.onAnonymous))

	hooks := &mcpserver.Hooks{}
	hooks.AddOnRegisterSession(func(context.Context, mcpserver.ClientSession) {
		s.metrics.sessions.Inc()
	})
	hooks.AddOnUnregisterSession(func(context.Context, mcpserver.ClientSession) {
		s.metrics.sessions.Dec()
	})

	// AllowAnonymousPersonal is deliberately false on this transport,
	// including under allow_unauthenticated and including for a caller
	// admitted by an `anonymous: true` rule (#36): a hosted server that
	// cannot name its caller must not let them write into a personal
	// namespace, because every anonymous caller would share the same one
	// — and, by the same token, must not let them READ one either. An
	// anonymous caller here owns nothing, so they see every public page
	// and no personal memory at all (see transportOptions.viewer).
	//
	// Publishing a COLLECTION to anonymous callers does not move this
	// line and cannot: ownership is a property of who the caller is, and
	// an anonymous caller is still nobody. Their grants are read-only by
	// validation, so they are not offered the memory tool at all.
	mcpSrv := newServer(reg, transportOptions{},
		mcpserver.WithHooks(hooks),
		// The per-request tool filter is what keeps tools/list from
		// naming collections the caller may not read — see toolFilter.
		mcpserver.WithToolFilter(toolFilter(reg)),
		mcpserver.WithToolHandlerMiddleware(s.metrics.instrumentTool),
	)

	streamOpts := []mcpserver.StreamableHTTPOption{
		mcpserver.WithEndpointPath(cfg.EndpointPath),
		mcpserver.WithStreamableHTTPLogger(s.log),
	}
	if cfg.Stateful {
		streamOpts = append(streamOpts, mcpserver.WithStateful(true))
	}
	if cfg.HeartbeatInterval > 0 {
		streamOpts = append(streamOpts, mcpserver.WithHeartbeatInterval(cfg.HeartbeatInterval))
	}
	if cfg.SessionIdleTTL > 0 {
		streamOpts = append(streamOpts, mcpserver.WithSessionIdleTTL(cfg.SessionIdleTTL))
	}
	if cfg.DisableDNSRebindingProtection {
		streamOpts = append(streamOpts, mcpserver.WithDisableLocalhostProtection(true))
	}
	s.stream = mcpserver.NewStreamableHTTPServer(mcpSrv, streamOpts...)

	s.handler = s.routes()
	// Establish the readiness baseline before the listener opens: the
	// first Check latches whether each collection's content root was
	// present at startup, which is what later distinguishes "this
	// deployment serves an empty collection on purpose" from "the
	// volume went away". It also primes meerkat_ready for the first
	// scrape.
	s.readiness()
	s.srv = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}
	return s, nil
}

// warnCollectionWidePersonalMemories logs, once at startup, every
// collection that opted out of private personal reads
// (`memory.personal_visibility: collection`) while this server
// authenticates its callers.
//
// The pairing is the point. Without OIDC there is no principal to keep a
// memory private FROM, so the setting changes nothing worth mentioning.
// With OIDC, meerkat knows exactly whose each personal memory is and has
// been told to show it to every other reader of the collection anyway —
// a deliberate, unusual choice, and one an operator should be able to
// find in the log of the process that is acting on it rather than only
// in the config file they wrote months ago.
//
// It is a warning, not an error: the setting exists precisely so a
// deployment that relied on the pre-#27 behaviour can keep it.
func (s *HostedServer) warnCollectionWidePersonalMemories() {
	if !s.AuthEnabled() {
		return
	}
	for _, c := range s.reg.All() {
		if !c.PersonalReadsAreCollectionWide() {
			continue
		}
		s.log.Warn("personal memories are readable collection-wide",
			"collection", c.Name,
			"setting", "memory.personal_visibility="+memory.VisibilityCollection,
			"effect", "every caller who can read this collection can read every personal memory in it, "+
				"including memories saved by other authenticated principals",
			"secure_default", memory.VisibilityPrivate)
	}
}

func (c *HostedConfig) applyDefaults() {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.EndpointPath == "" {
		c.EndpointPath = DefaultEndpointPath
	}
	if c.Version == "" {
		c.Version = "dev"
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewJSONHandler(defaultLogWriter(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = defaultReadTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = defaultIdleTimeout
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = defaultHeartbeat
	}
	if c.SessionIdleTTL == 0 {
		c.SessionIdleTTL = defaultSessionIdle
	}
	if c.ReadinessTTL == 0 {
		c.ReadinessTTL = defaultReadinessTTL
	}
}

// reservedPaths are the operational endpoints the mux always registers.
// The MCP endpoint may not be moved on top of one: http.ServeMux panics
// on a duplicate pattern, and a panic during startup is a worse
// diagnostic than a sentence naming the conflict.
//
// Mounting the MCP endpoint at a reserved path would also be a security
// problem in its own right — those paths are outside the authentication
// gate.
func reservedPaths() []string {
	return []string{LivenessPath, ReadinessPath, MetricsPath, authn.MetadataPath}
}

func (c *HostedConfig) validate() error {
	if !strings.HasPrefix(c.EndpointPath, "/") {
		return fmt.Errorf("MCP endpoint path %q must start with /", c.EndpointPath)
	}
	for _, p := range reservedPaths() {
		if c.EndpointPath == p {
			return fmt.Errorf("MCP endpoint path %q collides with a reserved unauthenticated endpoint — choose another --path", p)
		}
	}
	if c.Auth != nil && len(c.Auth.Providers) > 0 && authn.MetadataURL(c.Auth.Resource) == "" {
		return fmt.Errorf("auth.resource %q is not an absolute URL, so no RFC 9728 metadata location can be derived for the 401 challenge", c.Auth.Resource)
	}
	return nil
}

// routes builds the mux.
//
// Only the MCP endpoint is behind the authentication gate. The three
// operational endpoints are not, deliberately:
//
//   - /livez and /readyz are polled by an orchestrator that has no OIDC
//     token and shouldn't need one; a probe that can fail for auth
//     reasons is a probe that restarts healthy pods.
//   - /metrics is scraped by Prometheus, likewise.
//
// The cost of that is bounded by what those endpoints say, and they are
// written to say nothing worth having: /readyz reports counts, never
// collection names (the names go to the log, which is not public), and
// no metric carries a collection name, a page ID or a caller identity
// as a label. The RFC 9728 metadata endpoint is public by definition —
// its whole job is to be readable by a client that has no token yet.
func (s *HostedServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle(s.cfg.EndpointPath, s.gate.Middleware(captureIdentity(s.stream)))

	if s.auth != nil && s.auth.Resource != "" {
		md := authn.MetadataHandler(s.auth, "meerkat knowledge base")
		mux.Handle(authn.MetadataPath, md)
		// RFC 9728 §3.1 puts a path-qualified resource's metadata under
		// the well-known prefix. Serve both, so a client that guesses the
		// bare path (as many do) still discovers the same document.
		if p := mcpserver.ProtectedResourceMetadataPath(s.auth.Resource); p != authn.MetadataPath {
			mux.Handle(p, md)
		}
	}

	mux.HandleFunc("GET "+LivenessPath, s.handleLivez)
	mux.HandleFunc("GET "+ReadinessPath, s.handleReadyz)
	mux.Handle("GET "+MetricsPath, promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /{$}", s.handleRoot)

	// Outermost first: the root span has to exist before the access log
	// runs, so the log line can carry the trace it belongs to, and it has
	// to cover the authentication gate, so a 401 is a complete trace
	// rather than a gap. With no telemetry configured traceHTTP returns
	// its argument unchanged — not a wrapper — so this composes to
	// exactly the two middlewares it always did.
	return s.traceHTTP(s.accessLog(s.metrics.instrumentHTTP(mux)))
}

// Handler exposes the fully-wrapped mux, for tests and for embedding
// meerkat's MCP endpoint in a larger server.
func (s *HostedServer) Handler() http.Handler { return s.handler }

// Addr returns the configured bind address.
func (s *HostedServer) Addr() string { return s.cfg.Addr }

// EndpointPath returns the MCP endpoint path.
func (s *HostedServer) EndpointPath() string { return s.cfg.EndpointPath }

// AuthEnabled reports whether OIDC bearer authentication is in force.
// It is false both when no auth: block is configured at all and when
// one sets allow_unauthenticated — in either case no token is checked.
//
// It stays true when the policy also publishes collections to anonymous
// callers: those are a carve-out INSIDE an authenticated deployment, not
// a different posture. Ask AnonymousEnabled for the carve-out.
func (s *HostedServer) AuthEnabled() bool { return s.auth != nil && len(s.auth.Providers) > 0 }

// AnonymousEnabled reports whether any policy rule publishes
// collections to callers with no bearer token. It is what the startup
// banner says out loud, because "this server answers unauthenticated
// requests" is not something an operator should have to infer from a
// config file they wrote months ago.
func (s *HostedServer) AnonymousEnabled() bool { return s.auth.HasAnonymousRules() }

// ServeStreamableHTTP builds a hosted server from cfg and serves it
// until ctx is cancelled — the Streamable HTTP counterpart of
// ServeStdio, and what `mk mcp serve-http` calls.
//
// ready, when non-nil, is invoked with the constructed server after
// startup work (OIDC discovery, policy validation, indexing) has
// succeeded and before the listener opens, so a caller can log what it
// is about to serve.
func ServeStreamableHTTP(ctx context.Context, cfg HostedConfig, ready func(*HostedServer)) error {
	s, err := NewHosted(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if ready != nil {
		ready(s)
	}
	return s.ListenAndServe(ctx)
}

// ListenAndServe blocks until the server stops, shutting down
// gracefully on ctx cancellation. Active MCP sessions are closed first
// so SSE streams end cleanly rather than being severed by the listener
// going away.
func (s *HostedServer) ListenAndServe(ctx context.Context) error {
	// Reconciliation runs for exactly as long as the server does. Starting
	// it here rather than in NewHosted means a caller that builds a server
	// to inspect its handler (a test, an embedding host) never acquires a
	// background poller it did not ask for.
	//
	// The controller's context carries the telemetry, so a scheduled
	// refresh cycle is traced by the same code an admin-triggered one is
	// — there is one funnel (Controller.runOnce) and it sees the same
	// context either way.
	s.refresh.Start(telemetry.NewContext(ctx, s.tel))
	defer func() { _ = s.refresh.Close() }()

	errCh := make(chan error, 1)
	go func() {
		err := s.srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		s.stream.CloseSessions(shutdownCtx)
		_ = s.srv.Shutdown(shutdownCtx)
		// Flush telemetry LAST, and never let it decide the outcome. The
		// spans worth keeping are the ones from the requests that just
		// finished, so the flush belongs after the graceful drain; and a
		// collector that is already gone must not turn a clean shutdown
		// into a failed one, so the error is logged inside Shutdown and
		// discarded here.
		_ = s.tel.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		_ = s.tel.Shutdown(context.Background())
		return err
	}
}

// Close stops reconciliation and releases every collection's search
// index.
//
// The refresh controller is stopped FIRST, and Close waits for any
// in-flight cycle: a cycle that is mid-swap is about to install a
// snapshot into a registry we are about to tear down.
func (s *HostedServer) Close() error {
	_ = s.refresh.Close()
	// Telemetry after the controller and before the registry: a cycle
	// that was still finishing may have ended spans, and they should get
	// their one bounded flush attempt. Its error is deliberately not part
	// of Close's answer — an unreachable collector is not a failure to
	// release this server's resources.
	_ = s.tel.Shutdown(context.Background())
	if s.reg == nil {
		return nil
	}
	err := s.reg.Close()
	s.reg = nil
	return err
}

// Reload runs one reconciliation cycle for every configured refresh
// target, right now, and reports what failed.
//
// It is the ADMIN RELOAD TRIGGER — `mk mcp serve-http` wires SIGHUP to
// it — and it deliberately calls the very same Reconcile path the
// scheduled loops use. A second update path would be a second place to
// get the staging discipline, the generation preconditions and the
// atomic swap wrong.
//
// A deployment with no `refresh:` block anywhere has nothing to reload,
// and this is a no-op rather than an error: an operator sending SIGHUP
// to the wrong process should not get a failure they have to interpret.
func (s *HostedServer) Reload(ctx context.Context) error {
	return s.refresh.ReloadNow(telemetry.NewContext(ctx, s.tel))
}

// --- probes ----------------------------------------------------------

// readinessCache memoises the readiness answer for ReadinessTTL. See
// HostedConfig.ReadinessTTL.
type readinessCache struct {
	mu       sync.Mutex
	at       time.Time
	ready    bool
	health   []collections.Health
	computed bool
}

// handleLivez answers liveness. It reports on the process only: a
// content root that has gone away makes the server UNREADY, never
// un-live, because restarting the process does not remount a volume and
// a liveness failure would just produce a crash loop.
func (s *HostedServer) handleLivez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// handleReadyz answers readiness from the registry's own state: every
// mounted collection must enumerate its pages and hold a built search
// index. That is exactly the set of things a search/show/list request
// depends on, so a green /readyz means those requests can be served.
//
// The response body carries COUNTS AND STATE, never collection names,
// never a bucket, never a generation, never an error string — /readyz is
// unauthenticated, and the mounted set is not public information (see
// routes). Everything behind a number here goes to the structured log
// and to authenticated collection discovery instead.
//
// Two axes, deliberately separate:
//
//	ready     the collection enumerates and holds a built index, so it
//	          is answering queries. Drives the HTTP status.
//	degraded  its most recent refresh FAILED and it is serving the last
//	          known-good snapshot. Reported, but does not by itself fail
//	          the probe.
//
// A degraded collection answering correct, slightly older content is not
// an availability failure, and 503-ing on one would drain a healthy
// replica over a publication-pipeline mistake — usually every replica at
// once, since they all read the same bucket. An operator who wants the
// opposite trade asks for it per collection with `failure_policy:
// unready`, which makes the collection not-ready and shows up here as a
// 503 through the normal path.
func (s *HostedServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ready, health := s.readinessFor(r.Context())
	total := len(health)
	nReady, nDegraded := 0, 0
	for _, h := range health {
		if h.Ready {
			nReady++
		}
		if h.Degraded {
			nDegraded++
		}
	}
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "degraded"
	} else if nDegraded > 0 {
		// Serving, but not current. Worth a word an alert can match on,
		// and not worth taking the replica out of rotation for.
		state = "degraded"
	}
	writeJSON(w, status, map[string]any{
		"status": state,
		"collections": map[string]int{
			"ready":    nReady,
			"degraded": nDegraded,
			"total":    total,
		},
	})
}

// readiness returns the cached readiness answer, recomputing it when
// stale. It is readinessFor with no context — the startup call, which
// happens before any request exists.
func (s *HostedServer) readiness() (bool, []collections.Health) {
	return s.readinessFor(telemetry.NewContext(context.Background(), s.tel))
}

// readinessFor is readiness with the caller's context, so a recompute
// lands in that request's trace.
//
// The span carries the two COUNTS the response body carries and nothing
// else: no collection name, no error text, no stale reason. /readyz is
// unauthenticated and a span is exported off-box, so the stricter of the
// two rules applies.
func (s *HostedServer) readinessFor(ctx context.Context) (bool, []collections.Health) {
	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if s.ready.computed && time.Since(s.ready.at) < s.cfg.ReadinessTTL {
		return s.ready.ready, s.ready.health
	}
	_, span := telemetry.Span(ctx, telemetry.SpanReadiness)
	ready, health := s.reg.Ready()
	if !ready && (!s.ready.computed || s.ready.ready) {
		// Log the transition into degraded, with the names the response
		// body deliberately omits.
		for _, h := range health {
			if !h.Ready {
				s.log.Error("collection not ready", "collection", h.Name, "error", h.Error)
			}
		}
	}
	s.ready.ready, s.ready.health, s.ready.at, s.ready.computed = ready, health, time.Now(), true
	s.metrics.setReady(ready)
	nReady, nDegraded, pages := s.metrics.setCollectionStates(health)
	// The indexed page total rides the readiness sweep because that is
	// where the pages were just enumerated — computing it separately
	// would double the most expensive thing a probe does.
	telemetry.FromContext(ctx).Metrics().SetIndexedPages(pages)
	span.SetAttributes(
		telemetry.KeyReady.Bool(ready),
		telemetry.KeyCollectionsReady.Int(nReady),
		telemetry.KeyCollectionsDegraded.Int(nDegraded),
		telemetry.KeyIndexPages.Int(pages),
	)
	span.End()
	return ready, health
}

// handleRoot is a human-readable banner. It names no collection: an
// unauthenticated GET / must not enumerate what a deployment mounts.
func (s *HostedServer) handleRoot(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "meerkat hosted MCP server (version %s)\n\n", s.cfg.Version)
	fmt.Fprintf(w, "  %-34s MCP Streamable HTTP endpoint\n", s.cfg.EndpointPath)
	if s.auth != nil && s.auth.Resource != "" {
		fmt.Fprintf(w, "  %-34s OAuth 2.0 protected resource metadata (RFC 9728)\n", authn.MetadataPath)
	}
	fmt.Fprintf(w, "  %-34s liveness probe\n", LivenessPath)
	fmt.Fprintf(w, "  %-34s readiness probe\n", ReadinessPath)
	fmt.Fprintf(w, "  %-34s Prometheus metrics\n", MetricsPath)
	fmt.Fprintln(w, "")
	switch {
	case s.AuthEnabled() && s.AnonymousEnabled():
		// No count and no names: this page is unauthenticated, and the
		// mounted set is not public information. That selected collections
		// are published is a fact about the deployment's posture; WHICH
		// ones is for mk_list_collections, which answers each caller with
		// their own set.
		fmt.Fprintln(w, "Authentication: OIDC bearer token required on the MCP endpoint,")
		fmt.Fprintln(w, "except for selected collections published to unauthenticated callers.")
	case s.AuthEnabled():
		fmt.Fprintln(w, "Authentication: OIDC bearer token required on the MCP endpoint.")
	default:
		fmt.Fprintln(w, "Authentication: none configured — every mounted collection is readable by any caller.")
	}
}

// --- access logging ---------------------------------------------------

// accessLog emits one structured line per request.
//
// It deliberately does not log: the Authorization header or any part of
// a token, the caller's group membership (a full directory-group list
// per request is a liability and tells an operator nothing they can act
// on), or request bodies. It does log the subject and tenant of an
// authenticated caller, which is what an audit trail needs.
//
// When tracing is on it additionally carries `trace_id` and `span_id`,
// so this line and the trace of the same request join. That is the whole
// of the bridge between the two surfaces, and it goes one way only: IDs
// come into the log, and NOTHING goes from the log into the span. The
// identity fields above stay here, on the operator's own stderr, rather
// than being exported to a collector — see internal/telemetry's package
// comment.
func (s *HostedServer) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Reuse the recorder the tracing middleware already installed,
		// when there is one, so a request is never counted twice or
		// double-wrapped. With telemetry off there is none and this is the
		// line it always was.
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		}
		// The authentication gate runs BELOW this middleware and installs
		// grants on a derived request, which this frame never sees. Hand
		// a holder down for captureIdentity to fill in, so the access log
		// can name the caller without the gate having to know it exists.
		holder := &identityHolder{}
		r = r.WithContext(context.WithValue(r.Context(), identityHolderKey, holder))
		next.ServeHTTP(rec, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"bytes", rec.written,
			"remote", clientIP(r),
		}
		if ua := r.UserAgent(); ua != "" {
			attrs = append(attrs, "user_agent", ua)
		}
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			attrs = append(attrs, "mcp_session", sid)
		}
		if id, ok := holder.get(); ok {
			if id.Subject == "" {
				// An admitted ANONYMOUS request (#36). It gets a bounded
				// classification and nothing else: no sub, no issuer, no
				// tenant — not even empty ones. There is no identity here,
				// and a log line that printed `"sub":""` would be inventing
				// a principal that does not exist, which is exactly the
				// thing an audit trail must not do.
				attrs = append(attrs, "auth", string(authn.ReasonAnonymous))
			} else {
				attrs = append(attrs, "sub", id.Subject, "issuer", id.Issuer)
				if id.Tenant != "" {
					attrs = append(attrs, "tenant", id.Tenant)
				}
			}
		}
		attrs = append(attrs, s.tel.LogAttrs(r.Context())...)
		s.log.Info("mcp.access", attrs...)
	})
}

// identityHolder carries the authenticated identity back UP the
// middleware stack, from below the authentication gate to the access
// log above it.
//
// The obvious alternative — logging inside the gate — would log only
// the MCP endpoint, leaving the probes and metrics unlogged; the other
// — putting the access log below the gate — would lose every 401,
// which is the line an operator most wants. A one-word mutable holder
// threaded through the context is the small cost of keeping one log
// line for every request.
//
// It is written once, by captureIdentity, on the request goroutine
// before the handler returns, and read after ServeHTTP returns. The
// mutex is there because an SSE handler may still be writing when a
// sibling read happens under -race.
type identityHolder struct {
	mu  sync.Mutex
	id  authz.Identity
	set bool
}

func (h *identityHolder) put(id authz.Identity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.id, h.set = id, true
}

func (h *identityHolder) get() (authz.Identity, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id, h.set
}

type ctxKey int

const identityHolderKey ctxKey = iota

// captureIdentity records the authenticated identity for the access
// log. It sits directly below the authentication gate, so by the time
// it runs the grants are in the request context.
//
// An anonymous admission (#36) also installs grants — over an Identity
// with no subject — so the holder is set with a zero identity and the
// access log renders that as one bounded field rather than as a set of
// empty identity fields. A request with NO grants at all (no auth: block
// configured, or allow_unauthenticated) leaves the holder untouched and
// the log line exactly as it always was.
func captureIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := r.Context().Value(identityHolderKey).(*identityHolder); ok {
			if g := authz.FromContext(r.Context()); g != nil {
				h.put(g.Identity())
			}
		}
		next.ServeHTTP(w, r)
	})
}

// onDeny records a refused request. Reason is a small closed set, so it
// is safe as a metric label.
func (s *HostedServer) onDeny(r *http.Request, reason authn.Reason, err error) {
	s.metrics.authFailures.WithLabelValues(string(reason)).Inc()
	attrs := []any{"reason", string(reason), "path", r.URL.Path, "remote", clientIP(r)}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	attrs = append(attrs, s.tel.LogAttrs(r.Context())...)
	s.log.Warn("mcp.auth_denied", attrs...)
}

// onAnonymous records a request admitted without a token, on the
// strength of the policy's anonymous rules.
//
// It is Debug, not Warn: on a deployment that publishes a collection,
// anonymous traffic is the intended traffic and a per-request warning
// would be noise an operator learns to ignore. The line is here at all
// so that "who reached the public collections" is answerable from the
// same stderr as everything else — the ACCESS log below carries the same
// classification on every request, this one just makes the gate's own
// decision greppable. Neither invents an identity: there is none.
func (s *HostedServer) onAnonymous(r *http.Request, reason authn.Reason) {
	s.metrics.authAnonymous.Inc()
	attrs := append([]any{
		"outcome", string(reason), "path", r.URL.Path, "remote", clientIP(r),
	}, s.tel.LogAttrs(r.Context())...)
	s.log.Debug("mcp.auth_anonymous", attrs...)
}

// statusRecorder captures the status and byte count for the access log.
//
// It implements http.Flusher and Unwrap because the Streamable HTTP
// transport needs both: mcp-go type-asserts the ResponseWriter to
// http.Flusher to decide whether SSE streaming is possible at all
// (a wrapper that swallowed Flusher would silently downgrade every
// session to buffered JSON), and reaches through http.ResponseController
// — which follows Unwrap — to set per-write deadlines.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += n
	return n, err
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// clientIP is the peer address with the port stripped. X-Forwarded-For
// is deliberately NOT consulted: it is client-controlled unless a
// trusted proxy is known to rewrite it, and meerkat has no way to know
// that here. Operators who terminate at a proxy should log there.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ResolveListenAddr picks a usable bind string from a host+port pair.
func ResolveListenAddr(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}
