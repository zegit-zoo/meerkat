package http

import (
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/wellknown"
)

// securitytxt_test.go: `mk http serve` publishes security.txt without
// authentication by default, and not at all with NoSecurityTxt (#126).

func TestSecurityTxt_ServedWithoutAuth(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, wellknown.SecurityTxtPath, nil))
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, want 200 with no Authorization header", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Contact: mailto:security@primitive-engineering.se") {
		t.Errorf("body = %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestSecurityTxt_OffWhenDisabled(t *testing.T) {
	srv, err := New(Config{APIKey: "test-key", Version: "test", NoSecurityTxt: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	for _, auth := range []string{"", "Bearer test-key"} {
		req := httptest.NewRequest(nethttp.MethodGet, wellknown.SecurityTxtPath, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == nethttp.StatusOK || strings.Contains(rec.Body.String(), "Contact:") {
			t.Errorf("auth %q: a disabled security.txt answered %d: %s", auth, rec.Code, rec.Body.String())
		}
	}
}
