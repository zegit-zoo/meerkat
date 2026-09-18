// Package retrieval tracks RETRIEVAL SESSIONS (meerkat-mob issue F): the
// sequence of mk_search -> pointer -> mk_search -> mk_show an agent
// performs for one question — how long until it had context, how many
// hops and steps it took, whether it landed, and where it gave up.
//
// Per-call telemetry cannot answer the three questions the mob needs:
// how long a hot, well-used path takes; how long an obscure cold path
// takes; how long it takes to conclude a path does not exist. A session
// spans calls, so it can.
//
// Definitions (the issue's, made concrete):
//
//   - session: every tool call sharing a key — an explicit session_id,
//     or the MCP client session — within an idle window. It ends on
//     mk_report_outcome or on idle timeout.
//   - first_context: the first search with >= 1 hit, or the first show.
//   - first_relevant_context: the last show that was not followed by
//     another search (the proxy from brief Q1); a found report confirms
//     it, a gave_up report voids it.
//   - hop: a search scoped to a collection different from the previous
//     call's; step: any tool call; attempt: a search without a show
//     since the last show.
//   - wrong_turn: a hop into a collection that is never shown from.
//   - hot: every collection the session touched was resident before it
//     was touched (issue E's residency).
//
// Everything exported to spans and metrics is a count, a duration, a
// boolean or a closed-set value. Path IDENTITY goes to the traversal
// log (issue G), never here.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/zegit-zoo/meerkat/internal/telemetry"
)

// Outcomes.
const (
	OutcomeFound    = "found"
	OutcomeNotFound = "not_found"
	OutcomeGaveUp   = "gave_up"
	OutcomeTimeout  = "timeout"
)

// DefaultIdleTimeout ends a session that has been quiet this long.
const DefaultIdleTimeout = 120 * time.Second

// Limits are the per-session traversal restraints (issue D's manifest
// `limits:`): 0 means unlimited.
type Limits struct {
	MaxHops     int
	MaxSteps    int
	MaxAttempts int
}

// Quality is the consumer-reported quality from mk_report_outcome.
type Quality struct {
	Accuracy, Completeness, AnswerQuality float64
}

// ErrLimitReached is returned by Step and Search when a limit is
// exceeded. The caller answers with a structured limit_reached telling
// the agent to report the outcome rather than keep searching.
type ErrLimitReached struct {
	Limit string // hops | steps | attempts
	Max   int
}

func (e *ErrLimitReached) Error() string {
	return fmt.Sprintf("retrieval limit reached: %s > %d — report the outcome with mk_report_outcome instead of searching further", e.Limit, e.Max)
}

// Tracker holds live sessions.
type Tracker struct {
	idle   time.Duration
	limits Limits
	now    func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
}

// New builds a tracker. idle <= 0 means DefaultIdleTimeout.
func New(idle time.Duration, limits Limits) *Tracker {
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	return &Tracker{idle: idle, limits: limits, now: time.Now, sessions: map[string]*Session{}}
}

// Session is one retrieval session.
type Session struct {
	tracker *Tracker
	key     string
	started time.Time
	last    time.Time
	span    trace.Span
	spanCtx context.Context

	mu             sync.Mutex
	steps          int
	attempts       int
	hops           int
	shows          int
	firstContext   time.Time
	lastShow       time.Time
	searchedAfter  bool
	lastCollection string
	touched        []string
	shown          map[string]bool
	hot            bool
	tierReached    int
	ended          bool
}

// Begin returns the session for key, creating it on first sight, and a
// context under which the caller's own span becomes a child of the
// session span. An empty key means no session: the context is returned
// unchanged and the Session is nil (every method on a nil Session is a
// no-op returning nil).
func (t *Tracker) Begin(ctx context.Context, key string) (context.Context, *Session) {
	if t == nil || key == "" {
		return ctx, nil
	}
	t.mu.Lock()
	s, ok := t.sessions[key]
	if !ok {
		now := t.now()
		// The session span is a ROOT: detached from whatever request
		// span is current, so that every later call's span can hang
		// under it.
		root := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
		spanCtx, span := telemetry.Span(root, telemetry.SpanRetrievalSession)
		s = &Session{tracker: t, key: key, started: now, last: now, span: span, spanCtx: spanCtx, shown: map[string]bool{}, hot: true, tierReached: -1}
		t.sessions[key] = s
	}
	t.mu.Unlock()
	return trace.ContextWithSpan(ctx, s.span), s
}

// Step records one tool call and enforces the step limit.
func (s *Session) Step() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps++
	s.last = s.tracker.now()
	if s.tracker.limits.MaxSteps > 0 && s.steps > s.tracker.limits.MaxSteps {
		return &ErrLimitReached{Limit: "steps", Max: s.tracker.limits.MaxSteps}
	}
	return nil
}

// Search records a search scoped to collection ("" for an unscoped
// one) that returned hits, at tier (-1 unknown), against a collection
// that was resident before the call or not. It enforces the hop and
// attempt limits.
func (s *Session) Search(collection string, tier int, hits int, resident bool) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.tracker.now()
	s.attempts++
	if s.lastCollection != "" && collection != "" && collection != s.lastCollection {
		s.hops++
	}
	s.touch(collection, tier, resident)
	if hits > 0 && s.firstContext.IsZero() {
		s.firstContext = now
	}
	if !s.lastShow.IsZero() {
		s.searchedAfter = true
	}
	if s.tracker.limits.MaxHops > 0 && s.hops > s.tracker.limits.MaxHops {
		return &ErrLimitReached{Limit: "hops", Max: s.tracker.limits.MaxHops}
	}
	if s.tracker.limits.MaxAttempts > 0 && s.attempts > s.tracker.limits.MaxAttempts {
		return &ErrLimitReached{Limit: "attempts", Max: s.tracker.limits.MaxAttempts}
	}
	return nil
}

