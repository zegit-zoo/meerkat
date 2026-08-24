package refresh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// controller_test.go covers the SCHEDULING half: what one cycle records,
// what an overlap does, and that the loops stop when told to.

// fakeTarget is a Target whose Reconcile is whatever a test says it is.
type fakeTarget struct {
	key  Key
	spec *Spec

	mu    sync.Mutex
	calls int
	out   Outcome
	err   error
	// block, when non-nil, is received from before a call returns — used
	// to hold a cycle open while something else is attempted.
	block chan struct{}
}

func newFakeTarget(ordinal int, kind string) *fakeTarget {
	return &fakeTarget{
		key:  Key{Ordinal: ordinal, Kind: kind, Name: "notes"},
		spec: &Spec{Interval: Duration(MinInterval)},
	}
}

func (f *fakeTarget) Key() Key    { return f.key }
func (f *fakeTarget) Spec() *Spec { return f.spec }

func (f *fakeTarget) Reconcile(context.Context) (Outcome, error) {
	f.mu.Lock()
	f.calls++
	block, out, err := f.block, f.out, f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return out, err
}

func (f *fakeTarget) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeTarget) setResult(out Outcome, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out, f.err = out, err
}

// scrape renders a registry exactly as /metrics would, so one assertion
// covers a series' NAME, its LABELS and its value together — the idiom
// internal/mcp's metrics tests already use, and the one that actually
// catches a rename or a stray label.
func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func wantSeries(t *testing.T, body string, series ...string) {
	t.Helper()
	for _, s := range series {
		if !strings.Contains(body, s) {
			t.Errorf("metrics missing %q\ngot:\n%s", s, body)
		}
	}
}

func TestNew_NoTargetsIsNoController(t *testing.T) {
	if c := New(Options{}); c != nil {
		t.Fatal("a deployment that configured no refresh should carry no controller")
	}
	// Every method tolerates the nil, so callers need no guard.
	var c *Controller
	c.Start(context.Background())
	if err := c.Close(); err != nil {
		t.Errorf("Close on a nil controller = %v", err)
	}
	if err := c.ReloadNow(context.Background()); err != nil {
		t.Errorf("ReloadNow on a nil controller = %v", err)
	}
	if c.Targets() != 0 {
		t.Error("a nil controller has no targets")
	}
}

func TestReloadNow_RunsEveryTargetThroughReconcile(t *testing.T) {
	content := newFakeTarget(0, KindContent)
	mem := newFakeTarget(0, KindMemory)
	reg := prometheus.NewRegistry()
	c := New(Options{Targets: []Target{content, mem}, Registry: reg})

	content.setResult(Outcome{Changed: true, Version: "1234"}, nil)
	if err := c.ReloadNow(context.Background()); err != nil {
		t.Fatalf("ReloadNow: %v", err)
	}
	if content.callCount() != 1 || mem.callCount() != 1 {
		t.Fatalf("calls = content:%d memory:%d, want one each", content.callCount(), mem.callCount())
	}
	body := scrape(t, reg)
	wantSeries(t, body,
		`meerkat_refresh_attempts_total{collection="0",kind="content"} 1`,
		`meerkat_refresh_changes_total{collection="0",kind="content"} 1`,
		// A no-change probe is a SUCCESS with no change: attempted, not
		// counted as a change, and it still moves the last-success clock.
		`meerkat_refresh_attempts_total{collection="0",kind="memory"} 1`,
		`meerkat_refresh_changes_total{collection="0",kind="memory"} 0`,
	)
	if !strings.Contains(body, `meerkat_refresh_last_success_timestamp_seconds{collection="0",kind="memory"}`) {
		t.Error("an unchanged probe should still record a successful cycle")
	}
	// The version token must never become a label — a generation
	// increments forever, and one series per publication is a leak that
	// grows without bound.
	if strings.Contains(body, "1234") {
		t.Errorf("a source version leaked into a metric label:\n%s", body)
	}
}

