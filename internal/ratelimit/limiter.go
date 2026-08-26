// Package ratelimit implements the token-aware limits the gateway enforces:
// requests per minute, tokens per minute with reserve-and-settle, and a
// monthly token budget.
//
// The reserve-and-settle shape is the point. A request-per-minute limit does
// not protect a model backend, because one request can be a hundred tokens or
// a hundred thousand. The gateway therefore estimates the cost of a request
// before sending it, reserves that many tokens from the agent's bucket, and
// returns the difference once the real usage is known. An agent that
// consistently under-declares max_tokens is throttled on its estimates, which
// is the correct incentive.
package ratelimit

import (
	"context"
	"time"
)

// Decision is the outcome of a limit check.
type Decision string

// Limit decisions. These strings are metric label values.
const (
	DecisionAllow  Decision = "allow"
	DecisionLimit  Decision = "limit"  // requests per minute
	DecisionQuota  Decision = "quota"  // tokens per minute
	DecisionBudget Decision = "budget" // monthly budget
	DecisionShed   Decision = "shed"   // load shed before limiting
)

// Result carries a limit decision and the state a caller needs to build the
// rate-limit response headers.
type Result struct {
	Decision   Decision
	Limit      int64
	Remaining  int64
	ResetAfter time.Duration
	RetryAfter time.Duration
	Reason     string
}

// Allowed reports whether the request may proceed.
func (r Result) Allowed() bool { return r.Decision == DecisionAllow }

// Limits is one agent's declared envelope.
type Limits struct {
	RequestsPerMinute  int64
	TokensPerMinute    int64
	MonthlyTokenBudget int64
}

// Reservation is an outstanding token reservation that must be settled.
type Reservation struct {
	Key      string
	Reserved int64
	Settled  bool
	limiter  Limiter
}

// Settle returns the difference between the reserved and actual token counts
// to the bucket and records the actual against the monthly budget. It is safe
// to call twice; the second call is a no-op.
func (r *Reservation) Settle(ctx context.Context, actual int64) error {
	if r == nil || r.Settled || r.limiter == nil {
		return nil
	}
	r.Settled = true
	return r.limiter.Settle(ctx, r.Key, r.Reserved, actual)
}

// Limiter enforces limits for one key.
//
// Two implementations ship: Memory, used in development, in tests and as the
// fallback when Redis is unavailable, and Redis, used in production where the
// limit must hold across every gateway replica.
type Limiter interface {
	// AllowRequest consumes one request from the per-minute request bucket.
	AllowRequest(ctx context.Context, key string, limits Limits) (Result, error)
	// Reserve takes tokens from the per-minute token bucket and checks the
	// monthly budget.
	Reserve(ctx context.Context, key string, tokens int64, limits Limits) (Result, *Reservation, error)
	// Settle reconciles a reservation against actual usage.
	Settle(ctx context.Context, key string, reserved, actual int64) error
	// Usage reports the monthly consumption for a key.
	Usage(ctx context.Context, key string) (int64, error)
	// Healthy reports whether the backing store is reachable.
	Healthy(ctx context.Context) bool
	Close() error
}

// monthKey groups monthly budget accounting. Using the calendar month in UTC
// rather than a rolling window matches how the client's finance team reports,
// which is the only reason a budget number is ever looked at.
func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }

// resetAfter returns the time until the current minute window rolls over.
func resetAfter(now time.Time) time.Duration {
	return now.Truncate(time.Minute).Add(time.Minute).Sub(now)
}
