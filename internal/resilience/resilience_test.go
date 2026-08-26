package resilience

import (
	"context"
	"math/rand"
	"testing"
	"time"
)

func testConfig(now func() time.Time) BreakerConfig {
	c := DefaultBreakerConfig()
	c.Now = now
	return c
}

func TestBreakerTripsOnConsecutiveFailures(t *testing.T) {
	clock := time.Now()
	b := NewBreaker("backend", testConfig(func() time.Time { return clock }), nil)
	for i := 0; i < 10; i++ {
		if !b.Allow() {
			t.Fatalf("breaker opened early, after %d failures", i)
		}
		b.Failure()
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open after 10 consecutive failures", b.State())
	}
	if b.Allow() {
		t.Error("an open breaker must refuse calls")
	}
}

func TestBreakerTripsOnFailureRatio(t *testing.T) {
	clock := time.Now()
	b := NewBreaker("backend", testConfig(func() time.Time { return clock }), nil)
	// Alternate success and failure so the consecutive-failure rule never
	// fires; only the ratio rule can trip this.
	for i := 0; i < 40; i++ {
		b.Allow()
		if i%2 == 0 {
			b.Failure()
		} else {
			b.Success()
		}
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open at a 50%% failure ratio", b.State())
	}
}

func TestBreakerHalfOpenRecovery(t *testing.T) {
	clock := time.Now()
	cfg := testConfig(func() time.Time { return clock })
	transitions := []State{}
	b := NewBreaker("backend", cfg, func(_ string, _, to State) { transitions = append(transitions, to) })

	for i := 0; i < cfg.ConsecutiveFailures; i++ {
		b.Allow()
		b.Failure()
	}
	if b.State() != StateOpen {
		t.Fatal("expected the breaker to be open")
	}

	clock = clock.Add(cfg.OpenDuration + time.Second)
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %s, want half_open after the open duration", b.State())
	}
	admitted := 0
	for i := 0; i < cfg.HalfOpenProbes+3; i++ {
		if b.Allow() {
			admitted++
		}
	}
	if admitted != cfg.HalfOpenProbes {
		t.Errorf("half-open admitted %d probes, want %d", admitted, cfg.HalfOpenProbes)
	}
	for i := 0; i < cfg.HalfOpenProbes; i++ {
		b.Success()
	}
	if b.State() != StateClosed {
		t.Fatalf("state = %s, want closed after successful probes", b.State())
	}
	if len(transitions) < 3 {
		t.Errorf("expected open, half_open and closed transitions, got %v", transitions)
	}
}

func TestBreakerHalfOpenReopensOnOneFailure(t *testing.T) {
	clock := time.Now()
	cfg := testConfig(func() time.Time { return clock })
	b := NewBreaker("backend", cfg, nil)
	for i := 0; i < cfg.ConsecutiveFailures; i++ {
		b.Allow()
		b.Failure()
	}
	clock = clock.Add(cfg.OpenDuration + time.Second)
	b.Allow()
	b.Failure()
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open again after a failed probe", b.State())
	}
}

func TestRetryBackoffHonoursRetryAfterAndCap(t *testing.T) {
	cfg := DefaultRetryConfig()
	rnd := rand.New(rand.NewSource(1))
	if got := cfg.Backoff(1, 700*time.Millisecond, rnd); got != 700*time.Millisecond {
		t.Errorf("Retry-After should be obeyed, got %s", got)
	}
	if got := cfg.Backoff(1, time.Hour, rnd); got != cfg.MaxDelay {
		t.Errorf("an absurd Retry-After must be capped, got %s", got)
	}
	for attempt := 1; attempt <= 6; attempt++ {
		if got := cfg.Backoff(attempt, 0, rnd); got > cfg.MaxDelay {
			t.Errorf("attempt %d backoff %s exceeds the cap", attempt, got)
		}
	}
}

func TestRetryBudgetCapsFleetRetries(t *testing.T) {
	b := NewRetryBudget(0.1, time.Minute)
	for i := 0; i < 100; i++ {
		b.Request()
	}
	allowed := 0
	for i := 0; i < 100; i++ {
		if b.Allow() {
			allowed++
		}
	}
	// 10% of 100 requests plus the small constant allowance.
	if allowed < 10 || allowed > 14 {
		t.Errorf("allowed %d retries against 100 requests, want about 13", allowed)
	}
	if b.Rejected() == 0 {
		t.Error("rejections should be counted")
	}
}

func TestRetryBudgetAllowsFirstRetriesAtLowTraffic(t *testing.T) {
	b := NewRetryBudget(0.1, time.Minute)
	b.Request()
	if !b.Allow() {
		t.Error("the first retry at very low traffic must be permitted")
	}
}

func TestRemainingBudgetTracksDeadline(t *testing.T) {
	cfg := DefaultRetryConfig()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	budget := cfg.RemainingBudget(ctx, 0)
	if budget <= 0 || budget > 260*time.Millisecond {
		t.Errorf("budget %s should be about a quarter of a one second deadline", budget)
	}
	if spent := cfg.RemainingBudget(ctx, time.Second); spent != 0 {
		t.Errorf("an exhausted budget should be zero, got %s", spent)
	}
}

func TestConcurrencyLimiterShedsBatchFirst(t *testing.T) {
	l := NewConcurrencyLimiter(10, 0.2) // batch may fill to 8, interactive to 10
	var releases []func()
	for i := 0; i < 8; i++ {
		rel, ok := l.Acquire(context.Background(), PriorityBatch)
		if !ok {
			t.Fatalf("batch request %d was shed too early", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := l.Acquire(context.Background(), PriorityBatch); ok {
		t.Error("batch traffic must be shed once it reaches the reserve threshold")
	}
	rel, ok := l.Acquire(context.Background(), PriorityInteractive)
	if !ok {
		t.Error("interactive traffic must still be admitted into the reserve")
	}
	releases = append(releases, rel)
	for _, r := range releases {
		r()
	}
	if l.Inflight() != 0 {
		t.Errorf("inflight = %d after all releases", l.Inflight())
	}
	if l.Shed() == 0 {
		t.Error("shed requests must be counted")
	}
}

func TestParsePriority(t *testing.T) {
	if ParsePriority("batch") != PriorityBatch {
		t.Error("batch not parsed")
	}
	for _, v := range []string{"", "interactive", "nonsense"} {
		if ParsePriority(v) != PriorityInteractive {
			t.Errorf("%q should default to interactive", v)
		}
	}
}

func TestSleepRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); err == nil {
		t.Error("Sleep must return when the context is done")
	}
}
