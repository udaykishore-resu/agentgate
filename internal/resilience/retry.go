package resilience

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// RetryConfig configures retry behaviour for one backend attempt sequence.
type RetryConfig struct {
	// MaxAttempts includes the first attempt.
	MaxAttempts int
	// BaseDelay is the first backoff interval.
	BaseDelay time.Duration
	// MaxDelay caps a single backoff interval.
	MaxDelay time.Duration
	// BudgetRatio caps the fraction of the caller's remaining deadline that
	// may be spent waiting between attempts. Without this, three retries
	// against a slow backend turn a 5-second budget into a 20-second one and
	// the caller times out having learned nothing.
	BudgetRatio float64
}

// DefaultRetryConfig matches SPEC.md section 3.3.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second, BudgetRatio: 0.25}
}

// Backoff returns the delay before the given attempt number, with full jitter.
// Full jitter, rather than equal or decorrelated jitter, because the failure
// mode being defended against is synchronised retry from hundreds of agent
// replicas, and full jitter spreads them widest.
func (c RetryConfig) Backoff(attempt int, retryAfter time.Duration, rnd *rand.Rand) time.Duration {
	if retryAfter > 0 {
		// A provider that tells the gateway when to come back is obeyed, up to
		// the cap; guessing better than the provider is not a winning strategy.
		if retryAfter > c.MaxDelay {
			return c.MaxDelay
		}
		return retryAfter
	}
	exp := float64(c.BaseDelay) * math.Pow(2, float64(attempt-1))
	if exp > float64(c.MaxDelay) {
		exp = float64(c.MaxDelay)
	}
	if rnd == nil {
		return time.Duration(exp)
	}
	return time.Duration(rnd.Float64() * exp)
}

// RemainingBudget returns how much of the caller's deadline may still be spent
// waiting between attempts.
func (c RetryConfig) RemainingBudget(ctx context.Context, spent time.Duration) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return c.MaxDelay
	}
	total := time.Until(dl)
	budget := time.Duration(float64(total) * c.BudgetRatio)
	if budget <= spent {
		return 0
	}
	return budget - spent
}

// RetryBudget is a fleet-wide cap on the fraction of requests that may be
// retried, expressed as a ratio of retries to primary requests over a sliding
// window.
//
// This is the control that prevents a retry storm. Per-request retry limits
// bound one caller's amplification; only a shared budget bounds the fleet's.
// When the budget is exhausted the gateway fails fast rather than retrying,
// which is the correct behaviour: if 10% of traffic is already failing, the
// backend does not need 10% more.
type RetryBudget struct {
	ratio  float64
	window time.Duration

	mu       sync.Mutex
	requests []time.Time
	retries  []time.Time
	rejected atomic.Int64
}

// NewRetryBudget builds a budget. ratio is retries permitted per primary
// request, so 0.1 means 10%.
func NewRetryBudget(ratio float64, window time.Duration) *RetryBudget {
	if ratio <= 0 {
		ratio = 0.1
	}
	if window <= 0 {
		window = 10 * time.Second
	}
	return &RetryBudget{ratio: ratio, window: window}
}

// Request records a primary request.
func (b *RetryBudget) Request() {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, now)
	b.trimLocked(now)
}

// Allow reports whether a retry may be spent, and records it when it may.
func (b *RetryBudget) Allow() bool {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trimLocked(now)
	// A small constant allowance keeps the budget usable at very low traffic,
	// where a strict ratio would forbid the first retry the gateway ever makes.
	allowance := float64(len(b.requests))*b.ratio + 3
	if float64(len(b.retries)) >= allowance {
		b.rejected.Add(1)
		return false
	}
	b.retries = append(b.retries, now)
	return true
}

// Rejected reports how many retries the budget has refused.
func (b *RetryBudget) Rejected() int64 { return b.rejected.Load() }

// Ratio reports the current retry-to-request ratio.
func (b *RetryBudget) Ratio() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trimLocked(time.Now())
	if len(b.requests) == 0 {
		return 0
	}
	return float64(len(b.retries)) / float64(len(b.requests))
}

func (b *RetryBudget) trimLocked(now time.Time) {
	cutoff := now.Add(-b.window)
	b.requests = trim(b.requests, cutoff)
	b.retries = trim(b.retries, cutoff)
}

func trim(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	if i == 0 {
		return ts
	}
	return append(ts[:0], ts[i:]...)
}

// Sleep waits for d or until the context is done, whichever comes first.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
