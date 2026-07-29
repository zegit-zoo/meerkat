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
	"testing/fstest"
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

func postShow(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(nethttp.MethodPost, "/show", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func postList(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(nethttp.MethodPost, "/list", strings.NewReader(body))
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

// TestAuth_DenyByDefault is the regression test for the finding that
// auth was applied per-handler (each of /search, /show, /list
// individually wrapped in requireAuth) with nothing structural
// preventing a new route from forgetting the wrapper — and that the
// previous version of this test (a hardcoded []string{"/search",
// "/show", "/list"}) wouldn't have caught that, since a forgotten route
// simply wouldn't appear in the list.
//
// It enumerates srv.routeTable() instead — the exact same table
// routes() uses to register every pattern on the mux — so a new
// pattern automatically gets a test case here with no edit to this
// function: one that defaults to "must require auth" (public is false
// unless the table says otherwise), and one that (for the three
// documented public exceptions) confirms the allowlist isn't
// accidentally denying a route meant to be public.
func TestAuth_DenyByDefault(t *testing.T) {
	srv := newTestServer(t)
	for _, rt := range srv.routeTable() {
		t.Run(rt.pattern, func(t *testing.T) {
			method, path, ok := strings.Cut(rt.pattern, " ")
			if !ok {
				t.Fatalf("route pattern %q missing a %q-separated method prefix", rt.pattern, " ")
			}
			var body io.Reader
			if method != nethttp.MethodGet {
				body = strings.NewReader(`{}`)
			}
			req := httptest.NewRequest(method, path, body)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rt.public {
				if rec.Code == nethttp.StatusUnauthorized {
					t.Errorf("public route unexpectedly requires auth (status 401): %s", rt.pattern)
				}
				return
			}
			if rec.Code != nethttp.StatusUnauthorized {
				t.Errorf("status = %d, want 401 for non-public route %s", rec.Code, rt.pattern)
			}
		})
	}
}

// TestAuth_UnknownRouteDeniedByDefault extends TestAuth_DenyByDefault
// past routeTable's known patterns: a request matching no registered
// pattern at all resolves to pattern == "" (see http.ServeMux.Handler's
// doc comment), which authGate also treats as non-public — so it's
// auth-gated too, rather than falling through to an unauthenticated 404
// that would confirm to a prober which paths don't exist.
func TestAuth_UnknownRouteDeniedByDefault(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(nethttp.MethodPost, "/no-such-route", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != nethttp.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for an unregistered route", rec.Code)
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

// TestOpenAPI_ShowDocumentsTrustTierEnumAndStale mirrors
// TestOpenAPI_LimitMatchesEnforcement's invariant: the documented
// trust_tier enum must equal the exact set of values
// kb.Frontmatter.TrustTier can return, always — not just today — and
// stale must be documented as a boolean.
func TestOpenAPI_ShowDocumentsTrustTierEnumAndStale(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/openapi.json", nil))

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	showProps := doc["paths"].(map[string]any)["/show"].(map[string]any)["post"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)

	trustTier, ok := showProps["trust_tier"].(map[string]any)
	if !ok {
		t.Fatalf("openapi.json /show response schema is missing trust_tier: %v", showProps["trust_tier"])
	}
	rawEnum, ok := trustTier["enum"].([]any)
	if !ok {
		t.Fatalf("trust_tier enum is not a JSON array: %T", trustTier["enum"])
	}
	var got []string
	for _, v := range rawEnum {
		got = append(got, fmt.Sprintf("%v", v))
	}
	want := []string{kb.TrustUnverified, kb.TrustMachineConfirmed, kb.TrustHumanReviewed}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi.json documents trust_tier enum=%v, want %v (kb.Trust* constants)", got, want)
	}

	stale, ok := showProps["stale"].(map[string]any)
	if !ok {
		t.Fatalf("openapi.json /show response schema is missing stale: %v", showProps["stale"])
	}
	if stale["type"] != "boolean" {
		t.Errorf("openapi.json documents stale type=%v, want boolean", stale["type"])
	}
}

// TestOpenAPI_ListDocumentsTypeFilterAndField guards the /list schema
// additions: a "type" request filter property (mirroring
// prefix/category/status/owner) and a "type" response item property.
func TestOpenAPI_ListDocumentsTypeFilterAndField(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(nethttp.MethodGet, "/openapi.json", nil))

	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	listPost := doc["paths"].(map[string]any)["/list"].(map[string]any)["post"].(map[string]any)

	reqProps := listPost["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := reqProps["type"]; !ok {
		t.Errorf("openapi.json /list request schema is missing the %q filter property", "type")
	}

	respProps := listPost["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := respProps["type"]; !ok {
		t.Errorf("openapi.json /list response items schema is missing the %q property", "type")
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

// TestList_Authed_IncludesTypeAndFiltersByType drives the full POST
// /list handler end to end against injected KB content: the response
// now carries the frontmatter "type" field (previously dropped — see
// listEntry), and a "type" request filter narrows the result the same
// way --type (internal/cli/list.go) and mk_list's "type" argument
// (internal/mcp/server.go) do.
func TestList_Authed_IncludesTypeAndFiltersByType(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md":    {Data: []byte("---\nid: tables/orders\ntitle: Orders\ntype: BigQuery Table\n---\nbody\n")},
		"content/playbooks/oncall.md": {Data: []byte("---\nid: playbooks/oncall\ntitle: Oncall\ntype: Playbook\n---\nbody\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })
	srv := newTestServer(t)

	// No filter: both entries come back, each carrying its own type.
	rec := postList(t, srv, `{}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var all []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	gotTypes := map[string]string{}
	for _, e := range all {
		gotTypes[fmt.Sprintf("%v", e["id"])] = fmt.Sprintf("%v", e["type"])
	}
	if gotTypes["tables/orders"] != "BigQuery Table" {
		t.Errorf("tables/orders type = %q, want %q", gotTypes["tables/orders"], "BigQuery Table")
	}
	if gotTypes["playbooks/oncall"] != "Playbook" {
		t.Errorf("playbooks/oncall type = %q, want %q", gotTypes["playbooks/oncall"], "Playbook")
	}

	// Filtered: only the matching type comes back. A body containing
	// "type" being accepted at all (rather than 400 from
	// DisallowUnknownFields) is itself part of what this proves.
	rec = postList(t, srv, `{"type":"BigQuery Table"}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var filtered []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(filtered) != 1 || filtered[0]["id"] != "tables/orders" {
		t.Errorf(`POST /list {"type":"BigQuery Table"} = %v, want exactly [tables/orders]`, filtered)
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

// TestNewShowResponse_Shape proves the POST /show wire shape carries
// the page's own fields (id, front.type, front.description, ...)
// alongside the two OKF-derived advisory signals (trust_tier, stale)
// that aren't stored fields on kb.Page — mirroring MCP's
// TestShowPageJSON_Shape (internal/mcp/handlers_test.go).
func TestNewShowResponse_Shape(t *testing.T) {
	page := kb.Page{
		ID:    "tables/orders",
		Title: "Orders",
		Body:  "body",
		Front: kb.Frontmatter{
			ID:          "tables/orders",
			Type:        "BigQuery Table",
			Description: "One row per order.",
			StaleAfter:  "2020-01-01",
			Verified:    kb.VerifiedList{{By: "human:ahormati"}},
		},
	}
	raw, err := json.Marshal(newShowResponse(page))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	if parsed["id"] != "tables/orders" {
		t.Errorf("id = %v", parsed["id"])
	}
	if parsed["trust_tier"] != kb.TrustHumanReviewed {
		t.Errorf("trust_tier = %v, want %q", parsed["trust_tier"], kb.TrustHumanReviewed)
	}
	if parsed["stale"] != true {
		t.Errorf("stale = %v, want true", parsed["stale"])
	}
	front, ok := parsed["front"].(map[string]any)
	if !ok {
		t.Fatalf("front is not an object: %v", parsed["front"])
	}
	if front["type"] != "BigQuery Table" {
		t.Errorf("front.type = %v", front["type"])
	}
	if front["description"] != "One row per order." {
		t.Errorf("front.description = %v", front["description"])
	}
}

// TestShow_Authed_IncludesTrustTierAndStale drives the full POST /show
// handler end to end against injected KB content, proving trust_tier
// and stale reach the wire — the gap this change closes (previously
// /show serialised kb.Page directly, which picks up type/description
// automatically but not the wrapper-level trust_tier/stale the CLI and
// MCP handlers add). Fixture mirrors internal/cli/show_test.go's
// TestShowCmd_JSONIncludesTrustTierAndStale.
func TestShow_Authed_IncludesTrustTierAndStale(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/orders.md": {Data: []byte(`---
id: tables/orders
title: Orders
type: BigQuery Table
description: One row per order.
status: stable
stale_after: 2020-01-01
verified:
  - { by: human:ahormati, at: 2026-06-25T09:00:00Z }
---
# Orders
`)},
	})
	t.Cleanup(func() { kb.UseFS(nil) })
	srv := newTestServer(t)

	rec := postShow(t, srv, `{"id":"tables/orders"}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, rec.Body.String())
	}
	if result["trust_tier"] != "human-reviewed" {
		t.Errorf("trust_tier = %v, want human-reviewed", result["trust_tier"])
	}
	if result["stale"] != true {
		t.Errorf("stale = %v, want true (stale_after 2020-01-01 is in the past)", result["stale"])
	}
	front, ok := result["front"].(map[string]any)
	if !ok {
		t.Fatalf("front is not an object: %v", result["front"])
	}
	if front["type"] != "BigQuery Table" {
		t.Errorf("front.type = %v", front["type"])
	}
	if front["description"] != "One row per order." {
		t.Errorf("front.description = %v", front["description"])
	}
}

// TestShow_Authed_UnverifiedAndNotStale covers the opposite corner: no
// verified key and no stale_after at all.
func TestShow_Authed_UnverifiedAndNotStale(t *testing.T) {
	kb.UseFS(fstest.MapFS{
		"content/tables/customers.md": {Data: []byte("---\nid: tables/customers\ntype: BigQuery Table\n---\n# Customers\n")},
	})
	t.Cleanup(func() { kb.UseFS(nil) })
	srv := newTestServer(t)

	rec := postShow(t, srv, `{"id":"tables/customers"}`)
	if rec.Code != nethttp.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if result["trust_tier"] != "unverified" {
		t.Errorf("trust_tier = %v, want unverified", result["trust_tier"])
	}
	if result["stale"] != false {
		t.Errorf("stale = %v, want false", result["stale"])
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
