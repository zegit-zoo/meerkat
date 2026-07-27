// Package http exposes the embedded knowledge base over an
// HTTP/JSON API designed to be consumed by OpenWebUI tool servers
// and similar clients.
//
// The endpoint surface mirrors the MCP tool set 1:1 so that an
// OpenWebUI tool definition matches what an MCP client sees:
//
//	POST /search        body: {"query":"...", "limit":10}        -> [{id,title,score,snippet,category,status}]
//	POST /show          body: {"id":"concepts/Rate-Limiting"} -> {id,title,body,front}
//	POST /list          body: {"prefix":"systems/", "category":"systems", "status":"placeholder"} -> [{id,title,category,status,owner,source}]
//	GET  /openapi.json  OpenAPI 3.1 schema
//	GET  /healthz       liveness probe (always 200)
//	GET  /              human-readable banner
//
// Authentication: a single static bearer token is required on all
// endpoints except /healthz and /openapi.json. The token is supplied
// via --api-key flag or MEERKAT_API_KEY env. The server refuses to
// start with no key configured (no anonymous mode).
package http

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// Config controls how the HTTP server is constructed. Zero values
// yield safe defaults except APIKey, which is required.
type Config struct {
	// Addr is the bind address ("host:port"). Empty means ":4004".
	Addr string
	// APIKey is the static bearer token. Required; the constructor
	// returns an error if it's empty.
	APIKey string
	// ReadTimeout limits how long the server waits for a request
	// body. Defaults to 10s.
	ReadTimeout time.Duration
	// WriteTimeout limits how long the server has to write a
	// response. Defaults to 30s.
	WriteTimeout time.Duration
	// QueryTimeout bounds how long a single /search query may run
	// server-side, regardless of how long the client is willing to
	// wait. It is applied as a deadline on the context derived from
	// the request context, so it composes with an early client
	// disconnect — either one stops the underlying bleve search (see
	// search.Index.QueryContext). Defaults to search.DefaultQueryTimeout.
	// Keep it comfortably below WriteTimeout so a query that's allowed
	// to finish still has time to write its response.
	QueryTimeout time.Duration
	// Version is the meerkat version surfaced in /openapi.json and
	// the root banner. Empty falls back to "dev".
	Version string
}

// Server bundles the HTTP server and its in-memory search index so
// callers can manage lifecycles together.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	handler http.Handler // authGate(mux) — see routes/Handler
	srv     *http.Server
	idx     *search.Index
}

// New builds a Server with the search index already populated.
// Returns an error if APIKey is empty. Caller must call Close to
// release the index when done.
func New(cfg Config) (*Server, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("APIKey is required (set --api-key or MEERKAT_API_KEY)")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":4004"
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.QueryTimeout == 0 {
		cfg.QueryTimeout = search.DefaultQueryTimeout
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}

	idx, err := search.New()
	if err != nil {
		return nil, fmt.Errorf("build search index: %w", err)
	}

	s := &Server{cfg: cfg, idx: idx, mux: http.NewServeMux()}
	s.routes()
	s.srv = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	return s, nil
}

// Handler exposes the auth-gated mux for tests (and ListenAndServe,
// via s.srv.Handler set in New) — see routes/authGate for how the gate
// composes with route registration.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr returns the configured listen address (post-default).
func (s *Server) Addr() string { return s.cfg.Addr }

// ListenAndServe blocks until the server stops. Honours ctx
// cancellation by issuing a graceful shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// Close releases the search index. Safe to call multiple times.
func (s *Server) Close() error {
	if s.idx == nil {
		return nil
	}
	err := s.idx.Close()
	s.idx = nil
	return err
}

// route pairs a URL pattern (Go 1.22+ ServeMux syntax, e.g. "POST
// /search") with its handler and whether it's reachable without
// authentication. It's the single source of truth for both route
// registration (routes, below) and authGate's public allowlist, so a
// route can only skip authentication by explicitly saying so here —
// there's no separate per-handler wrapper for a new route to forget.
// routeTable in server_test.go's TestAuth_DenyByDefault walks this same
// table, so a new route that leaves public unset (the zero value) is
// automatically asserted to require auth, and a route wrongly marked
// public is caught if it doesn't match the three documented exceptions.
type route struct {
	pattern string
	public  bool
	handler http.HandlerFunc
}

// routeTable is every pattern this server registers. Add new routes
// here, not via a direct s.mux.Handle/HandleFunc call, so they pick up
// routes' and authGate's behaviour (and TestAuth_DenyByDefault's
// coverage) automatically.
func (s *Server) routeTable() []route {
	return []route{
		{pattern: "POST /search", handler: s.handleSearch},
		{pattern: "POST /show", handler: s.handleShow},
		{pattern: "POST /list", handler: s.handleList},
		// Public endpoints — needed by OpenWebUI / health probes.
		{pattern: "GET /openapi.json", public: true, handler: s.handleOpenAPI},
		{pattern: "GET /healthz", public: true, handler: s.handleHealthz},
		{pattern: "GET /", public: true, handler: s.handleRoot},
	}
}

