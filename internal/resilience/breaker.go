// Package resilience holds the failure-handling primitives the gateway uses:
// circuit breaking, retry with a bounded budget, and concurrency limiting.
//
// Each of these is a way of spending a little availability to protect a lot of
// it. Getting the constants wrong turns them into amplifiers instead: an
// unbounded retry converts a slow backend into an outage, and a breaker that
// trips too eagerly converts a blip into a failover storm. The defaults here
// are the ones argued for in docs/adr/0006-circuit-breaker-parameters.md.
package resilience

import (
	"errors"
	"sync"
	"time"
)

// State is a circuit breaker state.
type State string

// Breaker states.
const (
	StateClosed   State = "closed"
	StateOpen     State = "open"
	StateHalfOpen State = "half_open"
)

// ErrBreakerOpen is returned when a call is refused because the circuit is
// open.
var ErrBreakerOpen = errors.New("circuit breaker is open")

// BreakerConfig configures a circuit breaker.
type BreakerConfig struct {
	// WindowSize is the number of recent outcomes considered.
	WindowSize int
	// FailureRatio trips the breaker when exceeded within a full window.
	FailureRatio float64
	// ConsecutiveFailures trips the breaker regardless of ratio, which catches
	// a backend that has just gone away without waiting for a window to fill.
	ConsecutiveFailures int
	// MinimumRequests is the number of outcomes required before the ratio is
	// considered, so a single early failure does not trip a cold breaker.
	MinimumRequests int
	// OpenDuration is how long the breaker stays open before probing.
	OpenDuration time.Duration
	// HalfOpenProbes is how many calls are admitted while half-open.
	HalfOpenProbes int
	// Now is injectable for tests.
	Now func() time.Time
}

// DefaultBreakerConfig matches SPEC.md section 3.3.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		WindowSize:          50,
		FailureRatio:        0.5,
		ConsecutiveFailures: 10,
		MinimumRequests:     10,
		OpenDuration:        30 * time.Second,
		HalfOpenProbes:      5,
	}
}

// Breaker is a three-state circuit breaker over a sliding window of outcomes.
type Breaker struct {
	cfg  BreakerConfig
	name string

	mu           sync.Mutex
	state        State
	window       []bool // true = failure
	pos          int
	filled       int
	consecutive  int
	openedAt     time.Time
	probesIssued int
	probesOK     int
	onChange     func(name string, from, to State)
}

// NewBreaker builds a breaker. onChange is invoked on every state transition
// so the gateway can export breaker state as a metric, which is what makes
// "which backend is out" answerable without reading logs.
func NewBreaker(name string, cfg BreakerConfig, onChange func(name string, from, to State)) *Breaker {
	if cfg.WindowSize <= 0 {
		cfg = DefaultBreakerConfig()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if onChange == nil {
		onChange = func(string, State, State) {}
	}
	return &Breaker{
		cfg: cfg, name: name, state: StateClosed,
		window: make([]bool, cfg.WindowSize), onChange: onChange,
	}
}

// Name returns the breaker's identifier.
func (b *Breaker) Name() string { return b.name }

// State returns the current state, applying the open-duration timeout.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	return b.state
}

func (b *Breaker) maybeHalfOpenLocked() {
	if b.state == StateOpen && b.cfg.Now().Sub(b.openedAt) >= b.cfg.OpenDuration {
		b.transitionLocked(StateHalfOpen)
		b.probesIssued, b.probesOK = 0, 0
	}
}

func (b *Breaker) transitionLocked(to State) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	if to == StateOpen {
		b.openedAt = b.cfg.Now()
	}
	if to == StateClosed {
		b.window = make([]bool, b.cfg.WindowSize)
		b.pos, b.filled, b.consecutive = 0, 0, 0
	}
	b.onChange(b.name, from, to)
}

// Ready reports whether the breaker would admit a call, without consuming a
// half-open probe.
//
// The distinction matters. Routing filters and readiness endpoints ask about
// every backend on every request and every probe interval; if asking consumed
// the probe budget, a half-open breaker would exhaust its probes without a
// single real call being tried and would never close again. Ask with Ready;
// reserve with Allow, immediately before making the call.
func (b *Breaker) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	switch b.state {
	case StateClosed:
		return true
	case StateHalfOpen:
		return b.probesIssued < b.cfg.HalfOpenProbes
	default:
		return false
	}
}

// Allow reserves permission to make one call. When half-open it admits a
// bounded number of probes; every other caller is refused so that a recovering
// backend is not immediately buried under the traffic that broke it.
//
// Every successful Allow must be followed by a Success or a Failure, or the
// half-open probe it reserved is lost.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	switch b.state {
	case StateClosed:
		return true
	case StateHalfOpen:
		if b.probesIssued < b.cfg.HalfOpenProbes {
			b.probesIssued++
			return true
		}
		return false
	default:
		return false
	}
}

// Success records a successful call.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	switch b.state {
	case StateHalfOpen:
		b.probesOK++
		if b.probesOK >= b.cfg.HalfOpenProbes {
			b.transitionLocked(StateClosed)
		}
	default:
		b.recordLocked(false)
	}
}

// Failure records a failed call.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	switch b.state {
	case StateHalfOpen:
		// One failure during recovery is enough: the backend is not ready.
		b.transitionLocked(StateOpen)
	default:
		b.recordLocked(true)
		if b.consecutive >= b.cfg.ConsecutiveFailures {
			b.transitionLocked(StateOpen)
			return
		}
		if b.filled >= b.cfg.MinimumRequests && b.failureRatioLocked() >= b.cfg.FailureRatio {
			b.transitionLocked(StateOpen)
		}
	}
}

func (b *Breaker) recordLocked(failure bool) {
	b.window[b.pos] = failure
	b.pos = (b.pos + 1) % len(b.window)
	if b.filled < len(b.window) {
		b.filled++
	}
}

func (b *Breaker) failureRatioLocked() float64 {
	if b.filled == 0 {
		return 0
	}
	failures := 0
	for i := 0; i < b.filled; i++ {
		if b.window[i] {
			failures++
		}
	}
	return float64(failures) / float64(b.filled)
}

// Trip forces the breaker open, used by an operator draining a backend.
func (b *Breaker) Trip() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionLocked(StateOpen)
}

// Reset forces the breaker closed, used after a confirmed fix.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transitionLocked(StateClosed)
}

// Snapshot reports breaker state for diagnostics.
type Snapshot struct {
	Name        string  `json:"name"`
	State       State   `json:"state"`
	FailureRate float64 `json:"failure_rate"`
	Observed    int     `json:"observed"`
	Consecutive int     `json:"consecutive_failures"`
	OpenedAt    string  `json:"opened_at,omitempty"`
}

// Snapshot returns the breaker's current view.
func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpenLocked()
	s := Snapshot{
		Name: b.name, State: b.state, FailureRate: b.failureRatioLocked(),
		Observed: b.filled, Consecutive: b.consecutive,
	}
	if b.state == StateOpen {
		s.OpenedAt = b.openedAt.UTC().Format(time.RFC3339)
	}
	return s
}
