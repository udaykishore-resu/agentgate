package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentgate/agentgate/internal/cache"
	"github.com/agentgate/agentgate/internal/config"
	"github.com/agentgate/agentgate/internal/cost"
	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/provider"
	"github.com/agentgate/agentgate/internal/ratelimit"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/resilience"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/agentgate/agentgate/internal/version"
	"github.com/google/uuid"
)

// Options assembles a Server. Every dependency is injected rather than
// constructed inside, so the integration tests run the real server against
// fake backends instead of a parallel implementation that can drift.
type Options struct {
	Config     *config.Config
	Logger     *slog.Logger
	Telemetry  *telemetry.Provider
	Router     *Router
	Verifier   *identity.Verifier
	Limiter    ratelimit.Limiter
	Cache      *cache.Memory
	Prices     *cost.Book
	Usage      cost.Sink
	Quotas     QuotaResolver
	Guardrails map[string]guardrails.Provider // by pool name
}

// QuotaResolver returns the limits for an agent. The gateway does not call the
// control plane on the request path for this: the resolver caches, and a stale
// quota is a far better outcome than a request that fails because the control
// plane is down.
type QuotaResolver interface {
	Limits(ctx context.Context, claims *identity.Claims) ratelimit.Limits
}

// StaticQuota resolves every agent to the same envelope.
type StaticQuota struct{ Default ratelimit.Limits }

// Limits returns the configured default.
func (s StaticQuota) Limits(context.Context, *identity.Claims) ratelimit.Limits { return s.Default }

// Server is the gateway HTTP server.
type Server struct {
	cfg     *config.Config
	env     string
	log     *slog.Logger
	tp      *telemetry.Provider
	tracer  *telemetry.Tracer
	metrics *telemetry.Instruments

	router   *Router
	verifier *identity.Verifier
	limiter  ratelimit.Limiter
	cache    *cache.Memory
	prices   *cost.Book
	usage    cost.Sink
	quotas   QuotaResolver

	concurrency *resilience.ConcurrencyLimiter
	retryBudget *resilience.RetryBudget
	chain       []Stage

	idem   *idempotencyStore
	mux    *http.ServeMux
	ready  chan struct{}
	closed chan struct{}

	startedAt time.Time
}

