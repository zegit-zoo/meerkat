package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New(Config{APIKey: "test-key", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// newTestServerWithConfig builds a Server around an injected index and
// config, bypassing New's search.New() (which reads embedded/--kb-dir
// content unavailable to tests) so limit/timeout behavior can be tested
// against a known fixture corpus. Same-package test file, so it can
// reach Server's unexported fields directly — mirrors internal/mcp's
// pattern of calling searchHandler(idx) with an injected index.
func newTestServerWithConfig(t *testing.T, cfg Config, idx *search.Index) *Server {
	t.Helper()
	if cfg.QueryTimeout == 0 {
		cfg.QueryTimeout = search.DefaultQueryTimeout
	}
	s := &Server{cfg: cfg, idx: idx, mux: nethttp.NewServeMux()}
	s.routes()
	t.Cleanup(func() { _ = idx.Close() })
	return s
}

func postSearch(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(nethttp.MethodPost, "/search", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestNew_RequiresAPIKey rejects empty config.
func TestNew_RequiresAPIKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when APIKey is empty")
	}
}

// TestHealthz: no auth required, always 200.
func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/healthz", nil))
	if rec.Code != nethttp.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q", body["status"])
	}
}

// TestOpenAPI: no auth required, returns a parseable schema with the
// 3 paths we publish.
func TestOpenAPI(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/openapi.json", nil))
	if rec.Code != nethttp.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	var doc map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	paths, _ := doc["paths"].(map[string]any)
	for _, p := range []string{"/search", "/show", "/list"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("missing path: %s", p)
		}
	}
}

