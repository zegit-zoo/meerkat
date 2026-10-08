package http

import (
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "test-key-0123456789"

func do(srv *Server, method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// An unauthenticated GET to a path the server does not serve is denied
// like any other unauthenticated request, so a prober cannot tell real
// routes from nothing; with the key the same path is an ordinary 404.
func TestAuth_UnknownGETDeniedLikePOST(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/no-such-route", "/search", "/show", "/list", "/collections/extra", "/.well-known/other"} {
		if rec := do(srv, nethttp.MethodGet, path, ""); rec.Code != nethttp.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", path, rec.Code)
		}
	}
	if rec := do(srv, nethttp.MethodGet, "/no-such-route", "Bearer "+testKey); rec.Code != nethttp.StatusNotFound {
		t.Errorf("authenticated GET /no-such-route = %d, want 404", rec.Code)
	}
	// The root banner itself stays public.
	if rec := do(srv, nethttp.MethodGet, "/", ""); rec.Code != nethttp.StatusOK {
		t.Errorf("GET / = %d, want 200", rec.Code)
	}
}

// RFC 7235 §2.1: the auth scheme is case-insensitive.
func TestAuth_SchemeIsCaseInsensitive(t *testing.T) {
	srv := newTestServer(t)
	for _, tc := range []struct {
		header string
		want   int
	}{
		{"Bearer " + testKey, nethttp.StatusOK},
		{"bearer " + testKey, nethttp.StatusOK},
		{"BEARER " + testKey, nethttp.StatusOK},
		{"bEaReR " + testKey, nethttp.StatusOK},
		{"Bearer " + testKey + "x", nethttp.StatusUnauthorized},
		{"Bearer " + testKey[:len(testKey)-1], nethttp.StatusUnauthorized},
		{"Bearer ", nethttp.StatusUnauthorized},
		{"Basic " + testKey, nethttp.StatusUnauthorized},
		{testKey, nethttp.StatusUnauthorized},
	} {
		if rec := do(srv, nethttp.MethodGet, "/collections", tc.header); rec.Code != tc.want {
			t.Errorf("Authorization %q = %d, want %d", tc.header, rec.Code, tc.want)
		}
	}
}

func TestResponses_CarryNosniff(t *testing.T) {
	srv := newTestServer(t)
	for _, tc := range []struct{ path, auth string }{
		{"/", ""}, {"/healthz", ""}, {"/openapi.json", ""}, {"/collections", ""},
		{"/collections", "Bearer " + testKey}, {"/no-such-route", ""}, {"/.well-known/security.txt", ""},
	} {
		rec := do(srv, nethttp.MethodGet, tc.path, tc.auth)
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s (%d): X-Content-Type-Options = %q, want nosniff", tc.path, rec.Code, got)
		}
	}
}

func TestWriteInternalError_IsFixedText(t *testing.T) {
	rec := httptest.NewRecorder()
	writeInternalError(rec, "show", errors.New(`open /srv/kb/secret-collection/page.md: permission denied`))
	if rec.Code != nethttp.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "secret-collection") || strings.Contains(body, "permission") ||
		!strings.Contains(body, `"internal error"`) {
		t.Errorf("500 body must be fixed text, got %s", body)
	}
}

func TestNew_RefusesShortAPIKey(t *testing.T) {
	for _, key := range []string{"", "x", "test-key", strings.Repeat("a", MinAPIKeyLength-1)} {
		if _, err := New(Config{APIKey: key, Version: "test"}); err == nil {
			t.Errorf("New accepted a %d-character key", len(key))
		}
	}
	_, err := New(Config{APIKey: "short", Version: "test"})
	if err == nil || !strings.Contains(err.Error(), "too short") || !strings.Contains(err.Error(), "openssl rand") {
		t.Errorf("err = %v, want a clear too-short message with a fix", err)
	}
	srv, err := New(Config{APIKey: strings.Repeat("a", MinAPIKeyLength), Version: "test"})
	if err != nil {
		t.Fatalf("a key of exactly %d characters must be accepted: %v", MinAPIKeyLength, err)
	}
	_ = srv.Close()
}
