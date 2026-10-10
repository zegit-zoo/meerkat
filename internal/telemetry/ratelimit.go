package telemetry

import (
	"context"
	"sync"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ratelimit.go caps how many spans per second reach the exporter queue.
//
// The queue bound (export.go) limits MEMORY; it does not limit RATE, and
// a burst that fills it evicts the spans an operator actually wants. A
// token bucket in front of it makes the steady-state cost of tracing a
// configured number rather than a function of request volume.

// tokenBucket is a classic token bucket: rate tokens per second, a burst
// of one second's worth.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(perSecond int, now func() time.Time) *tokenBucket {
	if now == nil {
		now = time.Now
	}
	return &tokenBucket{rate: float64(perSecond), tokens: float64(perSecond), last: now(), now: now}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	b.tokens = min(b.rate, b.tokens+t.Sub(b.last).Seconds()*b.rate)
	b.last = t
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// limitProcessor drops ended spans beyond the bucket's rate and counts
// them with the queue's own drop counter.
type limitProcessor struct {
	next    sdktrace.SpanProcessor
	bucket  *tokenBucket
	metrics *Metrics
}

func newLimitProcessor(next sdktrace.SpanProcessor, perSecond int, m *Metrics) sdktrace.SpanProcessor {
	if perSecond <= 0 {
		return next
	}
	return &limitProcessor{next: next, bucket: newTokenBucket(perSecond, nil), metrics: m}
}

func (p *limitProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.next.OnStart(ctx, s)
}

func (p *limitProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	if !p.bucket.allow() {
		p.metrics.SpansDropped(1)
		return
	}
	p.next.OnEnd(s)
}

func (p *limitProcessor) ForceFlush(ctx context.Context) error { return p.next.ForceFlush(ctx) }
func (p *limitProcessor) Shutdown(ctx context.Context) error   { return p.next.Shutdown(ctx) }