// Show records a page shown from collection at tier.
func (s *Session) Show(collection string, tier int, resident bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.tracker.now()
	s.shows++
	s.attempts = 0
	s.touch(collection, tier, resident)
	if collection != "" {
		s.shown[collection] = true
	}
	if s.firstContext.IsZero() {
		s.firstContext = now
	}
	s.lastShow = now
	s.searchedAfter = false
}

func (s *Session) touch(collection string, tier int, resident bool) {
	if collection != "" {
		seen := false
		for _, c := range s.touched {
			if c == collection {
				seen = true
				break
			}
		}
		if !seen {
			s.touched = append(s.touched, collection)
			if !resident {
				s.hot = false
			}
		}
		s.lastCollection = collection
	}
	if tier > s.tierReached {
		s.tierReached = tier
	}
}

// Summary is what a session reports when it ends.
type Summary struct {
	Outcome       string
	Hops          int
	Steps         int
	Attempts      int
	WrongTurns    int
	Hot           bool
	TierReached   int
	FirstContext  time.Duration // 0 if never
	FirstRelevant time.Duration
	Duration      time.Duration
}

// End closes the session with an outcome (and the consumer's quality,
// when reported), observes the SLIs, ends the span and forgets the
// session. Ending twice is a no-op.
func (t *Tracker) End(ctx context.Context, key, outcome string, q *Quality) *Summary {
	if t == nil || key == "" {
		return nil
	}
	t.mu.Lock()
	s, ok := t.sessions[key]
	if ok {
		delete(t.sessions, key)
	}
	t.mu.Unlock()
	if !ok {
		return nil
	}
	return s.end(ctx, outcome, q)
}

func (s *Session) end(ctx context.Context, outcome string, q *Quality) *Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil
	}
	s.ended = true
	now := s.tracker.now()
	sum := &Summary{Outcome: outcome, Hops: s.hops, Steps: s.steps, Attempts: s.attempts, Hot: s.hot, TierReached: s.tierReached, Duration: now.Sub(s.started)}
	for _, c := range s.touched[min(1, len(s.touched)):] {
		if !s.shown[c] {
			sum.WrongTurns++
		}
	}
	if !s.firstContext.IsZero() {
		sum.FirstContext = s.firstContext.Sub(s.started)
	}
	// first_relevant_context: the last show not followed by a search,
	// confirmed by a found report; a gave_up report says nothing was.
	if !s.lastShow.IsZero() && outcome != OutcomeGaveUp && (outcome == OutcomeFound || !s.searchedAfter) {
		sum.FirstRelevant = s.lastShow.Sub(s.started)
	}

	m := telemetry.Record(ctx)
	m.RetrievalSession(outcome, sum.TierReached, sum.Hot, sum.Hops, sum.Steps, sum.WrongTurns,
		sum.FirstContext.Seconds(), sum.FirstRelevant.Seconds(), sum.Duration.Seconds())
	if q != nil {
		m.RetrievalQuality(sum.TierReached, q.Accuracy, q.Completeness, q.AnswerQuality)
	}
	s.span.SetAttributes(
		telemetry.KeyRetrievalOutcome.String(outcome),
		telemetry.KeyRetrievalHops.Int(sum.Hops),
		telemetry.KeyRetrievalSteps.Int(sum.Steps),
		telemetry.KeyRetrievalAttempts.Int(sum.Attempts),
		telemetry.KeyRetrievalWrongTurns.Int(sum.WrongTurns),
		telemetry.KeyRetrievalHotPath.Bool(sum.Hot),
		telemetry.KeyRetrievalTierReached.Int(sum.TierReached),
		telemetry.Outcome(telemetry.OutcomeOK),
	)
	s.span.End()
	return sum
}

// Sweep ends every session idle longer than the timeout: gave_up when
// nothing was ever shown, timeout otherwise. It returns how many ended.
func (t *Tracker) Sweep(ctx context.Context) int {
	if t == nil {
		return 0
	}
	now := t.now()
	var expired []*Session
	t.mu.Lock()
	for key, s := range t.sessions {
		s.mu.Lock()
		idle := now.Sub(s.last) >= t.idle
		s.mu.Unlock()
		if idle {
			expired = append(expired, s)
			delete(t.sessions, key)
		}
	}
	t.mu.Unlock()
	for _, s := range expired {
		outcome := OutcomeTimeout
		s.mu.Lock()
		if s.shows == 0 {
			outcome = OutcomeGaveUp
		}
		s.mu.Unlock()
		s.end(ctx, outcome, nil)
	}
	return len(expired)
}

// Run sweeps until ctx ends, then ends every remaining session as
// timeout so nothing is lost at shutdown.
func (t *Tracker) Run(ctx context.Context) {
	if t == nil {
		return
	}
	interval := t.idle / 4
	if interval < time.Second {
		interval = time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			bg := context.WithoutCancel(ctx)
			t.mu.Lock()
			rest := make([]*Session, 0, len(t.sessions))
			for k, s := range t.sessions {
				rest = append(rest, s)
				delete(t.sessions, k)
			}
			t.mu.Unlock()
			for _, s := range rest {
				s.end(bg, OutcomeTimeout, nil)
			}
			return
		case <-tick.C:
			t.Sweep(ctx)
		}
	}
}

// Live returns how many sessions are open.
func (t *Tracker) Live() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sessions)
}

// IsLimit reports whether err is a limit error and returns it.
func IsLimit(err error) (*ErrLimitReached, bool) {
	var e *ErrLimitReached
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
