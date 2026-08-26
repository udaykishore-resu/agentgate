// Package gateway implements the traffic plane: the provider-agnostic API,
// the policy chain every request passes through, and the routing, resilience
// and metering that sit behind it.
package gateway

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentgate/agentgate/internal/cache"
	"github.com/agentgate/agentgate/internal/config"
	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/provider"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/resilience"
)

// Backend is one runtime backend: the adapter, its routing weights, its
// breaker and its live concurrency.
type Backend struct {
	Name              string
	Provider          provider.Provider
	Weight            int
	Priority          int
	MaxConcurrent     int64
	MaxClassification registry.DataClassification
	Residency         string
	Timeout           time.Duration
	Pricing           string

	breaker  *resilience.Breaker
	inflight atomic.Int64
	enabled  atomic.Bool
	// drained marks a backend an operator has taken out of rotation without
	// restarting the gateway. Draining rather than deleting means the change
	// is reversible in one call during an incident.
	drained atomic.Bool

	successes atomic.Int64
	failures  atomic.Int64
	lastError atomic.Value // string
}

// Available reports whether the backend may currently be selected. It does not
// reserve a half-open probe; invoke does that immediately before it calls.
func (b *Backend) Available() bool {
	if !b.enabled.Load() || b.drained.Load() {
		return false
	}
	if b.MaxConcurrent > 0 && b.inflight.Load() >= b.MaxConcurrent {
		return false
	}
	return b.breaker.Ready()
}

// Breaker exposes the backend's circuit breaker.
func (b *Backend) Breaker() *resilience.Breaker { return b.breaker }

// Inflight reports current in-flight requests to this backend.
func (b *Backend) Inflight() int64 { return b.inflight.Load() }

// Drain takes the backend out of rotation.
func (b *Backend) Drain() { b.drained.Store(true) }

// Undrain returns the backend to rotation.
func (b *Backend) Undrain() { b.drained.Store(false) }

// Drained reports whether the backend is out of rotation.
func (b *Backend) Drained() bool { return b.drained.Load() }

// AcceptsClassification reports whether a request of the given classification
// may be routed here. This is the routing-time enforcement of data residency
// and third-party handling rules.
func (b *Backend) AcceptsClassification(c registry.DataClassification) bool {
	if c == "" {
		return true
	}
	return c.Rank() <= b.MaxClassification.Rank()
}

// Status is a backend's health for the operator API.
type Status struct {
	Name       string              `json:"name"`
	Provider   string              `json:"provider"`
	Model      string              `json:"model"`
	Priority   int                 `json:"priority"`
	Weight     int                 `json:"weight"`
	Breaker    resilience.Snapshot `json:"breaker"`
	Inflight   int64               `json:"inflight"`
	Successes  int64               `json:"successes"`
	Failures   int64               `json:"failures"`
	Drained    bool                `json:"drained"`
	LastError  string              `json:"last_error,omitempty"`
	MaxClassif string              `json:"max_classification"`
	Residency  string              `json:"residency,omitempty"`
}

// Status renders the backend's current state.
func (b *Backend) Status() Status {
	last, _ := b.lastError.Load().(string)
	return Status{
		Name: b.Name, Provider: string(b.Provider.Kind()), Model: b.Provider.Model(),
		Priority: b.Priority, Weight: b.Weight, Breaker: b.breaker.Snapshot(),
		Inflight: b.inflight.Load(), Successes: b.successes.Load(), Failures: b.failures.Load(),
		Drained: b.drained.Load(), LastError: last,
		MaxClassif: string(b.MaxClassification), Residency: b.Residency,
	}
}

// Pool is a set of interchangeable backends behind one logical model.
type Pool struct {
	Name              string
	Strategy          string
	Tier              resilience.Priority
	Backends          []*Backend
	Cache             cache.Policy
	Guardrail         guardrails.Policy
	GuardrailProvider guardrails.Provider
	ScanOutput        bool
	Retry             resilience.RetryConfig

	rrCursor atomic.Uint64
	rndMu    sync.Mutex
	rnd      *rand.Rand
}

// Select returns the ordered list of backends to try for a request.
//
// Selection is ordered, not single-choice, because failover needs to know what
// comes next without re-running the whole decision. The order is: filter to
// available backends that accept the request's data classification, group by
// priority tier, and within the lowest surviving tier order by the pool's
// strategy. Higher tiers follow in order, so an exhausted primary tier falls
// through to the on-premises tier rather than failing.
func (p *Pool) Select(classification registry.DataClassification, includeUnavailable bool) []*Backend {
	eligible := make([]*Backend, 0, len(p.Backends))
	for _, b := range p.Backends {
		if !b.AcceptsClassification(classification) {
			continue
		}
		if !includeUnavailable && !b.Available() {
			continue
		}
		eligible = append(eligible, b)
	}
	if len(eligible) == 0 {
		return nil
	}

	tiers := map[int][]*Backend{}
	for _, b := range eligible {
		tiers[b.Priority] = append(tiers[b.Priority], b)
	}
	priorities := make([]int, 0, len(tiers))
	for pr := range tiers {
		priorities = append(priorities, pr)
	}
	sort.Ints(priorities)

	out := make([]*Backend, 0, len(eligible))
	for _, pr := range priorities {
		out = append(out, p.order(tiers[pr])...)
	}
	return out
}