// TestAuth_Missing: data endpoints reject when no Authorization header.
func TestAuth_Missing(t *testing.T) {
	srv := newTestServer(t)
	for _, p := range []string{"/search", "/show", "/list"} {
		t.Run(p, func(t *testing.T) {
			req := httptest.NewRequest(nethttp.MethodPost, p, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != nethttp.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestAuth_Wrong: data endpoints reject a wrong bearer token.
func TestAuth_Wrong(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(nethttp.MethodPost, "/search",
		strings.NewReader(`{"query":"foo"}`))
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestSearch_Authed exercises the full happy path.
func TestSearch_Authed(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(nethttp.MethodPost, "/search",
		strings.NewReader(`{"query":"payment","limit":3}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var hits []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&hits); err != nil {
		t.Fatal(err)
	}
	// Don't fail the build if 'payment' has no hits in this snapshot,
	// but every hit (when present) must carry the documented shape.
	for _, h := range hits {
		for _, k := range []string{"id", "title", "score"} {
			if _, ok := h[k]; !ok {
				t.Errorf("hit missing field %q: %+v", k, h)
			}
		}
	}
}

// TestSearch_BadBody returns 400 on malformed JSON.
func TestSearch_BadBody(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(nethttp.MethodPost, "/search",
		strings.NewReader(`not json`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestSearch_MissingQuery returns 400 when query is empty.
func TestSearch_MissingQuery(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(nethttp.MethodPost, "/search",
		strings.NewReader(`{"query":""}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestSearch_QueryTooLong returns a clean 400 (not a 500 or a panic)
// for a query over the pre-parse length guard.
func TestSearch_QueryTooLong(t *testing.T) {
	srv := newTestServer(t)
	huge := strings.Repeat("x", 5000)
	rec := postSearch(t, srv, fmt.Sprintf(`{"query":%q}`, huge))
	if rec.Code != nethttp.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestSearch_QueryPathologicallyNested returns a clean 400 for the
// confirmed attack shape (a long run of nested parens), instead of
// burning CPU inside bleve's query-string parser.
func TestSearch_QueryPathologicallyNested(t *testing.T) {
	srv := newTestServer(t)
	nested := strings.Repeat("(", 100)
	rec := postSearch(t, srv, fmt.Sprintf(`{"query":%q}`, nested))
	if rec.Code != nethttp.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the documented error shape: %v", err)
	}
	if body.Error == "" {
		t.Error("expected a non-empty error message")
	}
}

// TestSearch_QueryTimeout proves QueryTimeout is actually wired into a
// context deadline: with an already-expired budget, the handler must
// answer 504 rather than hang or 500.
func TestSearch_QueryTimeout(t *testing.T) {
	idx, err := search.NewFromPages([]kb.Page{
		{ID: "a/b", Title: "T", Body: "some searchable body text", Front: kb.Frontmatter{Category: "concepts"}},
	})
	if err != nil {
		t.Fatalf("NewFromPages: %v", err)
	}
	srv := newTestServerWithConfig(t, Config{APIKey: "test-key", QueryTimeout: -1 * time.Second}, idx)

	rec := postSearch(t, srv, `{"query":"searchable"}`)
	if rec.Code != nethttp.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504, body=%s", rec.Code, rec.Body.String())
	}
}

// TestSearch_LimitClampedToMax proves the openapi-documented "maximum:
// 100" is actually enforced: against a fixture with more than MaxLimit
// matching pages, a huge requested limit must still come back capped.
func TestSearch_LimitClampedToMax(t *testing.T) {
	pages := make([]kb.Page, search.MaxLimit+50)
	for i := range pages {
		pages[i] = kb.Page{
			ID:    fmt.Sprintf("fixture/%d", i),
			Title: fmt.Sprintf("Widget %d", i),
			Body:  "widget widget shared searchable term",
			Front: kb.Frontmatter{Category: "concepts"},
		}
	}
	idx, err := search.NewFromPages(pages)
	if err != nil {
		t.Fatalf("NewFromPages: %v", err)
	}
	srv := newTestServerWithConfig(t, Config{APIKey: "test-key"}, idx)

	rec := postSearch(t, srv, `{"query":"widget","limit":100000}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var hits []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != search.MaxLimit {
		t.Errorf("got %d hits for limit=100000 against %d matching pages, want exactly MaxLimit=%d",
			len(hits), len(pages), search.MaxLimit)
	}
}

// TestOpenAPI_LimitMatchesEnforcement is a regression test for the
// exact mismatch this fix closes: the documented schema bounds must
// equal what the server enforces, always — not just today.
func TestOpenAPI_LimitMatchesEnforcement(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/openapi.json", nil))

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	limitSchema := doc["paths"].(map[string]any)["/search"].(map[string]any)["post"].(map[string]any)["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["limit"].(map[string]any)
	if got := limitSchema["maximum"].(float64); got != float64(search.MaxLimit) {
		t.Errorf("openapi.json documents limit maximum=%v, want search.MaxLimit=%d", got, search.MaxLimit)
	}
	if got := limitSchema["default"].(float64); got != float64(search.DefaultLimit) {
		t.Errorf("openapi.json documents limit default=%v, want search.DefaultLimit=%d", got, search.DefaultLimit)
	}
}

// TestList_Authed_NoBody: empty body is allowed (lists everything).
func TestList_Authed_NoBody(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(nethttp.MethodPost, "/list", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var entries []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		// Empty means no kb content embedded (content stripped); the
		// endpoint still returned 200 + a valid JSON array, which is what
		// this test checks.
		t.Skip("no kb content embedded")
	}
}

// TestShow_NotFound: 404 surfaces with helpful message.
func TestShow_NotFound(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(nethttp.MethodPost, "/show",
		strings.NewReader(`{"id":"nope/missing"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "not found") {
		t.Errorf("expected 'not found' in body, got %s", body)
	}
}

// TestResolveListenAddr defaults host to 0.0.0.0 if empty.
func TestResolveListenAddr(t *testing.T) {
	if got := ResolveListenAddr("", 4004); got != "0.0.0.0:4004" {
		t.Errorf("got %q", got)
	}
	if got := ResolveListenAddr("127.0.0.1", 4004); got != "127.0.0.1:4004" {
		t.Errorf("got %q", got)
	}
}
