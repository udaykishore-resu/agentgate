package ratelimit

import (
	"context"
	"sync"
	"time"
)

// bucket is a continuously-refilling token bucket.
type bucket struct {
	tokens   float64
	capacity float64
	rate     float64 // tokens per second
	last     time.Time
}

func (b *bucket) refill(now time.Time) {
	if b.last.IsZero() {
		b.last = now
		b.tokens = b.capacity
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
}

func (b *bucket) take(now time.Time, n float64) bool {
	b.refill(now)
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

func (b *bucket) give(n float64) {
	b.tokens += n
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
}

// retryAfter estimates how long until n tokens are available.
func (b *bucket) retryAfter(n float64) time.Duration {
	if b.rate <= 0 {
		return time.Minute
	}
	deficit := n - b.tokens
	if deficit <= 0 {
		return 0
	}
	return time.Duration(deficit / b.rate * float64(time.Second))
}

type keyState struct {
	requests *bucket
	tokens   *bucket
	month    string
	consumed int64
}

// Memory is an in-process Limiter.
//
// It is exactly correct for a single replica and approximately correct for
// several, because each replica holds its own buckets. That approximation is
// acceptable in development and as a degraded mode; it is not acceptable as
// the production enforcement point, which is why config validation refuses it
// in production unless the operator has explicitly accepted per-replica
// limits.
type Memory struct {
	mu    sync.Mutex
	state map[string]*keyState
	now   func() time.Time
}

// NewMemory builds an in-process limiter.
func NewMemory() *Memory {
	return &Memory{state: map[string]*keyState{}, now: time.Now}
}

func (m *Memory) stateFor(key string, limits Limits, now time.Time) *keyState {
	s, ok := m.state[key]
	if !ok {
		s = &keyState{
			requests: &bucket{capacity: float64(limits.RequestsPerMinute), rate: float64(limits.RequestsPerMinute) / 60},
			tokens:   &bucket{capacity: float64(limits.TokensPerMinute), rate: float64(limits.TokensPerMinute) / 60},
			month:    monthKey(now),
		}
		m.state[key] = s
		return s
	}
	// A quota change takes effect without restarting the gateway, and without
	// handing the agent a full bucket as a side effect of the change.
	if s.requests.capacity != float64(limits.RequestsPerMinute) {
		s.requests.capacity = float64(limits.RequestsPerMinute)
		s.requests.rate = float64(limits.RequestsPerMinute) / 60
		if s.requests.tokens > s.requests.capacity {
			s.requests.tokens = s.requests.capacity
		}
	}
	if s.tokens.capacity != float64(limits.TokensPerMinute) {
		s.tokens.capacity = float64(limits.TokensPerMinute)
		s.tokens.rate = float64(limits.TokensPerMinute) / 60
		if s.tokens.tokens > s.tokens.capacity {
			s.tokens.tokens = s.tokens.capacity
		}
	}
	if mk := monthKey(now); s.month != mk {
		s.month, s.consumed = mk, 0
	}
	return s
}

// AllowRequest consumes one request from the per-minute request bucket.
func (m *Memory) AllowRequest(_ context.Context, key string, limits Limits) (Result, error) {
	if limits.RequestsPerMinute <= 0 {
		return Result{Decision: DecisionAllow}, nil
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stateFor(key, limits, now)
	if !s.requests.take(now, 1) {
		return Result{
			Decision: DecisionLimit, Limit: limits.RequestsPerMinute, Remaining: 0,
			ResetAfter: resetAfter(now), RetryAfter: s.requests.retryAfter(1),
			Reason: "requests per minute exceeded",
		}, nil
	}
	return Result{
		Decision: DecisionAllow, Limit: limits.RequestsPerMinute,
		Remaining: int64(s.requests.tokens), ResetAfter: resetAfter(now),
	}, nil
}

// Reserve takes tokens from the per-minute token bucket and checks the monthly
// budget.
func (m *Memory) Reserve(_ context.Context, key string, tokens int64, limits Limits) (Result, *Reservation, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stateFor(key, limits, now)

	if limits.MonthlyTokenBudget > 0 && s.consumed+tokens > limits.MonthlyTokenBudget {
		return Result{
			Decision: DecisionBudget, Limit: limits.MonthlyTokenBudget,
			Remaining: maxInt64(limits.MonthlyTokenBudget-s.consumed, 0),
			Reason:    "monthly token budget exhausted",
			// A monthly budget does not refill this minute. Telling the caller
			// to retry in a second would be a lie that produces a retry storm.
			RetryAfter: time.Hour,
		}, nil, nil
	}
	if limits.TokensPerMinute > 0 && !s.tokens.take(now, float64(tokens)) {
		return Result{
			Decision: DecisionQuota, Limit: limits.TokensPerMinute,
			Remaining: int64(s.tokens.tokens), ResetAfter: resetAfter(now),
			RetryAfter: s.tokens.retryAfter(float64(tokens)),
			Reason:     "tokens per minute exceeded",
		}, nil, nil
	}
	return Result{
			Decision: DecisionAllow, Limit: limits.TokensPerMinute,
			Remaining: int64(s.tokens.tokens), ResetAfter: resetAfter(now),
		}, &Reservation{
			Key: key, Reserved: tokens, limiter: m,
		}, nil
}

// Settle returns unused tokens and records actual monthly consumption.
func (m *Memory) Settle(_ context.Context, key string, reserved, actual int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.state[key]
	if !ok {
		return nil
	}
	if mk := monthKey(m.now()); s.month != mk {
		s.month, s.consumed = mk, 0
	}
	if diff := reserved - actual; diff > 0 {
		s.tokens.give(float64(diff))
	} else if diff < 0 {
		// The response was larger than reserved. The overshoot is charged
		// unconditionally and the bucket is allowed to go negative, so the
		// debt carries into the next minute. Charging it only when the bucket
		// happens to hold it — which is what a take() would do — means an
		// agent that systematically under-declares max_tokens is never
		// throttled, which is precisely the incentive this design exists to
		// create.
		s.tokens.refill(m.now())
		s.tokens.tokens -= float64(-diff)
	}
	s.consumed += actual
	return nil
}

// Usage reports monthly consumption.
func (m *Memory) Usage(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.state[key]
	if !ok {
		return 0, nil
	}
	if mk := monthKey(m.now()); s.month != mk {
		return 0, nil
	}
	return s.consumed, nil
}

// Healthy always reports true; the store is this process.
func (m *Memory) Healthy(context.Context) bool { return true }

// Close is a no-op.
func (m *Memory) Close() error { return nil }

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