// New builds the gateway server and wires the policy chain.
func New(o Options) (*Server, error) {
	if o.Config == nil {
		return nil, fmt.Errorf("gateway: config is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Quotas == nil {
		o.Quotas = StaticQuota{Default: ratelimit.Limits{
			RequestsPerMinute:  o.Config.Gateway.Quota.RequestsPerMinute,
			TokensPerMinute:    o.Config.Gateway.Quota.TokensPerMinute,
			MonthlyTokenBudget: o.Config.Gateway.Quota.MonthlyTokenBudget,
		}}
	}
	s := &Server{
		cfg: o.Config, env: o.Config.Env, log: o.Logger, tp: o.Telemetry,
		router: o.Router, verifier: o.Verifier, limiter: o.Limiter,
		cache: o.Cache, prices: o.Prices, usage: o.Usage, quotas: o.Quotas,
		concurrency: resilience.NewConcurrencyLimiter(o.Config.Gateway.MaxConcurrent, o.Config.Gateway.BatchReserveRatio),
		retryBudget: resilience.NewRetryBudget(o.Config.Gateway.Retry.FleetBudgetRatio, o.Config.Gateway.Retry.FleetWindow.Or(10*time.Second)),
		idem:        newIdempotencyStore(o.Config.Gateway.IdempotencyTTL.Or(24 * time.Hour)),
		ready:       make(chan struct{}),
		closed:      make(chan struct{}),
		startedAt:   time.Now(),
	}
	s.tracer = o.Telemetry.Tracer("agentgate/gateway")
	s.metrics = telemetry.NewInstruments(o.Telemetry.Metrics)

	for _, p := range s.router.Pools() {
		if gp, ok := o.Guardrails[p.Name]; ok {
			p.GuardrailProvider = gp
		} else {
			p.GuardrailProvider = guardrails.Noop{}
		}
		mode := string(p.Guardrail.FailureMode)
		s.metrics.GuardrailMode.Set(1, s.env, p.Name, mode)
		for _, b := range p.Backends {
			s.exportBreaker(p.Name, b)
			b.Breaker().Reset()
		}
	}
	s.chain = s.buildChain()
	s.mux = s.routes()
	go s.observe()
	return s, nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

// MarkReady signals that dependencies are up and the gateway may take traffic.
func (s *Server) MarkReady() {
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
}

// Close stops background work.
func (s *Server) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", s.authenticated(s.handleChat))
	mux.Handle("POST /v1/embeddings", s.authenticated(s.handleEmbeddings))
	mux.Handle("POST /v1/token-count", s.authenticated(s.handleTokenCount))
	mux.Handle("GET /v1/models", s.authenticated(s.handleModels))
	mux.Handle("GET /v1/models/{id}", s.authenticated(s.handleModel))

	// Operator surface. Network-restricted rather than token-authenticated:
	// these must work when the control plane is the thing that is broken.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /admin/backends", s.handleBackends)
	mux.HandleFunc("POST /admin/backends/{pool}/{backend}/drain", s.handleDrain(true))
	mux.HandleFunc("POST /admin/backends/{pool}/{backend}/undrain", s.handleDrain(false))
	mux.HandleFunc("POST /admin/cache/purge", s.handleCachePurge)
	return mux
}

// authenticated wraps a handler with the full request lifecycle: trace, span,
// load shedding, token verification, panic recovery and metering.
func (s *Server) authenticated(h func(context.Context, *RequestContext) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := telemetry.ExtractHTTP(r.Context(), r.Header)
		ctx, span := s.tracer.Start(ctx, telemetry.SpanGatewayRequest,
			telemetry.WithSpanKind(telemetry.KindServer),
			telemetry.WithAttributes(
				telemetry.Attr("http.request.method", r.Method),
				telemetry.Attr("url.path", r.URL.Path),
			))
		defer span.End()

		rc := &RequestContext{
			Start: time.Now(), W: w, R: r, Span: span,
			RequestID: requestID(r), TraceID: span.SpanContext().TraceID.String(),
			Priority:  resilience.ParsePriority(r.Header.Get(HeaderPriority)),
			CacheMode: strings.ToLower(r.Header.Get(HeaderCache)),
		}
		span.SetAttributes(
			telemetry.Attr(telemetry.AttrRequestID, rc.RequestID),
			telemetry.Attr(telemetry.AttrPriority, string(rc.Priority)),
		)

		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic in request handler",
					"panic", p, "request_id", rc.RequestID, "path", r.URL.Path)
				span.SetStatus(telemetry.StatusError, "panic")
				rc.Problem = httpx.NewProblem(httpx.CodeInternal, "the gateway encountered an unexpected error")
				if !rc.streamStarted() {
					httpx.WriteProblem(w, rc.Problem, rc.RequestID, rc.TraceID)
				}
				s.finish(ctx, rc)
			}
		}()

		release, admitted := s.concurrency.Acquire(ctx, rc.Priority)
		if !admitted {
			s.metrics.Shed.Inc(s.env, "-", string(rc.Priority))
			s.metrics.RateLimit.Inc(s.env, "unknown", "unknown", "unknown", string(ratelimit.DecisionShed))
			rc.Problem = httpx.Errorf(httpx.CodeRateLimited,
				"the gateway is shedding %s traffic to protect interactive requests", rc.Priority).WithRetryAfter(2)
			rc.WriteHeaders()
			httpx.WriteProblem(w, rc.Problem, rc.RequestID, rc.TraceID)
			s.finish(ctx, rc)
			return
		}
		defer release()

		if err := h(ctx, rc); err != nil {
			rc.Problem = httpx.AsProblem(err)
			span.SetAttributes(telemetry.Attr(telemetry.AttrErrorCode, string(rc.Problem.Code)))
			if rc.Problem.Status >= 500 {
				span.SetStatus(telemetry.StatusError, string(rc.Problem.Code))
			}
			if rc.streamStarted() {
				// Headers are long gone. The contract says a stream that ends
				// without [DONE] failed, and the error frame says why.
				s.log.Warn("stream failed after first byte",
					"request_id", rc.RequestID, "code", rc.Problem.Code, "detail", rc.Problem.Detail)
			} else {
				rc.WriteHeaders()
				httpx.WriteProblem(w, rc.Problem, rc.RequestID, rc.TraceID)
			}
		}
		s.finish(ctx, rc)
	})
}

