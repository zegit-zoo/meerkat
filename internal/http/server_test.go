package http

import (
	"bytes"
	"encoding/json"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
