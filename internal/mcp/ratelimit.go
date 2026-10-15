package mcp

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/zegit-zoo/meerkat/internal/authz"
)

// ratelimit.go gates mk_report_outcome's writes to the traversal log
// (meerkat-mob#45, item 1). Every report used to become one stored
// object, for any caller who could read, with no bound on how many. Now
// a report reaches the log only when all three hold:
//
//  1. The caller is a principal. A caller admitted ANONYMOUSLY under a
//     policy (an `anonymous: true` rule) has no identity to account the
//     write to, so its report is recorded in telemetry and answered
//     `log: not_permitted` — the same no-op success, with a status,
//     that intake gives a caller without intake-write. A process with no
//     policy at all (stdio, or a hosted server with no auth: block) is
//     the trusted local shape, as it is for intake and personal memory.
//  2. It is the session's first report. A report that closes a live
//     retrieval session is always its first; one for a session key that
//     was already reported and has not been used for a new session since
//     is answered `log: duplicate`.
//  3. The caller's principal has a token left in its bucket
//     (ReportLimits); otherwise `log: rate_limited`.
//
// Telemetry is recorded whatever the verdict: it is a counter, not
// storage.

// Log verdicts, as the response's "log" field reports them.
const (
	logWritten       = "written"
	logNotConfigured = "not_configured"
	logNotPermitted  = "not_permitted"
	logDuplicate     = "duplicate"
	logRateLimited   = "rate_limited"
)

// ReportLimits bounds traversal-log writes per principal. Zero fields
// take the defaults.
type ReportLimits struct {
	// Burst is how many reports a principal may log back to back.
	Burst int
	// Every is how often a principal earns one more.
	Every time.Duration
	// Remember is how long a reported session key is remembered for
	// the one-report-per-session rule.
	Remember time.Duration
}

// Defaults: generous for an agent that reports once per question, and
// a hard ceiling of about 120 objects an hour per principal after the
// burst.
const (
	defaultReportBurst    = 20
	defaultReportEvery    = 30 * time.Second
	defaultReportRemember = time.Hour
	// maxTracked bounds both the bucket set and the reported-key set.
	maxTracked = 10000
)

func (l ReportLimits) withDefaults() ReportLimits {
	if l.Burst <= 0 {
		l.Burst = defaultReportBurst
	}
	if l.Every <= 0 {
		l.Every = defaultReportEvery
	}
	if l.Remember <= 0 {
		l.Remember = defaultReportRemember
	}
	return l
}

// buckets is a set of token buckets keyed by principal.
type buckets struct {
	burst float64
	every time.Duration
	max   int
	now   func() time.Time

	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newBuckets(burst int, every time.Duration) *buckets {
	return &buckets{burst: float64(burst), every: every, max: maxTracked, now: time.Now, m: map[string]*bucket{}}
}

// refill brings bk up to now.
func (b *buckets) refill(bk *bucket, now time.Time) {
	bk.tokens = min(b.burst, bk.tokens+float64(now.Sub(bk.at))/float64(b.every))
	bk.at = now
}

// allow takes a token from key's bucket, if there is one.
//
// When the set is full, buckets that have refilled completely are
// dropped first (a full bucket and an absent one are the same thing). If
// it is still full, a new key is refused: failing closed costs a log
// line, and failing open would hand a fresh bucket to whoever arrived
// last.
func (b *buckets) allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	bk, ok := b.m[key]
	if !ok {
		if len(b.m) >= b.max {
			for k, other := range b.m {
				b.refill(other, now)
				if other.tokens >= b.burst {
					delete(b.m, k)
				}
			}
			if len(b.m) >= b.max {
				return false
			}
		}
		bk = &bucket{tokens: b.burst, at: now}
		b.m[key] = bk
	}
	b.refill(bk, now)
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// reportGate decides whether one report may be written to the log.
type reportGate struct {
	limiter  *buckets
	remember time.Duration
	now      func() time.Time

	mu       sync.Mutex
	reported map[string]*list.Element // session key -> element holding a reportedKey
	order    *list.List               // oldest first
}

type reportedKey struct {
	key string
	at  time.Time
}

func newReportGate(l ReportLimits) *reportGate {
	l = l.withDefaults()
	return &reportGate{
		limiter: newBuckets(l.Burst, l.Every), remember: l.Remember, now: time.Now,
		reported: map[string]*list.Element{}, order: list.New(),
	}
}

// admit returns logWritten when the report may be logged, else the
// verdict to answer with. key is the principal-scoped session key ("" for
// a call with no session); closedLive says the report ended a live
// retrieval session.
func (g *reportGate) admit(ctx context.Context, key string, closedLive bool) string {
	if gr := authz.FromContext(ctx); gr != nil && gr.Identity().Subject == "" {
		return logNotPermitted
	}
	if key != "" && !closedLive && g.seen(key) {
		return logDuplicate
	}
	if !g.limiter.allow(principal(ctx)) {
		return logRateLimited
	}
	if key != "" {
		g.mark(key)
	}
	return logWritten
}

func (g *reportGate) seen(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	el, ok := g.reported[key]
	return ok && g.now().Sub(el.Value.(reportedKey).at) < g.remember
}

// mark remembers key as reported now. Expired keys leave from the
// front, and a full set drops its oldest, so the set is bounded and
// never scanned.
func (g *reportGate) mark(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if el, ok := g.reported[key]; ok {
		el.Value = reportedKey{key: key, at: now}
		g.order.MoveToBack(el)
		return
	}
	for front := g.order.Front(); front != nil && now.Sub(front.Value.(reportedKey).at) >= g.remember; front = g.order.Front() {
		g.drop(front)
	}
	if len(g.reported) >= maxTracked {
		g.drop(g.order.Front())
	}
	g.reported[key] = g.order.PushBack(reportedKey{key: key, at: now})
}

func (g *reportGate) drop(el *list.Element) {
	delete(g.reported, el.Value.(reportedKey).key)
	g.order.Remove(el)
}