func requestID(r *http.Request) string {
	if v := r.Header.Get(HeaderRequestID); v != "" && len(v) <= 128 {
		return v
	}
	return "req_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// finish records metrics, the usage record and the final span attributes. It
// runs on every path, including panics and shed requests, because a request
// that is not metered is a request that did not happen as far as the fleet
// view is concerned.
func (s *Server) finish(ctx context.Context, rc *RequestContext) {
	if rc.Reservation != nil {
		actual := int64(rc.InputTokens() + rc.OutputTokens())
		if err := rc.Reservation.Settle(ctx, actual); err != nil {
			s.log.Warn("could not settle token reservation",
				"request_id", rc.RequestID, "error", err)
		}
	}
	rc.releaseStream()

	status := http.StatusOK
	code := ""
	if rc.Problem != nil {
		status, code = rc.Problem.Status, string(rc.Problem.Code)
	}
	env, tenant, team, agent := rc.TenantLabels()
	attributionFixed := "false"
	if rc.AttributionCorrected {
		attributionFixed = "true"
	}
	cacheLabel := string(rc.CacheResult)
	if cacheLabel == "" {
		cacheLabel = "bypass"
	}

	s.metrics.Requests.Inc(env, tenant, team, agent, rc.PoolName(), rc.BackendName(),
		fmt.Sprint(status), code, rc.StreamLabel(), cacheLabel, attributionFixed)

	total := time.Since(rc.Start)
	s.metrics.Duration.Observe(total.Seconds(), env, rc.PoolName(), rc.BackendName(), rc.StreamLabel())
	s.metrics.Overhead.Observe(rc.GatewayOverhead().Seconds(), env, rc.PoolName(), rc.StreamLabel())
	if rc.TTFT > 0 {
		s.metrics.TTFT.Observe(rc.TTFT.Seconds(), env, rc.PoolName(), rc.BackendName())
	}
	if in := rc.InputTokens(); in > 0 {
		s.metrics.Tokens.Add(float64(in), env, tenant, team, agent, rc.BackendName(), "input")
	}
	if out := rc.OutputTokens(); out > 0 {
		s.metrics.Tokens.Add(float64(out), env, tenant, team, agent, rc.BackendName(), "output")
	}
	if rc.CostUSD > 0 {
		s.metrics.CostUSD.Add(rc.CostUSD, env, tenant, team, agent, rc.CostCenter(), rc.BackendName())
	}
	if rc.SavingsUSD > 0 {
		s.metrics.CacheSavings.Add(rc.SavingsUSD, env, tenant, team, rc.CostCenter())
	}

	if rc.Span != nil {
		rc.Span.SetAttributes(
			telemetry.Attr("http.response.status_code", status),
			telemetry.Attr(telemetry.AttrCostUSD, rc.CostUSD),
			telemetry.Attr(telemetry.AttrCache, cacheLabel),
			telemetry.Attr(telemetry.AttrAttempt, rc.Attempts),
			telemetry.Attr("agentgate.duration_ms", float64(total.Microseconds())/1000),
			telemetry.Attr("agentgate.overhead_ms", float64(rc.GatewayOverhead().Microseconds())/1000),
		)
		if rc.TTFT > 0 {
			rc.Span.SetAttributes(telemetry.Attr(telemetry.AttrTTFTMillis, float64(rc.TTFT.Microseconds())/1000))
		}
	}

	if s.usage != nil && rc.Claims != nil && (rc.InputTokens() > 0 || rc.OutputTokens() > 0) {
		s.usage.Write(ctx, s.usageRecord(rc, status, code, total))
	}
}

func (s *Server) usageRecord(rc *RequestContext, status int, code string, total time.Duration) cost.Record {
	rec := cost.Record{
		Timestamp: rc.Start.UTC(), RequestID: rc.RequestID, TraceID: rc.TraceID,
		Tenant: rc.Claims.Tenant, Team: rc.Claims.Team, AgentID: rc.Claims.AgentID,
		AgentIdentity: rc.Claims.Subject, AgentVersion: rc.Claims.AgentVersion,
		Env: string(rc.Claims.Env), CostCenter: rc.CostCenter(),
		LogicalModel: rc.Model.Name, Pool: rc.PoolName(),
		Operation:   string(rc.Operation),
		InputTokens: rc.InputTokens(), OutputTokens: rc.OutputTokens(),
		Cache: string(rc.CacheResult), Attempts: rc.Attempts,
		StatusCode: status, ErrorCode: code,
		DurationMS: float64(total.Microseconds()) / 1000,
	}
	// A record is billable only when a provider actually answered and reported
	// what it used. Everything else — a cache hit, a refusal, a failed attempt
	// — is recorded with a token count the gateway guessed, marked as an
	// estimate, and charged to nobody. Presenting a guess as a measurement is
	// how a chargeback report loses the argument it exists to win.
	measured := rc.Usage != nil && rc.Usage.PromptTokens > 0 && !rc.UsageEstimated
	served := status < 400 && rc.Backend != nil
	rec.Estimated = !measured
	rec.Billable = measured && served &&
		rc.CacheResult != cache.ResultHit && rc.CacheResult != cache.ResultSemantic
	if rc.Usage != nil {
		rec.CachedTokens = rc.Usage.CachedTokens
	}
	if rc.TTFT > 0 {
		rec.TTFTMS = float64(rc.TTFT.Microseconds()) / 1000
	}
	if rc.Backend != nil {
		rec.Provider = string(rc.Backend.Provider.Kind())
		rec.BackendModel = rc.Backend.Name
	} else if rc.CacheEntry != nil {
		rec.Provider = "cache"
		rec.BackendModel = rc.CacheEntry.Backend
	}
	cost.Price(s.prices, &rec)
	rc.CostUSD, rc.SavingsUSD = rec.CostUSD, rec.SavingsUSD
	return rec
}

// exportBreaker wires a backend's breaker state changes to the metric that the
// on-call engineer's dashboard reads.
func (s *Server) exportBreaker(pool string, b *Backend) {
	set := func(state resilience.State) {
		for _, st := range []resilience.State{resilience.StateClosed, resilience.StateOpen, resilience.StateHalfOpen} {
			v := 0.0
			if st == state {
				v = 1
			}
			s.metrics.Breaker.Set(v, s.env, pool, b.Name, string(st))
		}
	}
	set(resilience.StateClosed)
	b.breaker = resilience.NewBreaker(b.Name, breakerFrom(s.cfg.Gateway.Breaker, nil), func(name string, from, to resilience.State) {
		set(to)
		s.log.Warn("circuit breaker state change",
			"backend", name, "pool", pool, "from", from, "to", to)
	})
}

// observe publishes gauges that are not naturally event-driven.
func (s *Server) observe() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	var lastSent, lastFailed, lastDropped int64
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			for _, p := range s.router.Pools() {
				inflight := int64(0)
				for _, b := range p.Backends {
					inflight += b.Inflight()
				}
				s.metrics.Inflight.Set(float64(inflight), s.env, p.Name)
			}
			// The telemetry pipeline reports on itself. A plane that cannot
			// say how many of its own spans it dropped is a plane nobody
			// should believe, so these are exported as deltas against the
			// last observation rather than left as internal counters.
			sent, failed, dropped := s.tp.Stats()
			s.metrics.SpansExported.Add(float64(sent-lastSent), s.env, "ok")
			s.metrics.SpansExported.Add(float64(failed-lastFailed), s.env, "failed")
			s.metrics.SpansExported.Add(float64(dropped-lastDropped), s.env, "dropped")
			lastSent, lastFailed, lastDropped = sent, failed, dropped
		}
	}
}