func (p *Pool) order(bs []*Backend) []*Backend {
	if len(bs) <= 1 {
		return bs
	}
	switch p.Strategy {
	case "priority":
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].Name < bs[j].Name })
		return bs
	case "round_robin":
		n := int(p.rrCursor.Add(1) % uint64(len(bs)))
		return append(append([]*Backend{}, bs[n:]...), bs[:n]...)
	case "least_loaded":
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].inflight.Load() < bs[j].inflight.Load() })
		return bs
	case "weighted":
		return p.weighted(bs, false)
	default: // weighted_least_loaded
		return p.weighted(bs, true)
	}
}

// weighted picks a first backend by weighted random choice and orders the rest
// behind it. When loadAware is set the weights are divided by one plus the
// backend's in-flight count, which pushes traffic away from a backend that has
// started to queue without abandoning the configured split.
func (p *Pool) weighted(bs []*Backend, loadAware bool) []*Backend {
	scores := make([]float64, len(bs))
	total := 0.0
	for i, b := range bs {
		w := float64(b.Weight)
		if w <= 0 {
			w = 1
		}
		if loadAware {
			w /= 1 + float64(b.inflight.Load())
		}
		scores[i] = w
		total += w
	}
	remaining := make([]*Backend, len(bs))
	copy(remaining, bs)
	out := make([]*Backend, 0, len(bs))
	for len(remaining) > 0 {
		p.rndMu.Lock()
		r := p.rnd.Float64() * total
		p.rndMu.Unlock()
		idx := len(remaining) - 1
		acc := 0.0
		for i := range remaining {
			acc += scores[i]
			if r <= acc {
				idx = i
				break
			}
		}
		out = append(out, remaining[idx])
		total -= scores[idx]
		remaining = append(remaining[:idx], remaining[idx+1:]...)
		scores = append(scores[:idx], scores[idx+1:]...)
	}
	return out
}

// Router resolves a logical model to a pool and holds the runtime topology.
type Router struct {
	mu     sync.RWMutex
	pools  map[string]*Pool
	models map[string]config.Model
	order  []string
}

// NewRouter builds the routing topology from configuration.
func NewRouter(cfg *config.Config, build BackendFactory) (*Router, error) {
	r := &Router{pools: map[string]*Pool{}, models: map[string]config.Model{}}
	for i := range cfg.Pools {
		pc := &cfg.Pools[i]
		pool := &Pool{
			Name:     pc.Name,
			Strategy: pc.Strategy,
			Tier:     resilience.ParsePriority(pc.Tier),
			Cache: cache.Policy{
				Enabled: pc.Cache.Enabled, TTL: pc.Cache.TTL.Or(10 * time.Minute),
				MaxTemperature: pc.Cache.MaxTemperature, AllowTools: pc.Cache.AllowTools,
				Semantic: pc.Cache.Semantic, SemanticThreshold: pc.Cache.SemanticThreshold,
				SemanticCandidates: pc.Cache.SemanticCandidates,
			},
			ScanOutput: pc.Guardrail.ScanOutput,
			rnd:        rand.New(rand.NewSource(time.Now().UnixNano() + int64(i))),
		}
		pool.Retry = retryFrom(cfg.Gateway.Retry, pc.Retry)
		pool.Guardrail = guardrailPolicyFrom(pc.Guardrail)

		for j := range pc.Backends {
			bc := pc.Backends[j]
			if !bc.IsEnabled() {
				continue
			}
			p, err := build(bc)
			if err != nil {
				return nil, fmt.Errorf("pool %s backend %s: %w", pc.Name, bc.Name, err)
			}
			b := &Backend{
				Name: bc.Name, Provider: p, Weight: bc.Weight, Priority: bc.Priority,
				MaxConcurrent: int64(bc.MaxConcurrent), Timeout: bc.Timeout.Or(60 * time.Second),
				MaxClassification: registry.DataClassification(bc.MaxClassification),
				Residency:         bc.Residency,
			}
			b.enabled.Store(true)
			b.breaker = resilience.NewBreaker(bc.Name, breakerFrom(cfg.Gateway.Breaker, pc.Breaker), nil)
			pool.Backends = append(pool.Backends, b)
		}
		r.pools[pc.Name] = pool
		r.order = append(r.order, pc.Name)
	}
	for _, m := range cfg.Models {
		r.models[m.Name] = m
	}
	return r, nil
}

// BackendFactory builds a provider adapter from configuration.
type BackendFactory func(config.Backend) (provider.Provider, error)