// TestReloadNow_FailureIsCountedAndDegradedIsSet pins the observable
// half of "serve last good": the cycle failed, the controller says so,
// and nothing about it is fatal.
func TestReloadNow_FailureIsCountedAndDegradedIsSet(t *testing.T) {
	target := newFakeTarget(2, KindContent)
	reg := prometheus.NewRegistry()
	c := New(Options{Targets: []Target{target}, Registry: reg})

	target.setResult(Outcome{}, errors.New("403 from the bucket"))
	err := c.ReloadNow(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("ReloadNow = %v, want the target's error", err)
	}
	wantSeries(t, scrape(t, reg),
		`meerkat_refresh_failures_total{collection="2",kind="content"} 1`,
		`meerkat_refresh_degraded{collection="2",kind="content"} 1`,
	)

	// A later success clears the degraded flag.
	target.setResult(Outcome{Changed: true, Version: "2"}, nil)
	if err := c.ReloadNow(context.Background()); err != nil {
		t.Fatalf("ReloadNow after recovery: %v", err)
	}
	wantSeries(t, scrape(t, reg), `meerkat_refresh_degraded{collection="2",kind="content"} 0`)
}

// TestReloadNow_BusyIsSkippedNotFailed: an overlapping cycle is the
// system working. It must not count as a failure, must not mark anything
// degraded, and must not be reported to the operator as an error.
func TestReloadNow_BusyIsSkippedNotFailed(t *testing.T) {
	target := newFakeTarget(0, KindContent)
	reg := prometheus.NewRegistry()
	c := New(Options{Targets: []Target{target}, Registry: reg})

	target.setResult(Outcome{}, ErrBusy)
	if err := c.ReloadNow(context.Background()); err != nil {
		t.Fatalf("ReloadNow with a busy target = %v, want no error", err)
	}
	wantSeries(t, scrape(t, reg),
		`meerkat_refresh_skipped_total{collection="0",kind="content"} 1`,
		`meerkat_refresh_failures_total{collection="0",kind="content"} 0`,
		`meerkat_refresh_degraded{collection="0",kind="content"} 0`,
	)
}

// TestReloadNow_CancellationIsNotAFailure: a graceful shutdown must not
// look like an incident on a dashboard.
func TestReloadNow_CancellationIsNotAFailure(t *testing.T) {
	target := newFakeTarget(0, KindMemory)
	reg := prometheus.NewRegistry()
	c := New(Options{Targets: []Target{target}, Registry: reg})

	target.setResult(Outcome{}, context.Canceled)
	if err := c.ReloadNow(context.Background()); err != nil {
		t.Fatalf("ReloadNow = %v, want a cancelled cycle to be silent", err)
	}
	wantSeries(t, scrape(t, reg), `meerkat_refresh_failures_total{collection="0",kind="memory"} 0`)
}

// TestReloadNow_JoinsEveryTargetsError proves one broken collection does
// not hide another's outcome.
func TestReloadNow_JoinsEveryTargetsError(t *testing.T) {
	a, b := newFakeTarget(0, KindContent), newFakeTarget(1, KindContent)
	a.setResult(Outcome{}, errors.New("alpha is broken"))
	b.setResult(Outcome{}, errors.New("beta is broken"))
	c := New(Options{Targets: []Target{a, b}})

	err := c.ReloadNow(context.Background())
	if err == nil {
		t.Fatal("expected both errors")
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Errorf("error = %v, want both targets named", err)
	}
	if a.callCount() != 1 || b.callCount() != 1 {
		t.Error("a failing target must not stop the others from reconciling")
	}
}