// handleHealth reports liveness. It must not depend on any downstream, or a
// provider outage would cause the orchestrator to restart healthy pods.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": s.cfg.Service.Name, "env": s.env,
		"version": version.Get(), "uptime_seconds": int(time.Since(s.startedAt).Seconds()),
	})
}

// handleReady reports readiness, which does depend on the things a request
// needs.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	select {
	case <-s.ready:
	default:
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]any{}
	healthyBackends := 0
	for _, p := range s.router.Pools() {
		for _, b := range p.Backends {
			if b.Available() {
				healthyBackends++
			}
		}
	}
	checks["backends_available"] = healthyBackends
	checks["rate_limiter"] = s.limiter.Healthy(ctx)
	if fb, ok := s.limiter.(*ratelimit.Fallback); ok {
		checks["rate_limiter_degraded"] = fb.Degraded()
	}
	checks["concurrency_saturation"] = s.concurrency.Saturation()
	checks["retry_ratio"] = s.retryBudget.Ratio()

	status := http.StatusOK
	if healthyBackends == 0 {
		status = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, status, map[string]any{"status": statusWord(status), "checks": checks})
}

func statusWord(code int) string {
	if code == http.StatusOK {
		return "ready"
	}
	return "not_ready"
}

// handleBackends exposes routing state for the operator.
func (s *Server) handleBackends(w http.ResponseWriter, _ *http.Request) {
	out := map[string][]Status{}
	for _, p := range s.router.Pools() {
		for _, b := range p.Backends {
			out[p.Name] = append(out[p.Name], b.Status())
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// handleDrain takes a backend in or out of rotation without a restart.
func (s *Server) handleDrain(drain bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool, ok := s.router.Pool(r.PathValue("pool"))
		if !ok {
			httpx.WriteProblem(w, httpx.Errorf(httpx.CodeNotFound, "unknown pool"), "", "")
			return
		}
		name := r.PathValue("backend")
		for _, b := range pool.Backends {
			if b.Name != name {
				continue
			}
			if drain {
				b.Drain()
			} else {
				b.Undrain()
			}
			s.log.Warn("backend rotation changed", "pool", pool.Name, "backend", name, "drained", drain)
			httpx.WriteJSON(w, http.StatusOK, b.Status())
			return
		}
		httpx.WriteProblem(w, httpx.Errorf(httpx.CodeNotFound, "unknown backend"), "", "")
	}
}

// handleCachePurge drops cached responses, the emergency control when a cached
// answer is suspected to be wrong or unsafe.
func (s *Server) handleCachePurge(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	n := s.cache.Purge(r.Context(), tenant)
	s.log.Warn("cache purged", "tenant", tenant, "entries", n)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"purged": n, "tenant": tenant})
}