func (s *Server) routes() {
	public := make(map[string]bool)
	for _, rt := range s.routeTable() {
		s.mux.HandleFunc(rt.pattern, rt.handler)
		if rt.public {
			public[rt.pattern] = true
		}
	}
	s.handler = s.authGate(public)
}

// authGate wraps s.mux so every request is authenticated by default;
// public is the explicit allowlist of patterns exempted (built from
// routeTable's `public: true` entries). This is deny-by-default:
// nothing about how a pattern got registered on s.mux exempts it from
// auth except appearing in public, so a route added without updating
// routeTable — or added to routeTable without an explicit `public:
// true` — is auth-gated automatically rather than relying on every
// call site remembering to wrap itself.
//
// A request matching no registered pattern at all resolves to pattern
// == "" (see http.ServeMux.Handler's doc comment), which is also not
// in public, so an unauthenticated caller gets 401 rather than a 404
// that would otherwise confirm which paths don't exist.
func (s *Server) authGate(public map[string]bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := s.mux.Handler(r)
		if public[pattern] {
			s.mux.ServeHTTP(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header (expected 'Bearer <key>')")
			return
		}
		got := []byte(strings.TrimPrefix(header, prefix))
		want := []byte(s.cfg.APIKey)
		// ConstantTimeCompare requires equal lengths to be useful;
		// it returns 0 for unequal lengths so the check is safe but
		// also cheaply rejects different lengths early.
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// --- request/response payloads -------------------------------

type searchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type searchHit struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Category string  `json:"category"`
	Status   string  `json:"status"`
	Score    float64 `json:"score"`
	Snippet  string  `json:"snippet"`
}

type showRequest struct {
	ID string `json:"id"`
}

type listRequest struct {
	Prefix   string `json:"prefix,omitempty"`
	Category string `json:"category,omitempty"`
	Status   string `json:"status,omitempty"`
	Owner    string `json:"owner,omitempty"`
}

type listEntry struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Category string    `json:"category"`
	Status   string    `json:"status"`
	Owner    string    `json:"owner,omitempty"`
	Source   kb.Source `json:"source,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// --- handlers -----------------------------------------------

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}

	// QueryTimeout bounds this query's execution server-side even if
	// the client is happy to wait forever; an early client disconnect
	// cancels r.Context() itself, which stops the search sooner still.
	// Either way the deadline/cancellation reaches bleve's collector via
	// Index.QueryContext -> bleve.SearchInContext (see its doc comment).
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	results, err := s.idx.QueryContext(ctx, req.Query, req.Limit)
	if err != nil {
		switch {
		case errors.Is(err, search.ErrInvalidQuery):
			// A rejected input (oversized / pathologically nested query),
			// not an internal failure — see search.validateQuery.
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, context.DeadlineExceeded):
			writeError(w, http.StatusGatewayTimeout,
				"search: query exceeded the server's maximum query duration")
		default:
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("search: %v", err))
		}
		return
	}
	out := make([]searchHit, len(results))
	for i, r := range results {
		out[i] = searchHit{
			ID:       r.Page.ID,
			Title:    r.Page.Title,
			Category: r.Page.Front.Category,
			Status:   r.Page.Front.Status,
			Score:    r.Score,
			Snippet:  oneLine(r.Snippet),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	var req showRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	page, err := kb.Load(req.ID)
	if err != nil {
		if errors.Is(err, kb.ErrNotFound) {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("page %q not found - try POST /list", req.ID))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	var req listRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	pages, err := kb.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if f := kb.ByPrefix(req.Prefix); f != nil {
		pages = kb.Filter(pages, f)
	}
	if req.Category != "" {
		pages = kb.Filter(pages, kb.ByCategory(req.Category))
	}
	if req.Status != "" {
		pages = kb.Filter(pages, kb.ByStatus(req.Status))
	}
	if req.Owner != "" {
		pages = kb.Filter(pages, kb.ByOwner(req.Owner))
	}
	out := make([]listEntry, 0, len(pages))
	for _, p := range pages {
		out = append(out, listEntry{
			ID:       p.ID,
			Title:    p.Title,
			Category: p.Front.Category,
			Status:   p.Front.Status,
			Owner:    p.Front.Owner,
			Source:   p.Front.Source,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, openAPISchema(s.cfg.Version))
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "meerkat http server (version %s)\n\n", s.cfg.Version)
	fmt.Fprintln(w, "Endpoints (require Authorization: Bearer <api-key>):")
	fmt.Fprintln(w, "  POST /search   body: {\"query\": \"...\", \"limit\": 10}")
	fmt.Fprintln(w, "  POST /show     body: {\"id\": \"concepts/Rate-Limiting\"}")
	fmt.Fprintln(w, "  POST /list     body: {\"prefix\": \"systems/\", \"category\": \"...\", \"status\": \"...\"}")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Public endpoints (no auth):")
	fmt.Fprintln(w, "  GET  /openapi.json")
	fmt.Fprintln(w, "  GET  /healthz")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Register /openapi.json with OpenWebUI as a Tool Server.")
}

// --- helpers ------------------------------------------------

func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ResolveListenAddr picks a usable bind string from a host+port pair,
// returning the same shape http.Server expects.
func ResolveListenAddr(host string, port int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}