// TestStartAndClose_StopsEveryLoop drives the real schedule and proves
// Close stops it.
func TestStartAndClose_StopsEveryLoop(t *testing.T) {
	target := newFakeTarget(0, KindContent)
	// Far below MinInterval, which validation would refuse; constructed
	// directly here so the loop actually ticks inside a test's lifetime.
	target.spec = &Spec{Interval: Duration(time.Millisecond)}
	c := New(Options{Targets: []Target{target}})

	c.Start(context.Background())
	c.Start(context.Background()) // idempotent
	waitFor(t, func() bool { return target.callCount() > 0 }, "the loop never ran a cycle")

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	after := target.callCount()
	time.Sleep(20 * time.Millisecond)
	if target.callCount() != after {
		t.Error("a cycle ran after Close returned")
	}
}

// TestClose_WaitsForAnInFlightCycle is the shutdown-ordering property
// spelled out: a cycle that is mid-swap holds a collection's reload slot
// and is about to install a snapshot, so Close must not return — and let
// the caller tear the registry down — until it has finished.
func TestClose_WaitsForAnInFlightCycle(t *testing.T) {
	target := newFakeTarget(0, KindContent)
	target.spec = &Spec{Interval: Duration(time.Millisecond)}
	release := make(chan struct{})
	target.mu.Lock()
	target.block = release
	target.mu.Unlock()

	c := New(Options{Targets: []Target{target}})
	c.Start(context.Background())
	waitFor(t, func() bool { return target.callCount() > 0 }, "the loop never entered a cycle")

	closed := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a cycle was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the cycle finished")
	}
}

// TestStart_CancellingTheContextStopsTheLoops covers the other shutdown
// path: the server's context going away rather than an explicit Close.
func TestStart_CancellingTheContextStopsTheLoops(t *testing.T) {
	target := newFakeTarget(0, KindMemory)
	target.spec = &Spec{Interval: Duration(time.Millisecond)}
	c := New(Options{Targets: []Target{target}})

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	waitFor(t, func() bool { return target.callCount() > 0 }, "the loop never ran a cycle")
	cancel()
	if err := c.Close(); err != nil {
		t.Fatalf("Close after cancellation: %v", err)
	}
}

// TestKey_LabelIsTheOrdinal is the label-discipline pin: a metric series
// is keyed by configuration position, never by the collection's name.
func TestKey_LabelIsTheOrdinal(t *testing.T) {
	k := Key{Ordinal: 3, Kind: KindContent, Name: "customer-secrets"}
	if got := k.Label(); got != "3" {
		t.Errorf("Label() = %q, want the ordinal", got)
	}
	if strings.Contains(k.Label(), k.Name) {
		t.Error("the metric label must not carry the collection name")
	}
}

// TestMetrics_AreRegisteredUnderTheirDocumentedNames pins the wire names
// and the label set, because a dashboard and an alert rule reference
// them by string: a rename is invisible until an on-call rotation
// notices a blank panel.
//
// Publishing a ZERO for a target that has never failed is part of the
// contract — "no data" and "no failures" look the same to a naive alert,
// and only one of them is true.
func TestMetrics_AreRegisteredUnderTheirDocumentedNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	target := newFakeTarget(0, KindContent)
	target.key.Name = "customer-secrets"
	_ = New(Options{Targets: []Target{target}, Registry: reg})

	body := scrape(t, reg)
	wantSeries(t, body,
		`meerkat_refresh_attempts_total{collection="0",kind="content"} 0`,
		`meerkat_refresh_changes_total{collection="0",kind="content"} 0`,
		`meerkat_refresh_failures_total{collection="0",kind="content"} 0`,
		`meerkat_refresh_skipped_total{collection="0",kind="content"} 0`,
		`meerkat_refresh_degraded{collection="0",kind="content"} 0`,
	)
	if strings.Contains(body, "customer-secrets") {
		t.Errorf("a collection name reached an unauthenticated metric label:\n%s", body)
	}
	if got := strings.Join(refreshLabels, ","); got != "collection,kind" {
		t.Errorf("refresh labels = %q, want exactly collection,kind", got)
	}
}

// waitFor polls cond until it holds or the test gives up.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}