// idempotencyStore de-duplicates retried non-streaming requests.
type idempotencyStore struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]idempotencyEntry
}

type idempotencyEntry struct {
	bodyHash string
	response []byte
	status   int
	stored   time.Time
}

func newIdempotencyStore(ttl time.Duration) *idempotencyStore {
	return &idempotencyStore{ttl: ttl, m: map[string]idempotencyEntry{}}
}

func (s *idempotencyStore) get(key, bodyHash string) (idempotencyEntry, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key]
	if !ok || time.Since(e.stored) > s.ttl {
		return idempotencyEntry{}, false, false
	}
	if e.bodyHash != bodyHash {
		return idempotencyEntry{}, false, true // conflict
	}
	return e, true, false
}

func (s *idempotencyStore) put(key, bodyHash string, status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) > 100000 {
		// Bounded rather than unbounded: a memory leak in the idempotency
		// cache would be a slow outage, and dropping the oldest keys only
		// costs a duplicate call.
		for k, v := range s.m {
			if time.Since(v.stored) > s.ttl/2 {
				delete(s.m, k)
			}
		}
	}
	s.m[key] = idempotencyEntry{bodyHash: bodyHash, status: status, response: body, stored: time.Now()}
}

// decodeJSON reads a request body under a size cap and decodes it, returning
// the raw bytes as well so the idempotency hash and the audit record are taken
// from exactly what the caller sent.
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, v any) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		return nil, httpx.Errorf(httpx.CodeInvalidRequest,
			"request body could not be read or exceeds the %d byte limit", maxBytes)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return raw, httpx.Errorf(httpx.CodeInvalidRequest, "request body is not valid JSON: %v", err)
	}
	return raw, nil
}

// providerModelsFor is used by the models endpoint.
func (s *Server) providerModelsFor(claims *identity.Claims) []provider.Model {
	return s.router.Models(func(pool string) bool { return claims.MayUsePool(pool) })
}

// classificationFor resolves the data classification the request must be
// routed under.
func classificationFor(claims *identity.Claims) registry.DataClassification {
	if claims == nil || claims.DataClassification == "" {
		return registry.ClassConfidential
	}
	return registry.DataClassification(claims.DataClassification)
}
