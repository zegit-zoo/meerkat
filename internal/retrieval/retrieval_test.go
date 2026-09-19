package retrieval

import (
	"context"
	"testing"
	"time"
)

func newClock(start time.Time) (*time.Time, func() time.Time) {
	now := start
	return &now, func() time.Time { return now }
}

func TestSession_ThreeHopsThenFound(t *testing.T) {
	ctx := context.Background()
	tr := New(0, Limits{})
	clock, now := newClock(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	tr.now = now

	_, s := tr.Begin(ctx, "sess-1")
	if s == nil || tr.Live() != 1 {
		t.Fatal("Begin must create a live session")
	}
	// root (hot) -> platform (hot) -> flux (cold): 3 searches, then a show.
	steps := []struct {
		coll     string
		tier     int
		hits     int
		resident bool
	}{{"root", 0, 1, true}, {"platform", 1, 1, true}, {"flux", 2, 2, false}}
	for _, st := range steps {
		*clock = clock.Add(time.Second)
		if err := s.Step(); err != nil {
			t.Fatal(err)
		}
		if err := s.Search(st.coll, st.tier, st.hits, st.resident); err != nil {
			t.Fatal(err)
		}
	}
	*clock = clock.Add(time.Second)
	_ = s.Step()
	s.Show("flux", 2, true)
	*clock = clock.Add(time.Second)

	sum := tr.End(ctx, "sess-1", OutcomeFound, &Quality{Accuracy: 0.9, Completeness: 0.8, AnswerQuality: 0.7})
	if sum == nil || tr.Live() != 0 {
		t.Fatal("End must return a summary and forget the session")
	}
	// Hops: root->platform, platform->flux. Wrong turns: platform (hopped
	// into, never shown); the start collection is not a turn. Not hot:
	// flux was cold when first touched.
	if sum.Hops != 2 || sum.Steps != 4 || sum.WrongTurns != 1 || sum.Hot || sum.TierReached != 2 || sum.Outcome != OutcomeFound {
		t.Errorf("summary = %+v", sum)
	}
	if sum.FirstContext != time.Second || sum.FirstRelevant != 4*time.Second || sum.Duration != 5*time.Second {
		t.Errorf("timings = first %s relevant %s total %s", sum.FirstContext, sum.FirstRelevant, sum.Duration)
	}
	if tr.End(ctx, "sess-1", OutcomeFound, nil) != nil {
		t.Error("ending twice must be a no-op")
	}
}

func TestSession_WrongTurnsExcludeTheStart(t *testing.T) {
	ctx := context.Background()
	tr := New(0, Limits{})
	_, s := tr.Begin(ctx, "k")
	_ = s.Search("root", 0, 1, true)
	_ = s.Search("platform", 1, 1, true)
	_ = s.Search("vendors", 1, 0, true)
	_ = s.Search("flux", 2, 1, true)
	s.Show("flux", 2, true)
	sum := tr.End(ctx, "k", OutcomeFound, nil)
	// Hops: root->platform, platform->vendors, vendors->flux = 3; wrong
	// turns: platform and vendors (hopped into, never shown); the start
	// collection is not a turn.
	if sum.Hops != 3 || sum.WrongTurns != 2 || !sum.Hot {
		t.Errorf("summary = %+v", sum)
	}
}

func TestSession_GaveUpAndProxies(t *testing.T) {
	ctx := context.Background()
	tr := New(0, Limits{})
	clock, now := newClock(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	tr.now = now
	_, s := tr.Begin(ctx, "g")
	_ = s.Search("root", 0, 0, true)
	*clock = clock.Add(3 * time.Second)
	_ = s.Search("platform", 1, 0, true)
	sum := tr.End(ctx, "g", OutcomeGaveUp, nil)
	if sum.FirstContext != 0 || sum.FirstRelevant != 0 || sum.Outcome != OutcomeGaveUp || sum.Duration != 3*time.Second {
		t.Errorf("gave up with no hits: %+v", sum)
	}

	// A show followed by another search is not the relevant context
	// unless the report says found.
	_, s = tr.Begin(ctx, "p")
	_ = s.Search("root", 0, 1, true)
	*clock = clock.Add(time.Second)
	s.Show("root", 0, true)
	*clock = clock.Add(time.Second)
	_ = s.Search("platform", 1, 1, true)
	sum = tr.End(ctx, "p", OutcomeNotFound, nil)
	if sum.FirstRelevant != 0 {
		t.Errorf("a show followed by a search is not relevant on not_found: %+v", sum)
	}
	_, s = tr.Begin(ctx, "q")
	_ = s.Search("root", 0, 1, true)
	*clock = clock.Add(time.Second)
	s.Show("root", 0, true)
	*clock = clock.Add(time.Second)
	_ = s.Search("platform", 1, 1, true)
	sum = tr.End(ctx, "q", OutcomeFound, nil)
	if sum.FirstRelevant != time.Second {
		t.Errorf("a found report confirms the last show: %+v", sum)
	}
}

func TestSession_Limits(t *testing.T) {
	ctx := context.Background()
	tr := New(0, Limits{MaxHops: 2, MaxSteps: 10, MaxAttempts: 3})
	_, s := tr.Begin(ctx, "l")
	for _, c := range []string{"a", "b", "c"} {
		_ = s.Step()
		if err := s.Search(c, 1, 1, true); err != nil {
			if lim, ok := IsLimit(err); !ok || lim.Limit != "hops" || lim.Max != 2 || c != "c" {
				t.Fatalf("hop limit: %v at %s", err, c)
			}
		}
	}
	_, s2 := tr.Begin(ctx, "a")
	var err error
	for i := 0; i < 4; i++ {
		err = s2.Search("x", 0, 0, true)
	}
	if lim, ok := IsLimit(err); !ok || lim.Limit != "attempts" {
		t.Errorf("attempt limit: %v", err)
	}
	s2.Show("x", 0, true) // a show resets attempts
	if err := s2.Search("x", 0, 0, true); err != nil {
		t.Errorf("attempts must reset after a show: %v", err)
	}
	_, s3 := tr.Begin(ctx, "s")
	for i := 0; i < 10; i++ {
		if err := s3.Step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if lim, ok := IsLimit(s3.Step()); !ok || lim.Limit != "steps" {
		t.Error("step limit")
	}
	if _, ok := IsLimit(nil); ok {
		t.Error("nil is not a limit")
	}
}

func TestTracker_SweepAndNilSafety(t *testing.T) {
	ctx := context.Background()
	tr := New(10*time.Second, Limits{})
	clock, now := newClock(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	tr.now = now
	_, quiet := tr.Begin(ctx, "quiet")
	_ = quiet.Search("root", 0, 1, true)
	_, shown := tr.Begin(ctx, "shown")
	shown.Show("root", 0, true)
	_, fresh := tr.Begin(ctx, "fresh")
	*clock = clock.Add(11 * time.Second)
	_ = fresh.Step() // touched now: not idle
	if n := tr.Sweep(ctx); n != 2 || tr.Live() != 1 {
		t.Errorf("sweep ended %d, live %d; want 2 and 1", n, tr.Live())
	}

	var none *Tracker
	if c, s := none.Begin(ctx, "x"); s != nil || c != ctx {
		t.Error("nil tracker must be inert")
	}
	if _, s := tr.Begin(ctx, ""); s != nil {
		t.Error("empty key means no session")
	}
	var ns *Session
	if err := ns.Step(); err != nil {
		t.Error("nil session step")
	}
	if err := ns.Search("a", 0, 1, true); err != nil {
		t.Error("nil session search")
	}
	ns.Show("a", 0, true)
	if none.End(ctx, "x", OutcomeFound, nil) != nil || tr.End(ctx, "missing", OutcomeFound, nil) != nil {
		t.Error("ending an unknown session is a no-op")
	}
}

func TestTracker_RunEndsRemainingOnShutdown(t *testing.T) {
	tr := New(time.Hour, Limits{})
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = tr.Begin(ctx, "open")
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	cancel()
	<-done
	if tr.Live() != 0 {
		t.Error("shutdown must end every open session")
	}
}
