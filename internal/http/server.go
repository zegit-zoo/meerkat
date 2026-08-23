// Package http exposes the embedded knowledge base over an
// HTTP/JSON API designed to be consumed by OpenWebUI tool servers
// and similar clients.
//
// The endpoint surface mirrors the MCP tool set 1:1 so that an
// OpenWebUI tool definition matches what an MCP client sees:
//
//	POST /search        body: {"query":"...", "limit":10, "collection":"..."} -> [{id,collection,title,score,snippet,category,status}]
//	POST /show          body: {"id":"concepts/Rate-Limiting", "collection":"..."} -> {id,collection,title,body,front,trust_tier,stale}
//	POST /list          body: {"prefix":"systems/", "category":"systems", "status":"placeholder", "type":"BigQuery Table", "collection":"..."} -> [{id,collection,title,category,status,owner,type,source}]
//	GET  /collections   the mounted collections
//	GET  /openapi.json  OpenAPI 3.1 schema
//	GET  /healthz       liveness probe (always 200)
//	GET  /              human-readable banner
//
// "collection" is optional everywhere: omitted, /search and /list span
// every mounted collection and /show resolves across them (an ID present
// in several is a 409, naming the qualified IDs to choose from — an ID
// may be written "<collection>:<page-id>").
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

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/kbdir"
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
	// Collections is the set of knowledge-base collections this server
	// serves. Nil falls back to a single collection over the
	// process-global KB filesystem (internal/collections.Global), which
	// is what every caller that predates collections effectively asked
	// for.
	Collections *collections.Registry
}

// Server bundles the HTTP server and its collections (each with its own
// in-memory search index) so callers can manage lifecycles together.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	handler http.Handler // authGate(mux) — see routes/Handler
	srv     *http.Server
	reg     *collections.Registry
}

// New builds a Server with every collection's search index already
// populated. Returns an error if APIKey is empty. Caller must call
// Close to release the indexes when done.
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

	reg := cfg.Collections
	if reg == nil {
		reg = collections.Global(kbdir.SourceEmbedded)
	}
	// Index every collection now rather than on first query: a server
	// should pay that cost at startup and refuse to start if a
	// collection can't be indexed, not surface it as a 500 later.
	for _, c := range reg.All() {
		if _, err := c.Index(); err != nil {
			return nil, fmt.Errorf("build search index for collection %q: %w", c.Name, err)
		}
	}

	s := &Server{cfg: cfg, reg: reg, mux: http.NewServeMux()}
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

// Close releases every collection's search index. Safe to call
// multiple times.
func (s *Server) Close() error {
	if s.reg == nil {
		return nil
	}
	err := s.reg.Close()
	s.reg = nil
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
		// Auth-gated like the data endpoints: which collections a
		// deployment mounts (and where their content comes from) is
		// itself information an anonymous caller has no business having.
		{pattern: "GET /collections", handler: s.handleCollections},
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
	// Collection restricts the query to one mounted collection. Empty
	// (the default, and the only possibility before collections existed)
	// queries every one.
	Collection string `json:"collection,omitempty"`
}

// searchHit carries the page's own unqualified ID plus the collection
// it came from — the collection is never folded into the ID, so a
// client that round-trips an id is unaffected by how many collections
// a deployment mounts.
type searchHit struct {
	ID         string  `json:"id"`
	Collection string  `json:"collection"`
	Title      string  `json:"title"`
	Category   string  `json:"category"`
	Status     string  `json:"status"`
	Score      float64 `json:"score"`
	Snippet    string  `json:"snippet"`
}

type showRequest struct {
	ID string `json:"id"`
	// Collection restricts the lookup to one mounted collection. ID may
	// also be written "<collection>:<page-id>"; both together must agree.
	Collection string `json:"collection,omitempty"`
}

// collectionEntry is one item of GET /collections.
type collectionEntry struct {
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
	Source string `json:"source"`
	Pages  int    `json:"pages"`
}

// showResponse is the POST /show wire shape: the page's stored fields
// (embedded, so id/path/title/body/front are top-level exactly as
// before this change) plus the collection it came from and two
// OKF-derived advisory signals that are computed rather than stored —
// see kb.Frontmatter.TrustTier / IsStale. Field names and JSON keys are
// deliberately identical to showResult (internal/cli/show.go) and
// showPageOutput (internal/mcp/server.go) so all three surfaces agree
// on what collection/trust_tier/stale are called.
type showResponse struct {
	kb.Page
	Collection string `json:"collection"`
	TrustTier  string `json:"trust_tier"`
	Stale      bool   `json:"stale"`
}

