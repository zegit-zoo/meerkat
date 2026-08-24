package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/refresh"
)

// refresh_test.go covers what the hosted server adds on top of
// internal/refresh and internal/collections: that a `refresh:` block in
// configuration produces a controller at all, that a failed cycle
// reaches /readyz and /metrics in the shape those two endpoints promise,
// and that the admin reload trigger goes through the same path.

// unreachableGCS returns a source that is a VALID refreshable gcs source
// and cannot possibly resolve: credentials are pointed at a file that
// does not exist, so the Google client fails to construct before any
// network call is attempted.
//
// That is deliberate. This test must be hermetic on a developer's
// machine as well as in CI, and a bucket name in a test that ran with
// ambient application-default credentials would otherwise reach out to
// somebody's real project.
func unreachableGCS(t *testing.T) contentsource.Source {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "no-such-credentials.json"))
	src := contentsource.Source{
		Type:   contentsource.TypeGCS,
		Bucket: "example-kb",
		Prefix: "handbook/live/",
		Layout: contentsource.MergeLayout(contentsource.Layout{}),
		Refresh: &refresh.Spec{
			Interval: refresh.Duration(time.Minute),
			Jitter:   refresh.Duration(10 * time.Second),
		},
	}
	if err := src.Validate(); err != nil {
		t.Fatalf("the test's own source is invalid: %v", err)
	}
	return src
}

