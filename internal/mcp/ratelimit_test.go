package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/retrieval"
)

// ratelimit_test.go: mk_report_outcome's traversal-log writes need a
// principal, happen once per retrieval session, and are rate limited
// per principal (meerkat-mob#45 item 1).

func asCaller(subject string) context.Context {
	g := authz.NewGrants(authz.Identity{Subject: subject, Issuer: "https://issuer.example"},
		map[string][]authz.Capability{"flux": {authz.CapRead}})
	return authz.NewContext(context.Background(), g)
}

// report calls one long-lived handler, as a server does.
func report(t *testing.T, ctx context.Context, h mcpserver.ToolHandlerFunc, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = toolReportOutcome
	req.Params.Arguments = args
	res, err := h(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("report: %v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReportGate_AnonymousCallersAreNotLogged(t *testing.T) {
	f := newOutcomeFixture(t)
	h := reportOutcomeHandler(f.reg, f.opts)
	// Admitted anonymously under a policy: grants over an identity with
	// no subject.
	anon := authz.NewContext(context.Background(), authz.NewGrants(authz.Identity{},
		map[string][]authz.Capability{"flux": {authz.CapRead}}))
	out := report(t, anon, h, map[string]any{"outcome": "found", "session_id": "s1"})
	if out["recorded"] != true || out["logged"] != false || out["log"] != logNotPermitted {
		t.Errorf("anonymous report: %v", out)
	}
	if n := len(logLines(t, f.logDir)); n != 0 {
		t.Errorf("an anonymous report wrote %d log objects", n)
	}
	// No policy at all (stdio, no auth: block) is the trusted local
	// shape: it is logged.
	out = report(t, context.Background(), h, map[string]any{"outcome": "found", "session_id": "s1"})
	if out["logged"] != true || out["log"] != logWritten {
		t.Errorf("local report: %v", out)
	}
}

func TestReportGate_OneReportPerRetrievalSession(t *testing.T) {
	f := newOutcomeFixture(t)
	f.opts.Outcome.Sessions = retrieval.New(0, retrieval.Limits{})
	h := reportOutcomeHandler(f.reg, f.opts)
	alice, bob := asCaller("alice"), asCaller("bob")

	if out := report(t, alice, h, map[string]any{"outcome": "found", "session_id": "q1"}); out["log"] != logWritten {
		t.Fatalf("first report: %v", out)
	}
	for range 3 {
		if out := report(t, alice, h, map[string]any{"outcome": "found", "session_id": "q1"}); out["log"] != logDuplicate || out["logged"] != false {
			t.Errorf("repeat report: %v", out)
		}
	}
	if n := len(logLines(t, f.logDir)); n != 1 {
		t.Errorf("%d log objects for one session, want 1", n)
	}
	// Another principal's "q1" is another session.
	if out := report(t, bob, h, map[string]any{"outcome": "found", "session_id": "q1"}); out["log"] != logWritten {
		t.Errorf("bob's q1 was treated as alice's: %v", out)
	}
	// A new retrieval session under the reused id is a new session: the
	// report that closes it is its first.
	_, s := f.opts.Outcome.Sessions.Begin(alice, sessionKey(alice, "q1"))
	_ = s.Step()
	if out := report(t, alice, h, map[string]any{"outcome": "found", "session_id": "q1"}); out["log"] != logWritten {
		t.Errorf("report closing a new session under a reused id: %v", out)
	}
}

func TestReportGate_RateLimitedPerPrincipal(t *testing.T) {
	f := newOutcomeFixture(t)
	f.opts.Outcome.Reports = ReportLimits{Burst: 2, Every: time.Hour}
	h := reportOutcomeHandler(f.reg, f.opts)
	alice, bob := asCaller("alice"), asCaller("bob")
	var verdicts []any
	for i := range 4 {
		out := report(t, alice, h, map[string]any{"outcome": "found", "session_id": fmt.Sprintf("a%d", i)})
		verdicts = append(verdicts, out["log"])
	}
	want := []any{logWritten, logWritten, logRateLimited, logRateLimited}
	if fmt.Sprint(verdicts) != fmt.Sprint(want) {
		t.Errorf("alice's verdicts = %v, want %v", verdicts, want)
	}
	// Session-less reports (a fresh id each time) count against the same
	// bucket: varying the id is not a way around the limit.
	if out := report(t, alice, h, map[string]any{"outcome": "found"}); out["log"] != logRateLimited {
		t.Errorf("session-less report escaped the limit: %v", out)
	}
	if out := report(t, bob, h, map[string]any{"outcome": "found", "session_id": "a0"}); out["log"] != logWritten {
		t.Errorf("alice's limit applied to bob: %v", out)
	}
	if n := len(logLines(t, f.logDir)); n != 3 {
		t.Errorf("%d log objects, want 3", n)
	}
}

func TestBuckets_RefillAndFailClosedWhenFull(t *testing.T) {
	b := newBuckets(2, time.Minute)
	clock := time.Unix(0, 0)
	b.now = func() time.Time { return clock }
	take := func(n int) []bool {
		var got []bool
		for range n {
			got = append(got, b.allow("a"))
		}
		return got
	}
	if got := fmt.Sprint(take(3)); got != "[true true false]" {
		t.Fatalf("burst of 2: %s", got)
	}
	clock = clock.Add(time.Minute)
	if got := fmt.Sprint(take(2)); got != "[true false]" {
		t.Errorf("one token per minute: %s", got)
	}

	b.max = 2
	if !b.allow("b") {
		t.Fatal("b refused")
	}
	// Full of non-full buckets: a new key is refused.
	if b.allow("c") {
		t.Error("a new key got a bucket in a full set")
	}
	// Once a bucket has refilled completely it can be dropped.
	clock = clock.Add(10 * time.Minute)
	if !b.allow("c") {
		t.Error("full buckets were not pruned to make room")
	}
	if len(b.m) > b.max {
		t.Errorf("bucket set grew to %d past %d", len(b.m), b.max)
	}
}

func TestReportGate_RememberedKeysExpireAndAreBounded(t *testing.T) {
	g := newReportGate(ReportLimits{Remember: time.Minute})
	clock := time.Unix(0, 0)
	g.now = func() time.Time { return clock }
	g.mark("k")
	if !g.seen("k") {
		t.Fatal("a marked key is not seen")
	}
	clock = clock.Add(2 * time.Minute)
	if g.seen("k") {
		t.Error("a key outlived its window")
	}
	for i := range maxTracked + 5 {
		g.mark(fmt.Sprint(i))
	}
	if len(g.reported) > maxTracked || g.order.Len() != len(g.reported) {
		t.Errorf("reported set = %d (list %d), cap %d", len(g.reported), g.order.Len(), maxTracked)
	}
}
