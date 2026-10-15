package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zegit-zoo/meerkat/internal/traversal"
)

// traversalmetrics_test.go: the traversal log's daily cap answers
// `log: daily_cap`, and its retention and cap counters reach /metrics
// (meerkat-mob#45).

func openCappedLog(t *testing.T, dir string, perDay int) *traversal.Log {
	t.Helper()
	t.Setenv("MEERKAT_TEST_PATH_KEY", "not a secret, a test key long enough")
	l, err := traversal.Open(context.Background(), &traversal.Config{
		Backend: "local", Path: dir, HMACKeyEnv: "MEERKAT_TEST_PATH_KEY", MaxEntriesPerDay: &perDay,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestReportGate_DailyCap(t *testing.T) {
	f := newOutcomeFixture(t)
	f.opts.Outcome.Log = openCappedLog(t, f.logDir, 1)
	h := reportOutcomeHandler(f.reg, f.opts)

	if out := report(t, asCaller("alice"), h, map[string]any{"outcome": "found", "session_id": "a1"}); out["log"] != logWritten {
		t.Fatalf("first report: %v", out)
	}
	out := report(t, asCaller("bob"), h, map[string]any{"outcome": "found", "session_id": "b1"})
	if out["log"] != logDailyCap || out["logged"] != false || out["recorded"] != true {
		t.Errorf("report past the cap: %v", out)
	}
	if n := len(logLines(t, f.logDir)); n != 1 {
		t.Errorf("%d log objects, want 1", n)
	}
}

func TestRegisterTraversalLog_ExportsRetentionAndCap(t *testing.T) {
	dir := t.TempDir()
	// An expired day for Open's first retention pass to delete.
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(traversal.Prefix), "2020-01-01"), 0o750); err != nil {
		t.Fatal(err)
	}
	l := openCappedLog(t, dir, 1)
	ctx := context.Background()
	for range 2 {
		_, _ = l.Record(ctx, traversal.Entry{Session: "s", Outcome: "found"})
	}

	reg := prometheus.NewRegistry()
	registerTraversalLog(reg, l)
	registerTraversalLog(prometheus.NewRegistry(), nil) // no log, nothing to register

	for name, want := range map[string]float64{
		"meerkat_traversal_log_retention_days_total":          1, // deleted 1 + failed 0
		"meerkat_traversal_log_retention_list_failures_total": 0,
		"meerkat_traversal_log_capped_total":                  1,
	} {
		if got := counterTotal(t, reg, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// Open's pass, plus the write path's backstop pass.
	if got := counterTotal(t, reg, "meerkat_traversal_log_retention_runs_total"); got < 1 {
		t.Errorf("retention runs = %v, want at least 1", got)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		switch f.GetName() {
		case "meerkat_traversal_log_retention_last_run_timestamp_seconds":
			if v := f.GetMetric()[0].GetGauge().GetValue(); v <= 0 {
				t.Errorf("last run = %v, want a timestamp", v)
			}
		case "meerkat_traversal_log_retention_days_total":
			for _, m := range f.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() != "result" || (lp.GetValue() != "deleted" && lp.GetValue() != "failed") {
						t.Errorf("unexpected label %s=%q", lp.GetName(), lp.GetValue())
					}
				}
			}
		}
	}
}
