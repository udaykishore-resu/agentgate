package ratelimit

import (
	"context"
	"testing"
	"time"
)

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestRequestLimit(t *testing.T) {
	now := time.Now()
	m := NewMemory()
	m.now = fixedClock(&now)
	limits := Limits{RequestsPerMinute: 5, TokensPerMinute: 1000}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		res, err := m.AllowRequest(ctx, "k", limits)
		if err != nil || !res.Allowed() {
			t.Fatalf("request %d refused: %v %v", i, res.Decision, err)
		}
	}
	res, _ := m.AllowRequest(ctx, "k", limits)
	if res.Allowed() {
		t.Fatal("the sixth request within a minute must be limited")
	}
	if res.Decision != DecisionLimit {
		t.Errorf("decision = %s, want limit", res.Decision)
	}
	if res.RetryAfter <= 0 {
		t.Error("a limited request must carry a retry hint")
	}

	// The bucket refills continuously rather than resetting on a boundary.
	now = now.Add(13 * time.Second)
	if res, _ := m.AllowRequest(ctx, "k", limits); !res.Allowed() {
		t.Error("the bucket should have refilled after 13 seconds at 5/min")
	}
}

func TestReserveAndSettleReturnsUnusedTokens(t *testing.T) {
	now := time.Now()
	m := NewMemory()
	m.now = fixedClock(&now)
	limits := Limits{RequestsPerMinute: 1000, TokensPerMinute: 1000}
	ctx := context.Background()

	res, reservation, err := m.Reserve(ctx, "k", 800, limits)
	if err != nil || !res.Allowed() {
		t.Fatalf("reserve: %v %v", res.Decision, err)
	}
	if _, _, err := m.Reserve(ctx, "k", 300, limits); err != nil {
		t.Fatal(err)
	}
	// 800 reserved of 1000 leaves 200; a 300-token request cannot fit.
	if res2, _, _ := m.Reserve(ctx, "k", 300, limits); res2.Allowed() {
		t.Error("a request larger than the remaining budget must be refused")
	}

	// The response only used 100 of the 800 reserved tokens.
	if err := reservation.Settle(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if res3, _, _ := m.Reserve(ctx, "k", 600, limits); !res3.Allowed() {
		t.Error("settling should have returned the unused reservation to the bucket")
	}
}

func TestSettleIsIdempotent(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	limits := Limits{TokensPerMinute: 1000, RequestsPerMinute: 100}
	_, reservation, _ := m.Reserve(ctx, "k", 500, limits)
	if err := reservation.Settle(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(ctx, 100); err != nil {
		t.Fatal(err)
	}
	used, _ := m.Usage(ctx, "k")
	if used != 100 {
		t.Errorf("monthly usage = %d, want 100; settling twice must not double count", used)
	}
}

func TestOvershootIsCharged(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	limits := Limits{TokensPerMinute: 1000, RequestsPerMinute: 100}
	_, reservation, _ := m.Reserve(ctx, "k", 100, limits)
	// The response was far larger than the caller declared.
	if err := reservation.Settle(ctx, 900); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := m.Reserve(ctx, "k", 500, limits); res.Allowed() {
		t.Error("an agent that systematically under-declares must be throttled on actual usage")
	}
	used, _ := m.Usage(ctx, "k")
	if used != 900 {
		t.Errorf("monthly usage = %d, want the actual 900", used)
	}
}

func TestMonthlyBudget(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	limits := Limits{TokensPerMinute: 1000000, RequestsPerMinute: 1000000, MonthlyTokenBudget: 1000}

	_, reservation, _ := m.Reserve(ctx, "k", 900, limits)
	if err := reservation.Settle(ctx, 900); err != nil {
		t.Fatal(err)
	}
	res, _, _ := m.Reserve(ctx, "k", 200, limits)
	if res.Allowed() {
		t.Fatal("a request that would exceed the monthly budget must be refused")
	}
	if res.Decision != DecisionBudget {
		t.Errorf("decision = %s, want budget", res.Decision)
	}
	if res.RetryAfter < time.Minute {
		t.Error("a monthly budget refusal must not invite an immediate retry")
	}
}

func TestQuotaChangeTakesEffectWithoutRefill(t *testing.T) {
	now := time.Now()
	m := NewMemory()
	m.now = fixedClock(&now)
	ctx := context.Background()

	small := Limits{TokensPerMinute: 1000, RequestsPerMinute: 100}
	if _, _, err := m.Reserve(ctx, "k", 900, small); err != nil {
		t.Fatal(err)
	}
	// Lowering the limit must not hand the agent a full bucket.
	tiny := Limits{TokensPerMinute: 200, RequestsPerMinute: 100}
	res, _, _ := m.Reserve(ctx, "k", 200, tiny)
	if res.Allowed() {
		t.Error("lowering a quota must not refill the bucket")
	}
}

func TestZeroLimitsMeanUnlimited(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	res, err := m.AllowRequest(ctx, "k", Limits{})
	if err != nil || !res.Allowed() {
		t.Errorf("an unset request limit should not refuse traffic: %v %v", res.Decision, err)
	}
	res2, reservation, err := m.Reserve(ctx, "k", 1_000_000, Limits{})
	if err != nil || !res2.Allowed() || reservation == nil {
		t.Errorf("an unset token limit should not refuse traffic: %v %v", res2.Decision, err)
	}
}

// failingLimiter always errors, standing in for an unreachable Redis.
type failingLimiter struct{ *Memory }

func (failingLimiter) AllowRequest(context.Context, string, Limits) (Result, error) {
	return Result{}, context.DeadlineExceeded
}
func (failingLimiter) Reserve(context.Context, string, int64, Limits) (Result, *Reservation, error) {
	return Result{}, nil, context.DeadlineExceeded
}
func (failingLimiter) Settle(context.Context, string, int64, int64) error {
	return context.DeadlineExceeded
}
func (failingLimiter) Healthy(context.Context) bool { return false }
func (failingLimiter) Close() error                 { return nil }

func TestFallbackDegradesRatherThanFailing(t *testing.T) {
	var degraded bool
	f := NewFallback(failingLimiter{Memory: NewMemory()}, NewMemory(), nil, func(d bool) { degraded = d })
	ctx := context.Background()
	limits := Limits{RequestsPerMinute: 10, TokensPerMinute: 1000}

	res, err := f.AllowRequest(ctx, "k", limits)
	if err != nil {
		t.Fatalf("the fallback should absorb the primary's failure: %v", err)
	}
	if !res.Allowed() {
		t.Error("traffic must continue on the secondary limiter")
	}
	if !f.Degraded() || !degraded {
		t.Error("degradation must be visible, not silent")
	}

	// The secondary still enforces, just per replica.
	for i := 0; i < 20; i++ {
		res, _ = f.AllowRequest(ctx, "k", limits)
	}
	if res.Allowed() {
		t.Error("the secondary limiter must still enforce the limit")
	}
}

func TestOvershootLeavesTheBucketInDebt(t *testing.T) {
	now := time.Now()
	m := NewMemory()
	m.now = fixedClock(&now)
	ctx := context.Background()
	limits := Limits{TokensPerMinute: 1000, RequestsPerMinute: 1000}

	// Declare 10 tokens, consume 100,000. The bucket cannot hold the debt, and
	// dropping it silently is exactly what lets a systematically
	// under-declaring agent run unthrottled.
	_, reservation, _ := m.Reserve(ctx, "k", 10, limits)
	if err := reservation.Settle(ctx, 100_000); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := m.Reserve(ctx, "k", 1, limits); res.Allowed() {
		t.Fatal("the very next request must be refused; the debt did not carry")
	}
	// The debt is large enough that a minute of refill does not clear it.
	now = now.Add(time.Minute)
	if res, _, _ := m.Reserve(ctx, "k", 1, limits); res.Allowed() {
		t.Error("one minute of refill should not clear a hundred-fold overshoot")
	}
	// Long enough, and the agent recovers.
	now = now.Add(200 * time.Minute)
	if res, _, _ := m.Reserve(ctx, "k", 1, limits); !res.Allowed() {
		t.Error("the debt must eventually clear")
	}
}