// Resolve maps a logical model name to its model definition and pool.
func (r *Router) Resolve(model string) (config.Model, *Pool, *httpx.Problem) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[model]
	if !ok {
		return config.Model{}, nil, httpx.Errorf(httpx.CodeUnknownModel,
			"unknown model %q; call GET /v1/models for the models this agent may use", model)
	}
	p, ok := r.pools[m.Pool]
	if !ok {
		return config.Model{}, nil, httpx.Errorf(httpx.CodeInternal,
			"model %q references pool %q which is not configured", model, m.Pool)
	}
	return m, p, nil
}

// Pool returns a pool by name.
func (r *Router) Pool(name string) (*Pool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.pools[name]
	return p, ok
}

// Pools returns every pool, in configuration order.
func (r *Router) Pools() []*Pool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Pool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.pools[n])
	}
	return out
}

// Models returns the logical models an agent is entitled to, given its pool
// entitlement. A caller never sees a model it could not use, which removes an
// entire class of support question.
func (r *Router) Models(entitled func(pool string) bool) []provider.Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.Model, 0, len(r.models))
	for name, m := range r.models {
		if entitled != nil && !entitled(m.Pool) {
			continue
		}
		caps := m.Capabilities
		if len(caps) == 0 {
			caps = capabilitiesOf(r.pools[m.Pool])
		}
		out = append(out, provider.Model{
			ID: name, Object: "model", OwnedBy: "agentgate", Pool: m.Pool,
			ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput, Capabilities: caps,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func capabilitiesOf(p *Pool) []string {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range p.Backends {
		for _, c := range b.Provider.Capabilities() {
			if !seen[string(c)] {
				seen[string(c)] = true
				out = append(out, string(c))
			}
		}
	}
	sort.Strings(out)
	return out
}

// Health probes every backend, used by the readiness endpoint.
func (r *Router) Health(ctx context.Context) map[string]string {
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range r.Pools() {
		for _, b := range p.Backends {
			wg.Add(1)
			go func(pool string, b *Backend) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				status := "ok"
				if err := b.Provider.Health(ctx); err != nil {
					status = "error: " + truncate(err.Error(), 120)
				}
				mu.Lock()
				out[pool+"/"+b.Name] = status
				mu.Unlock()
			}(p.Name, b)
		}
	}
	wg.Wait()
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func retryFrom(base config.Retry, override *config.Retry) resilience.RetryConfig {
	r := base
	if override != nil {
		if override.MaxAttempts > 0 {
			r.MaxAttempts = override.MaxAttempts
		}
		if override.BaseDelay > 0 {
			r.BaseDelay = override.BaseDelay
		}
		if override.MaxDelay > 0 {
			r.MaxDelay = override.MaxDelay
		}
		if override.BudgetRatio > 0 {
			r.BudgetRatio = override.BudgetRatio
		}
	}
	return resilience.RetryConfig{
		MaxAttempts: r.MaxAttempts,
		BaseDelay:   r.BaseDelay.Or(100 * time.Millisecond),
		MaxDelay:    r.MaxDelay.Or(2 * time.Second),
		BudgetRatio: r.BudgetRatio,
	}
}

func breakerFrom(base config.Breaker, override *config.Breaker) resilience.BreakerConfig {
	b := base
	if override != nil {
		if override.WindowSize > 0 {
			b.WindowSize = override.WindowSize
		}
		if override.FailureRatio > 0 {
			b.FailureRatio = override.FailureRatio
		}
		if override.ConsecutiveFailures > 0 {
			b.ConsecutiveFailures = override.ConsecutiveFailures
		}
		if override.MinimumRequests > 0 {
			b.MinimumRequests = override.MinimumRequests
		}
		if override.OpenDuration > 0 {
			b.OpenDuration = override.OpenDuration
		}
		if override.HalfOpenProbes > 0 {
			b.HalfOpenProbes = override.HalfOpenProbes
		}
	}
	return resilience.BreakerConfig{
		WindowSize: b.WindowSize, FailureRatio: b.FailureRatio,
		ConsecutiveFailures: b.ConsecutiveFailures, MinimumRequests: b.MinimumRequests,
		OpenDuration: b.OpenDuration.Or(30 * time.Second), HalfOpenProbes: b.HalfOpenProbes,
	}
}

func guardrailPolicyFrom(c config.GuardrailPolicy) guardrails.Policy {
	p := guardrails.Policy{
		Enabled:            c.Enabled,
		FailureMode:        guardrails.FailureMode(c.FailureMode),
		OutputWindowTokens: c.OutputWindowTokens,
		Timeout:            c.Timeout.Or(2 * time.Second),
		Categories:         map[string]guardrails.CategoryPolicy{},
	}
	if len(c.Categories) == 0 {
		p.Categories = guardrails.DefaultPolicy("").Categories
		return p
	}
	for name, cat := range c.Categories {
		p.Categories[name] = guardrails.CategoryPolicy{
			Action:    guardrails.Action(strings.ToLower(cat.Action)),
			Threshold: cat.Threshold,
		}
	}
	return p
}
