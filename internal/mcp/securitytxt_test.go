package mcp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/wellknown"
)

// securitytxt_test.go: `mk mcp serve-http` publishes security.txt to
// callers with no token, even behind OIDC, and not at all with
// NoSecurityTxt (#126).

func TestHostedSecurityTxt_ServedWithoutATokenBehindOIDC(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{
		rules: []authz.Rule{{Name: "all", Groups: []string{"team-a"}, Collections: []string{"*"}}},
	})
	resp := f.get(t, wellknown.SecurityTxtPath, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Contact: mailto:security@primitive-engineering.se") {
		t.Fatalf("status %d, body %q; want the file with no token", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	// The MCP endpoint itself is still gated: the file is the only new
	// thing a token-less caller can read.
	mcpResp := f.post(t, f.srv.EndpointPath(), nil)
	defer mcpResp.Body.Close()
	if mcpResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("MCP endpoint without a token = %d, want 401", mcpResp.StatusCode)
	}
}

func TestHostedSecurityTxt_OffWhenDisabled(t *testing.T) {
	reg, err := collections.New(collections.FromPages("notes", []kb.Page{testPage("p", "P", "body", "c", "reviewed", "o")}))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: reg, Version: "test", NoSecurityTxt: true,
		Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	code, body := get(t, ts, wellknown.SecurityTxtPath)
	if code == http.StatusOK || strings.Contains(body, "Contact:") {
		t.Errorf("a disabled security.txt answered %d: %s", code, body)
	}
}
