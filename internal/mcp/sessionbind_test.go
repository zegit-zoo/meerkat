package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/authn/authntest"
	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/memory"
	"github.com/zegit-zoo/meerkat/internal/retrieval"
)

// sessionbind_test.go: no caller can observe or alter another
// principal's session state by guessing or reusing an identifier
// (meerkat-mob#46) — neither a session_id tool argument nor an MCP
// session ID.

// bindFixture is a hosted server with OIDC, a retrieval tracker, and a
// way to speak raw Streamable HTTP to it.
type bindFixture struct {
	srv    *HostedServer
	http   *httptest.Server
	issuer *authntest.Issuer
}

func newBindFixture(t *testing.T, stateful bool, limits retrieval.Limits) *bindFixture {
	t.Helper()
	f := &bindFixture{issuer: authntest.NewIssuer(t)}
	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections: threeCollectionRegistry(t),
		Auth: &authz.Config{
			Resource:  testResource,
			Providers: []authz.Provider{{Issuer: f.issuer.URL, Audience: testAudience}},
			Rules:     []authz.Rule{{Name: "all", Groups: []string{"staff"}, Collections: []string{"*"}}},
		},
		Version:    "test",
		HTTPClient: f.issuer.Client(),
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Stateful:   stateful,
		Outcome:    OutcomeOptions{Sessions: retrieval.New(0, limits)},
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	f.srv = srv
	f.http = httptest.NewServer(srv.Handler())
	t.Cleanup(f.http.Close)
	return f
}

func (f *bindFixture) token(t *testing.T, subject string) string {
	t.Helper()
	return f.issuer.Token(t, authntest.Claims{Subject: subject, Audience: testAudience, Groups: []string{"staff"}})
}

// rpc sends one JSON-RPC message on the legacy (session-carrying)
// protocol and returns the status, the session ID header and the body.
func (f *bindFixture) rpc(t *testing.T, method, bearer, session, body string) (int, string, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.http.URL+f.srv.EndpointPath(), rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := f.http.Client().Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	if method == http.MethodGet && resp.StatusCode == http.StatusOK {
		// An open SSE stream: the status is the answer.
		return resp.StatusCode, resp.Header.Get("Mcp-Session-Id"), ""
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id"), string(b)
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func toolCallBody(name string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	return string(b)
}

func (f *bindFixture) initialize(t *testing.T, bearer string) string {
	t.Helper()
	code, sid, body := f.rpc(t, http.MethodPost, bearer, "", initBody)
	if code != http.StatusOK || sid == "" {
		t.Fatalf("initialize: %d sid=%q %s", code, sid, body)
	}
	return sid
}

func TestSessionBinding_MCPSessionIDIsRefusedToAnotherPrincipal(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		name := "stateless"
		if stateful {
			name = "stateful"
		}
		t.Run(name, func(t *testing.T) {
			f := newBindFixture(t, stateful, retrieval.Limits{})
			alice, bob := f.token(t, "alice"), f.token(t, "bob")
			aliceSID := f.initialize(t, alice)
			list := `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`

			if code, _, body := f.rpc(t, http.MethodPost, alice, aliceSID, list); code != http.StatusOK {
				t.Fatalf("alice on her own session: %d %s", code, body)
			}
			for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
				body := ""
				if m == http.MethodPost {
					body = list
				}
				if code, _, _ := f.rpc(t, m, bob, aliceSID, body); code != http.StatusNotFound {
					t.Errorf("bob %s with alice's session ID: %d, want 404", m, code)
				}
			}
			// Bob's DELETE did not end alice's session.
			if code, _, body := f.rpc(t, http.MethodPost, alice, aliceSID, list); code != http.StatusOK {
				t.Errorf("alice after bob's DELETE: %d %s", code, body)
			}
			// Her own DELETE does.
			if code, _, _ := f.rpc(t, http.MethodDelete, alice, aliceSID, ""); code != http.StatusOK {
				t.Errorf("alice DELETE: %d", code)
			}
		})
	}
}

