package ratelimit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Fallback wraps a primary Limiter and degrades to a secondary one when the
// primary is failing.
//
// The judgement encoded here is that quota is a cost control, not a safety
// control. When Redis is unavailable the choice is between refusing all
// traffic and enforcing limits per replica; refusing all traffic turns a
// cache-tier incident into a platform outage, so the gateway degrades to
// per-replica limits, marks the degradation loudly, and alerts. Guardrails,
// which are a safety control, make the opposite choice — see
// docs/adr/0010-fail-closed-guardrails-for-restricted-data.md.
type Fallback struct {
	primary   Limiter
	secondary Limiter
	logger    *slog.Logger
	onDegrade func(degraded bool)

	degraded  atomic.Bool
	failures  atomic.Int64
	lastProbe atomic.Int64 // unix nano
	probeGap  time.Duration
}

// NewFallback wraps primary with secondary.
func NewFallback(primary, secondary Limiter, logger *slog.Logger, onDegrade func(bool)) *Fallback {
	if logger == nil {
		logger = slog.Default()
	}
	if onDegrade == nil {
		onDegrade = func(bool) {}
	}
	return &Fallback{primary: primary, secondary: secondary, logger: logger, onDegrade: onDegrade, probeGap: 5 * time.Second}
}

// Degraded reports whether the limiter is currently running on the secondary.
func (f *Fallback) Degraded() bool { return f.degraded.Load() }

func (f *Fallback) active(ctx context.Context) Limiter {
	if !f.degraded.Load() {
		return f.primary
	}
	// Probe the primary occasionally so recovery is automatic.
	last := time.Unix(0, f.lastProbe.Load())
	if time.Since(last) > f.probeGap {
		f.lastProbe.Store(time.Now().UnixNano())
		if f.primary.Healthy(ctx) {
			f.degraded.Store(false)
			f.onDegrade(false)
			f.logger.Info("rate limiter recovered; distributed limits are in force again")
			return f.primary
		}
	}
	return f.secondary
}

func (f *Fallback) fail(err error) {
	if err == nil {
		return
	}
	f.failures.Add(1)
	if f.degraded.CompareAndSwap(false, true) {
		f.onDegrade(true)
		f.lastProbe.Store(time.Now().UnixNano())
		f.logger.Error("rate limiter degraded to per-replica limits; quota is no longer enforced fleet-wide",
			"error", err)
	}
}

// AllowRequest checks the request limit, degrading on primary failure.
func (f *Fallback) AllowRequest(ctx context.Context, key string, limits Limits) (Result, error) {
	l := f.active(ctx)
	res, err := l.AllowRequest(ctx, key, limits)
	if err != nil && l == f.primary {
		f.fail(err)
		return f.secondary.AllowRequest(ctx, key, limits)
	}
	return res, err
}

// Reserve reserves tokens, degrading on primary failure.
func (f *Fallback) Reserve(ctx context.Context, key string, tokens int64, limits Limits) (Result, *Reservation, error) {
	l := f.active(ctx)
	res, r, err := l.Reserve(ctx, key, tokens, limits)
	if err != nil && l == f.primary {
		f.fail(err)
		return f.secondary.Reserve(ctx, key, tokens, limits)
	}
	return res, r, err
}

// Settle reconciles usage against whichever limiter is active.
func (f *Fallback) Settle(ctx context.Context, key string, reserved, actual int64) error {
	l := f.active(ctx)
	if err := l.Settle(ctx, key, reserved, actual); err != nil {
		if l == f.primary {
			f.fail(err)
			return f.secondary.Settle(ctx, key, reserved, actual)
		}
		return err
	}
	return nil
}

// Usage reports monthly consumption from the active limiter.
func (f *Fallback) Usage(ctx context.Context, key string) (int64, error) {
	return f.active(ctx).Usage(ctx, key)
}

// Healthy reports the primary's health.
func (f *Fallback) Healthy(ctx context.Context) bool { return f.primary.Healthy(ctx) }

// Close closes both limiters.
func (f *Fallback) Close() error {
	_ = f.secondary.Close()
	return f.primary.Close()
}