// refreshingServer builds a hosted server over one collection whose
// content source is refreshable.
func refreshingServer(t *testing.T, src contentsource.Source) (*HostedServer, *httptest.Server) {
	t.Helper()
	c := collections.FromPages("handbook", []kb.Page{
		testPage("onboarding", "Onboarding", "how we onboard", "guides", "reviewed", "team-a"),
	})
	c.Source = src
	reg, err := collections.New(c)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewHosted(context.Background(), HostedConfig{
		Collections:  reg,
		Version:      "test",
		Logger:       slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		ReadinessTTL: time.Nanosecond, // no caching, so a transition is observable
	})
	if err != nil {
		t.Fatalf("NewHosted: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

// TestHosted_RefreshIsOptInAndOff proves the default posture: a
// deployment that wrote no refresh block gets no controller, no refresh
// metrics, and a reload trigger that does nothing rather than erroring.
func TestHosted_RefreshIsOptInAndOff(t *testing.T) {
	f := newHostedFixture(t, nil)
	if err := f.srv.Reload(context.Background()); err != nil {
		t.Errorf("Reload with nothing configured = %v, want a silent no-op", err)
	}
	resp := f.do(t, http.MethodGet, MetricsPath, "")
	defer resp.Body.Close()
	body := readAll(t, resp)
	if strings.Contains(body, "meerkat_refresh_") {
		t.Error("a deployment that configured no refresh is publishing refresh metrics")
	}
	// The readiness counts are still reported, and nothing is degraded.
	code, ready := get(t, f.http, ReadinessPath)
	if code != http.StatusOK {
		t.Fatalf("readyz = %d (%s)", code, ready)
	}
	if !strings.Contains(ready, `"degraded":0`) {
		t.Errorf("readyz body = %s, want degraded=0", ready)
	}
}

// TestHosted_FailedRefreshIsDegradedButStillServing is the end-to-end
// serve-last-good story on the hosted surface: the cycle fails, /readyz
// says degraded while still answering 200, the metrics say which target
// and how many, and the collection keeps serving.
func TestHosted_FailedRefreshIsDegradedButStillServing(t *testing.T) {
	srv, ts := refreshingServer(t, unreachableGCS(t))

	// Green before anything has been attempted: a collection that has not
	// yet reconciled is not degraded.
	code, body := get(t, ts, ReadinessPath)
	if code != http.StatusOK || !strings.Contains(body, `"degraded":0`) {
		t.Fatalf("readyz before any cycle = %d %s", code, body)
	}

	// The admin trigger runs the same cycle the scheduled loop would.
	err := srv.Reload(context.Background())
	if err == nil {
		t.Fatal("Reload should have reported the unreachable source")
	}
	if !strings.Contains(err.Error(), "handbook") {
		t.Errorf("error = %v, want it to name the collection", err)
	}

	// /readyz: still 200 (the collection is answering), status degraded,
	// counts only — no name, no bucket, no error text.
	code, body = get(t, ts, ReadinessPath)
	if code != http.StatusOK {
		t.Fatalf("readyz = %d (%s), want 200 — a stale collection is still serving", code, body)
	}
	for _, want := range []string{`"status":"degraded"`, `"ready":1`, `"degraded":1`, `"total":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("readyz body = %s, want %s", body, want)
		}
	}
	for _, forbidden := range []string{"handbook", "example-kb", "credentials", "error"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("readyz leaked %q — the body is counts and state only: %s", forbidden, body)
		}
	}

	// /metrics: the refresh series, labelled by ordinal and kind.
	code, metrics := get(t, ts, MetricsPath)
	if code != http.StatusOK {
		t.Fatalf("metrics = %d", code)
	}
	for _, want := range []string{
		`meerkat_refresh_attempts_total{collection="0",kind="content"} 1`,
		`meerkat_refresh_failures_total{collection="0",kind="content"} 1`,
		`meerkat_refresh_degraded{collection="0",kind="content"} 1`,
		`meerkat_collections_degraded 1`,
		`meerkat_collections_ready 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	for _, forbidden := range []string{"handbook", "example-kb", "handbook/live"} {
		if strings.Contains(metrics, forbidden) {
			t.Errorf("metrics leaked %q as a label", forbidden)
		}
	}

	// The collection is still answering, which is the whole point.
	if _, err := srv.reg.Search(context.Background(), "", "onboard", 5); err != nil {
		t.Errorf("a degraded collection stopped answering queries: %v", err)
	}
}

// TestHosted_UnreadyPolicyDrainsTheReplica covers the other failure
// policy end to end: /readyz answers 503 so an orchestrator can act,
// while the collection still serves the requests it already has.
func TestHosted_UnreadyPolicyDrainsTheReplica(t *testing.T) {
	src := unreachableGCS(t)
	src.Refresh.FailurePolicy = refresh.PolicyUnready
	srv, ts := refreshingServer(t, src)

	if err := srv.Reload(context.Background()); err == nil {
		t.Fatal("Reload should have reported the unreachable source")
	}
	code, body := get(t, ts, ReadinessPath)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d (%s), want 503 under failure_policy: unready", code, body)
	}
	if !strings.Contains(body, `"ready":0`) || !strings.Contains(body, `"degraded":1`) {
		t.Errorf("readyz body = %s, want ready=0 degraded=1", body)
	}
	if _, err := srv.reg.Search(context.Background(), "", "onboard", 5); err != nil {
		t.Errorf("an unready collection stopped answering queries: %v", err)
	}
}

// TestHosted_FreshnessDetailGoesToAuthenticatedDiscovery pins where the
// detail /readyz and the metrics deliberately omit actually lives: the
// auth-gated collection-discovery surface, which is already scoped to
// the collections this caller may read.
func TestHosted_FreshnessDetailGoesToAuthenticatedDiscovery(t *testing.T) {
	srv, _ := refreshingServer(t, unreachableGCS(t))
	if err := srv.Reload(context.Background()); err == nil {
		t.Fatal("Reload should have reported the unreachable source")
	}

	body, err := listCollectionsJSON(context.Background(), srv.reg)
	if err != nil {
		t.Fatalf("listCollectionsJSON: %v", err)
	}
	var got []collectionSummary
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d collections", len(got))
	}
	if len(got[0].Refresh) != 1 {
		t.Fatalf("refresh = %+v, want the one configured content target", got[0].Refresh)
	}
	status := got[0].Refresh[0]
	if status.Kind != refresh.KindContent {
		t.Errorf("kind = %q", status.Kind)
	}
	if status.Interval != "1m0s" || status.Policy != refresh.PolicyServeLastGood {
		t.Errorf("interval/policy = %q/%q", status.Interval, status.Policy)
	}
	if !status.Degraded || status.Error == "" {
		t.Errorf("status = %+v, want a degraded entry explaining why", status)
	}
	if status.LastAttempt.IsZero() {
		t.Error("a failed cycle should still record an attempt")
	}
	if !status.LastSuccess.IsZero() {
		t.Error("LastSuccess must stay zero until a cycle actually succeeds — it is how stale the content is")
	}
}

// TestHosted_RefreshControllerIsBuiltFromConfiguration proves the wiring
// itself: the targets come from the collections' own blocks, and the
// loops are not running merely because a server was constructed.
func TestHosted_RefreshControllerIsBuiltFromConfiguration(t *testing.T) {
	srv, ts := refreshingServer(t, unreachableGCS(t))
	if srv.refresh == nil {
		t.Fatal("a collection with a refresh block produced no controller")
	}
	if got := srv.refresh.Targets(); got != 1 {
		t.Errorf("targets = %d, want the one content target", got)
	}
	// Constructed, not started: the zeroed series are published, but no
	// cycle has run.
	_, metrics := get(t, ts, MetricsPath)
	if !strings.Contains(metrics, `meerkat_refresh_attempts_total{collection="0",kind="content"} 0`) {
		t.Error("building a server should publish zeroed refresh series without running a cycle")
	}
}