// newShowResponse builds the POST /show payload for a page reference,
// mirroring internal/cli/show.go's newShowResult.
func newShowResponse(ref collections.PageRef) showResponse {
	return showResponse{
		Page:       ref.Page,
		Collection: ref.Collection,
		TrustTier:  ref.Page.Front.TrustTier(),
		Stale:      ref.Page.Front.IsStale(time.Now().UTC()),
	}
}

type listRequest struct {
	Prefix   string `json:"prefix,omitempty"`
	Category string `json:"category,omitempty"`
	Status   string `json:"status,omitempty"`
	Owner    string `json:"owner,omitempty"`
	// Type filters on frontmatter 'type' (OKF's concept-kind field,
	// SPEC.md §4.1), matching the --type flag (internal/cli/list.go)
	// and mk_list's "type" argument (internal/mcp/server.go).
	Type string `json:"type,omitempty"`
	// Collection restricts the listing to one mounted collection. Empty
	// lists every one, in configuration order.
	Collection string `json:"collection,omitempty"`
}

type listEntry struct {
	ID         string `json:"id"`
	Collection string `json:"collection"`
	Title      string `json:"title"`
	Category   string `json:"category"`
	Status     string `json:"status"`
	Owner      string `json:"owner,omitempty"`
	// Type is frontmatter 'type' (OKF's concept-kind field), matching
	// the "type" key `mk list --json` and mk_list already emit.
	Type   string    `json:"type,omitempty"`
	Source kb.Source `json:"source,omitempty"`
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

	results, err := s.reg.Search(ctx, req.Collection, req.Query, req.Limit)
	if err != nil {
		switch {
		case errors.Is(err, search.ErrInvalidQuery), errors.Is(err, collections.ErrUnknownCollection):
			// A rejected input (oversized / pathologically nested query,
			// or a collection that isn't mounted), not an internal
			// failure — see search.validateQuery / Registry.Get.
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
			ID:         r.Page.ID,
			Collection: r.Collection,
			Title:      r.Page.Title,
			Category:   r.Page.Front.Category,
			Status:     r.Page.Front.Status,
			Score:      r.Score,
			Snippet:    oneLine(r.Snippet),
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
	ref, err := s.reg.Show(req.Collection, req.ID)
	if err != nil {
		switch {
		case errors.Is(err, kb.ErrNotFound):
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("page %q not found - try POST /list", req.ID))
		case errors.Is(err, collections.ErrUnknownCollection):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, collections.ErrAmbiguous):
			// 409 Conflict, not 404 or 500: the page exists, more than
			// once, and the error text names the qualified IDs the client
			// should re-request with.
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, newShowResponse(ref))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	var req listRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	refs, err := s.reg.Pages(req.Collection)
	if err != nil {
		if errors.Is(err, collections.ErrUnknownCollection) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	keep := func(f kb.FilterFunc) {
		if f == nil {
			return
		}
		out := refs[:0:0]
		for _, ref := range refs {
			if f(ref.Page) {
				out = append(out, ref)
			}
		}
		refs = out
	}
	keep(kb.ByPrefix(req.Prefix))
	if req.Category != "" {
		keep(kb.ByCategory(req.Category))
	}
	if req.Status != "" {
		keep(kb.ByStatus(req.Status))
	}
	if req.Owner != "" {
		keep(kb.ByOwner(req.Owner))
	}
	if req.Type != "" {
		keep(kb.ByType(req.Type))
	}
	out := make([]listEntry, 0, len(refs))
	for _, ref := range refs {
		p := ref.Page
		out = append(out, listEntry{
			ID:         p.ID,
			Collection: ref.Collection,
			Title:      p.Title,
			Category:   p.Front.Category,
			Status:     p.Front.Status,
			Owner:      p.Front.Owner,
			Type:       p.Front.Type,
			Source:     p.Front.Source,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCollections enumerates what this server mounts — the HTTP
// counterpart of `mk list --collections`, and the discovery surface for
// the "collection" field every other endpoint accepts.
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	out := make([]collectionEntry, 0, s.reg.Len())
	for _, c := range s.reg.All() {
		e := collectionEntry{Name: c.Name, Type: c.Source.Type, Source: c.Provenance}
		if pages, err := c.Pages(); err == nil {
			e.Pages = len(pages)
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, openAPISchema(s.cfg.Version, s.reg.Names()))
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
	fmt.Fprintln(w, "  GET  /collections")
	if !s.reg.Single() {
		fmt.Fprintf(w, "\nMounted collections (pass \"collection\" to narrow, omit to span all):\n  %s\n",
			strings.Join(s.reg.Names(), ", "))
	}
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