func TestSessionBinding_UnboundOrForgedIDsAreUnknown(t *testing.T) {
	f := newBindFixture(t, false, retrieval.Limits{})
	bob := f.token(t, "bob")
	list := `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`
	aliceNS := memory.Namespace(authz.Identity{Subject: "alice", Issuer: f.issuer.URL})
	for name, sid := range map[string]string{
		"mcp-go's own shape":       "mcp-session-123e4567-e89b-12d3-a456-426614174000",
		"garbage":                  "q1",
		"bound to someone else":    newSessionID(aliceNS),
		"tag truncated":            newSessionID(aliceNS)[:40],
		"anonymous principal's ID": newSessionID(memory.Namespace(authz.Identity{})),
	} {
		if code, _, _ := f.rpc(t, http.MethodPost, bob, sid, list); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, code)
		}
	}
}

func TestSessionBinding_StatefulRefusesAnIDItNeverIssued(t *testing.T) {
	f := newBindFixture(t, true, retrieval.Limits{})
	bob := f.token(t, "bob")
	// Correctly bound to bob, but never issued by this process.
	bobNS := memory.Namespace(authz.Identity{Subject: "bob", Issuer: f.issuer.URL})
	sid := newSessionID(bobNS)
	if code, _, _ := f.rpc(t, http.MethodPost, bob, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); code != http.StatusNotFound {
		t.Errorf("stateful mode accepted an ID it never issued: %d", code)
	}
}

func TestSessionBinding_SessionIDArgumentIsScopedToThePrincipal(t *testing.T) {
	// Two steps per session. Alice spends hers under session_id "q1";
	// bob, using the same "q1", must still have his.
	f := newBindFixture(t, false, retrieval.Limits{MaxSteps: 2})
	alice, bob := f.token(t, "alice"), f.token(t, "bob")
	aliceSID, bobSID := f.initialize(t, alice), f.initialize(t, bob)
	search := func(bearer, sid string) string {
		code, _, body := f.rpc(t, http.MethodPost, bearer, sid,
			toolCallBody(toolSearch, map[string]any{"query": "overview", "session_id": "q1"}))
		if code != http.StatusOK {
			t.Fatalf("search: %d %s", code, body)
		}
		return body
	}
	for range 3 {
		search(alice, aliceSID)
	}
	if !strings.Contains(search(alice, aliceSID), "limit_reached") {
		t.Fatal("alice never reached her step limit; the test proves nothing")
	}
	if body := search(bob, bobSID); strings.Contains(body, "limit_reached") {
		t.Errorf("bob's session q1 was spent by alice's calls: %s", body)
	}
	// Bob ending "q1" ends HIS session, not alice's: hers is still spent.
	code, _, body := f.rpc(t, http.MethodPost, bob, bobSID,
		toolCallBody(toolReportOutcome, map[string]any{"session_id": "q1", "outcome": "found"}))
	if code != http.StatusOK || strings.Contains(body, `"isError":true`) {
		t.Fatalf("bob's report: %d %s", code, body)
	}
	if !strings.Contains(search(alice, aliceSID), "limit_reached") {
		t.Error("bob's report ended alice's session q1")
	}
}

func TestSessionKeys_DifferByPrincipal(t *testing.T) {
	ctx := context.Background()
	a := authz.NewContext(ctx, authz.NewGrants(authz.Identity{Subject: "alice", Issuer: "https://idp"}, nil))
	b := authz.NewContext(ctx, authz.NewGrants(authz.Identity{Subject: "bob", Issuer: "https://idp"}, nil))
	other := authz.NewContext(ctx, authz.NewGrants(authz.Identity{Subject: "alice", Issuer: "https://other-idp"}, nil))

	if sessionKey(a, "q1") == sessionKey(b, "q1") || sessionKey(a, "q1") == sessionKey(other, "q1") {
		t.Error("the same session_id from two principals shares a retrieval session")
	}
	again := authz.NewContext(ctx, authz.NewGrants(authz.Identity{Subject: "alice", Issuer: "https://idp", Email: "new@example.com"}, nil))
	if sessionKey(a, "q1") != sessionKey(again, "q1") {
		t.Error("a principal's session key moved with a mutable claim")
	}
	if sessionKey(a, "") != "" {
		t.Error("no session_id and no MCP session must still mean no session")
	}
	if advisoryKey(a, "") == advisoryKey(b, "") || advisoryKey(a, "") == "" {
		t.Error("session-less advisory buckets are shared across principals")
	}
	if advisoryKey(a, "q1") == advisoryKey(b, "q1") {
		t.Error("advisories for the same session_id are shared across principals")
	}
}

