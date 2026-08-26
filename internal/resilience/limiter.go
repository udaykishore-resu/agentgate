package resilience

import (
	"context"
	"sync"
	"sync/atomic"
)

// Priority is the request tier used by the load shedder.
type Priority string

// Request priorities. Interactive traffic is a person waiting; batch traffic
// is not, so batch is what gets shed first.
const (
	PriorityInteractive Priority = "interactive"
	PriorityBatch       Priority = "batch"
)

// ParsePriority maps a header value to a priority, defaulting to interactive
// because an unlabelled request is more likely to have a human behind it.
func ParsePriority(s string) Priority {
	if Priority(s) == PriorityBatch {
		return PriorityBatch
	}
	return PriorityInteractive
}

// ConcurrencyLimiter bounds in-flight work and sheds low-priority requests
// before high-priority ones.
//
// The reserve is what makes the two tiers mean something: batch traffic may
// only fill the pool up to the shed threshold, leaving headroom that
// interactive traffic can always claim. Without a reserve, a batch job that
// arrives first simply wins.
type ConcurrencyLimiter struct {
	limit         int64
	shedThreshold int64

	inflight atomic.Int64
	shed     atomic.Int64

	mu      sync.Mutex
	waiters int
}

// NewConcurrencyLimiter builds a limiter. batchReserveRatio is the fraction of
// capacity held back from batch traffic.
func NewConcurrencyLimiter(limit int64, batchReserveRatio float64) *ConcurrencyLimiter {
	if limit <= 0 {
		limit = 1024
	}
	if batchReserveRatio < 0 || batchReserveRatio >= 1 {
		batchReserveRatio = 0.2
	}
	return &ConcurrencyLimiter{
		limit:         limit,
		shedThreshold: int64(float64(limit) * (1 - batchReserveRatio)),
	}
}

// Acquire admits a request, returning a release function. ok is false when the
// request must be shed.
func (l *ConcurrencyLimiter) Acquire(_ context.Context, p Priority) (release func(), ok bool) {
	current := l.inflight.Add(1)
	ceiling := l.limit
	if p == PriorityBatch {
		ceiling = l.shedThreshold
	}
	if current > ceiling {
		l.inflight.Add(-1)
		l.shed.Add(1)
		return func() {}, false
	}
	var once sync.Once
	return func() { once.Do(func() { l.inflight.Add(-1) }) }, true
}

// Inflight reports current in-flight requests.
func (l *ConcurrencyLimiter) Inflight() int64 { return l.inflight.Load() }

// Shed reports how many requests have been shed.
func (l *ConcurrencyLimiter) Shed() int64 { return l.shed.Load() }

// Limit reports the configured ceiling.
func (l *ConcurrencyLimiter) Limit() int64 { return l.limit }

// Saturation reports in-flight work as a fraction of the ceiling, which is the
// signal the autoscaler and the load-shedding alert both use.
func (l *ConcurrencyLimiter) Saturation() float64 {
	if l.limit == 0 {
		return 0
	}
	return float64(l.inflight.Load()) / float64(l.limit)
}
