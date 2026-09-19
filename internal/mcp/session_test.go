package mcp

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// A recorded session of three collection hops produces one
// meerkat.retrieval.session span with the hop count, and the SLI
// histograms observe once each (meerkat-mob #6 acceptance).
func TestRetrievalSession_SpanAndSLIs(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	ctx := context.Background()
	c := f.client(ctx, "", nil)
	sid := "acceptance-session-1"

	// root-ish: runbooks -> architecture -> secrets -> back to runbooks.
	for _, coll := range []string{"runbooks", "architecture", "secrets", "runbooks"} {
		callText(t, ctx, c, toolSearch, map[string]any{"query": "paging", "collection": coll, "session_id": sid}) //nolint:errcheck // exercising
	}
	callText(t, ctx, c, toolShow, map[string]any{"id": "runbooks:incidents/paging", "session_id": sid}) //nolint:errcheck // exercising
	callText(t, ctx, c, toolReportOutcome, map[string]any{                                              //nolint:errcheck // exercising
		"session_id": sid, "outcome": "found", "pages": []any{"runbooks:incidents/paging"},
		"quality": map[string]any{"accuracy": 0.9, "completeness": 0.8, "answer_quality": 0.7},
	})

	var sessions int
	for _, s := range f.flush() {
		if s.Name != "meerkat.retrieval.session" {
			continue
		}
		sessions++
		attrs := map[attribute.Key]attribute.Value{}
		for _, a := range s.Attributes {
			attrs[a.Key] = a.Value
		}
		if attrs["meerkat.retrieval.hops"].AsInt64() != 3 {
			t.Errorf("hops = %v, want 3", attrs["meerkat.retrieval.hops"].AsInt64())
		}
		if attrs["meerkat.retrieval.outcome"].AsString() != "found" {
			t.Errorf("outcome = %v", attrs["meerkat.retrieval.outcome"].AsString())
		}
		if attrs["meerkat.retrieval.wrong_turns"].AsInt64() != 2 {
			t.Errorf("wrong_turns = %v, want 2 (architecture and secrets never shown)", attrs["meerkat.retrieval.wrong_turns"].AsInt64())
		}
		if !attrs["meerkat.retrieval.hot_path"].AsBool() {
			t.Error("a flat deployment's collections are always resident: hot")
		}
		for _, a := range s.Attributes {
			v := strings.ToLower(a.Value.AsString())
			if strings.Contains(v, "runbooks") || strings.Contains(v, "paging") || strings.Contains(v, sid) {
				t.Errorf("session span carries an identity: %s=%q", a.Key, a.Value.AsString())
			}
		}
	}
	if sessions != 1 {
		t.Fatalf("session spans = %d, want exactly one", sessions)
	}

	families, err := f.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, fam := range families {
		switch fam.GetName() {
		case "meerkat_retrieval_sessions_total", "meerkat_retrieval_time_to_first_context_seconds",
			"meerkat_retrieval_time_to_first_relevant_context_seconds", "meerkat_retrieval_collection_hops",
			"meerkat_retrieval_steps", "meerkat_retrieval_wrong_turns", "meerkat_retrieval_accuracy":
			for _, m := range fam.GetMetric() {
				n := uint64(0)
				if m.GetHistogram() != nil {
					n = m.GetHistogram().GetSampleCount()
				} else if m.GetCounter() != nil {
					n = uint64(m.GetCounter().GetValue())
				}
				if n == 1 {
					seen[fam.GetName()] = true
				}
			}
		}
	}
	for _, want := range []string{"meerkat_retrieval_sessions_total", "meerkat_retrieval_time_to_first_context_seconds", "meerkat_retrieval_time_to_first_relevant_context_seconds", "meerkat_retrieval_collection_hops", "meerkat_retrieval_steps", "meerkat_retrieval_wrong_turns", "meerkat_retrieval_accuracy"} {
		if !seen[want] {
			t.Errorf("%s did not observe exactly once", want)
		}
	}
}

func TestRetrievalSession_LimitReachedAnswer(t *testing.T) {
	f := newTracedFixture(t, tracedOptions{})
	ctx := context.Background()
	c := f.client(ctx, "", nil)
	sid := "limits-1"
	// The fixture allows 12 hops; alternate collections until it trips.
	var last string
	for i := 0; i < 20; i++ {
		coll := []string{"runbooks", "architecture"}[i%2]
		last, _ = callText(t, ctx, c, toolSearch, map[string]any{"query": "paging", "collection": coll, "session_id": sid})
		if strings.Contains(last, `"limit_reached"`) {
			break
		}
	}
	if !strings.Contains(last, `"limit_reached"`) || !strings.Contains(last, `"limit": "hops"`) || !strings.Contains(last, "mk_report_outcome") {
		t.Fatalf("expected a limit_reached answer naming hops and pointing at mk_report_outcome, got %s", last)
	}
	families, _ := f.reg.Gather()
	for _, fam := range families {
		if fam.GetName() == "meerkat_retrieval_limit_reached_total" {
			for _, m := range fam.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "limit" && l.GetValue() == "hops" && m.GetCounter().GetValue() >= 1 {
						return
					}
				}
			}
		}
	}
	t.Error("meerkat_retrieval_limit_reached_total{limit=hops} was not counted")
}