func TestSessionBinder_StreamCapPerPrincipal(t *testing.T) {
	b := newSessionBinder(false, 1, 0)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	h := b.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	ctxFor := func(sub string) context.Context {
		return authz.NewContext(context.Background(), authz.NewGrants(authz.Identity{Subject: sub, Issuer: "i"}, nil))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctxFor("alice")))
	}()
	<-entered

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctxFor("alice")))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("alice's second stream: %d, want 429", rec.Code)
	}
	// Bob is not affected by alice's streams.
	go func() { <-entered }()
	recBob := httptest.NewRecorder()
	bobDone := make(chan struct{})
	go func() {
		defer close(bobDone)
		h.ServeHTTP(recBob, httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctxFor("bob")))
	}()
	close(release)
	<-done
	<-bobDone
	if recBob.Code != http.StatusOK {
		t.Errorf("bob's stream: %d, want 200", recBob.Code)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.streams) != 0 {
		t.Errorf("stream counts leaked: %v", b.streams)
	}
}

func TestSessionBinder_StatefulCapsEvictTheOldest(t *testing.T) {
	b := newSessionBinder(true, 2, 3)
	clock := time.Unix(0, 0)
	b.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	mgr := func(sub string) *boundManager {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(
			authz.NewContext(context.Background(), authz.NewGrants(authz.Identity{Subject: sub, Issuer: "i"}, nil)))
		return b.ResolveSessionIdManager(r).(*boundManager)
	}
	alice, bob := mgr("alice"), mgr("bob")
	a1, a2, a3 := alice.Generate(), alice.Generate(), alice.Generate()
	if _, err := alice.Validate(a1); err == nil {
		t.Error("alice's oldest ID survived her per-principal cap")
	}
	for _, id := range []string{a2, a3} {
		if _, err := alice.Validate(id); err != nil {
			t.Errorf("alice's recent ID %s was evicted: %v", id, err)
		}
	}
	b1 := bob.Generate()
	b2 := bob.Generate() // overall cap 3: evicts the oldest overall (a2)
	if _, err := alice.Validate(a2); err == nil {
		t.Error("the overall cap did not evict the oldest ID")
	}
	for _, id := range []string{b1, b2} {
		if _, err := bob.Validate(id); err != nil {
			t.Errorf("bob's ID evicted: %v", err)
		}
	}
	if _, err := bob.Validate(a3); err == nil {
		t.Error("bob validated alice's ID")
	}
	// Terminate by the owner frees the slot; a foreign terminate does not.
	_, _ = bob.Terminate(a3)
	if _, err := alice.Validate(a3); err != nil {
		t.Error("bob terminated alice's session")
	}
	_, _ = alice.Terminate(a3)
	if _, err := alice.Validate(a3); err == nil {
		t.Error("alice's own terminate did not end her session")
	}
	// The sweeper (nil request) may terminate anything.
	sweeper := b.ResolveSessionIdManager(nil)
	_, _ = sweeper.Terminate(b1)
	if _, err := bob.Validate(b1); err == nil {
		t.Error("the sweeper's terminate was ignored")
	}
	if _, err := sweeper.Validate(b2); err == nil {
		t.Error("an unscoped manager validated a session")
	}
	if !bytes.HasPrefix([]byte(b2), []byte(sessionIDPrefix)) {
		t.Errorf("session ID %q lost its prefix", b2)
	}
}
