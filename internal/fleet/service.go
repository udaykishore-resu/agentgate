package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentgate/agentgate/internal/cost"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/agentgate/agentgate/internal/version"
)

// AgentView is one row of the fleet view: who owns an agent, what version is
// where, whether it is healthy, what it consumes and who pays for it.
type AgentView struct {
	AgentID        string             `json:"agent_id"`
	Identity       string             `json:"identity"`
	DisplayName    string             `json:"display_name"`
	Team           string             `json:"team"`
	Tenant         string             `json:"tenant"`
	Owner          registry.Owner     `json:"owner"`
	Runtime        string             `json:"runtime"`
	Framework      string             `json:"framework,omitempty"`
	Classification string             `json:"data_classification"`
	Versions       []registry.Version `json:"versions"`
	ProdVersion    string             `json:"prod_version,omitempty"`
	Pools          []string           `json:"pools"`

	RequestsLastHour int64   `json:"requests_last_hour"`
	TokensLastHour   int64   `json:"tokens_last_hour"`
	SpendLast24h     float64 `json:"spend_usd_last_24h"`
	SpendMonthToDate float64 `json:"spend_usd_month_to_date"`
	ErrorRate        float64 `json:"error_rate"`
	P95LatencySec    float64 `json:"p95_latency_seconds"`

	Telemetry TrustStats `json:"telemetry"`
	Health    string     `json:"health"`
	Issues    []string   `json:"issues,omitempty"`
}

// Options configures the fleet service.
type Options struct {
	Env        string
	Store      registry.Store
	Prom       *PromClient
	Trust      *TrustTracker
	Aggregator *cost.Aggregator
	Anomaly    *cost.AnomalyDetector
	Metrics    *telemetry.Instruments
	Logger     *slog.Logger
	// UsageDir is where the file usage sink writes, read for the chargeback
	// export when no warehouse is wired up.
	UsageDir        string
	SLOWindow       time.Duration
	MinCompleteness float64
}

// Service is the observability plane's API.
type Service struct {
	opts Options
	mux  *http.ServeMux

	mu       sync.RWMutex
	cached   []AgentView
	cachedAt time.Time
	sloCache []Status
	sloAt    time.Time
}

// New builds the fleet service.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.SLOWindow <= 0 {
		o.SLOWindow = 28 * 24 * time.Hour
	}
	if o.MinCompleteness <= 0 {
		o.MinCompleteness = 0.98
	}
	s := &Service{opts: o}
	s.mux = s.routes()
	go s.refreshLoop()
	return s
}

// Handler returns the HTTP handler.
func (s *Service) Handler() http.Handler { return s.mux }

func (s *Service) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /api/v1/agents", s.handleAgents)
	mux.HandleFunc("GET /api/v1/agents/{id}", s.handleAgent)
	mux.HandleFunc("GET /api/v1/slo", s.handleSLO)
	mux.HandleFunc("GET /api/v1/telemetry/health", s.handleTelemetryHealth)
	mux.HandleFunc("GET /api/v1/chargeback", s.handleChargeback)
	mux.HandleFunc("GET /api/v1/signals/{id}", s.handleSignals)
	mux.HandleFunc("POST /v1/traces", s.opts.Trust.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version.Get()})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		checks := map[string]any{"registry": true}
		if _, err := s.opts.Store.ListAgents(r.Context(), registry.Filter{}); err != nil {
			checks["registry"] = false
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": checks})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": checks})
	})
	return mux
}

func (s *Service) refreshLoop() {
	// Half the freshness window below, so a scheduled rebuild almost always
	// lands before a reader would otherwise be served stale data.
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		views, err := s.build(ctx)
		cancel()
		if err != nil {
			s.opts.Logger.Warn("fleet refresh failed", "error", err)
			continue
		}
		s.mu.Lock()
		s.cached, s.cachedAt = views, time.Now()
		s.mu.Unlock()
		s.publishGauges(views)
		s.recordExpectedTraffic(context.Background(), views)
	}
}

// recordExpectedTraffic tells the trust tracker how many requests the gateway
// actually served for each agent, which is the denominator of trace
// completeness. Taking it from the gateway's own counter rather than from the
// trace stream is what makes a total collector outage show up as zero
// completeness instead of as no data at all.
func (s *Service) recordExpectedTraffic(ctx context.Context, views []AgentView) {
	if s.opts.Prom == nil || s.opts.Trust == nil {
		return
	}
	counts := s.byAgent(ctx, `sum by (agent, env) (increase(agentgate_gateway_requests_total[5m]))`)
	for _, v := range views {
		for _, ver := range v.Versions {
			key := agentNameOf(v.Identity) + "|" + string(ver.Env)
			if n, ok := counts[key]; ok {
				s.opts.Trust.RecordExpected(v.AgentID, string(ver.Env), int64(n))
			}
		}
	}
}

// agentNameOf returns the final segment of an agent identity URI, which is the
// value the gateway uses as the `agent` metric label.
func agentNameOf(identityURI string) string {
	if i := strings.LastIndex(identityURI, "/"); i >= 0 {
		return identityURI[i+1:]
	}
	return identityURI
}

func (s *Service) publishGauges(views []AgentView) {
	if s.opts.Metrics == nil {
		return
	}
	counts := map[string]map[registry.State]int{}
	for _, v := range views {
		for _, ver := range v.Versions {
			env := string(ver.Env)
			if counts[env] == nil {
				counts[env] = map[registry.State]int{}
			}
			counts[env][ver.State]++
		}
	}
	for env, byState := range counts {
		for state, n := range byState {
			s.opts.Metrics.FleetAgents.Set(float64(n), env, string(state))
		}
	}
	if s.opts.Anomaly != nil {
		for cc, ceiling := range s.opts.Anomaly.Ceilings() {
			s.opts.Metrics.DailyCeiling.Set(ceiling, s.opts.Env, cc)
		}
	}
}

// Views returns the fleet, rebuilding if the cache is cold.
func (s *Service) Views(ctx context.Context) ([]AgentView, error) {
	s.mu.RLock()
	cached, at := s.cached, s.cachedAt
	s.mu.RUnlock()
	// A newly registered agent should appear on the dashboard within one
	// refresh, not one minute and a half. The rebuild reads the registry and
	// four Prometheus queries, so this is cheap enough to do often.
	if cached != nil && time.Since(at) < 30*time.Second {
		return cached, nil
	}
	views, err := s.build(ctx)
	if err != nil {
		if cached != nil {
			return cached, nil // stale beats empty during a Prometheus outage
		}
		return nil, err
	}
	s.mu.Lock()
	s.cached, s.cachedAt = views, time.Now()
	s.mu.Unlock()
	return views, nil
}

// build joins the registry, the trust tracker, Prometheus and the cost
// aggregator into the fleet view.
func (s *Service) build(ctx context.Context) ([]AgentView, error) {
	agents, err := s.opts.Store.ListAgents(ctx, registry.Filter{})
	if err != nil {
		return nil, err
	}
	trust := map[string]TrustStats{}
	for _, t := range s.opts.Trust.Stats() {
		trust[t.AgentID+"|"+t.Env] = t
	}
	requests := s.byAgent(ctx, `sum by (agent, env) (increase(agentgate_gateway_requests_total[1h]))`)
	errors := s.byAgent(ctx, `sum by (agent, env) (increase(agentgate_gateway_requests_total{status=~"5.."}[1h]))`)
	tokens := s.byAgent(ctx, `sum by (agent, env) (increase(agentgate_gateway_tokens_total[1h]))`)
	spend := s.byAgent(ctx, `sum by (agent, env) (increase(agentgate_gateway_cost_usd_total[24h]))`)

	out := make([]AgentView, 0, len(agents))
	for _, a := range agents {
		v := AgentView{
			AgentID: a.AgentID, Identity: a.IdentityS, DisplayName: a.DisplayName,
			Team: a.Identity.Team, Tenant: a.Identity.Tenant, Owner: a.Owner,
			Runtime: a.Runtime, Framework: a.Framework,
			Classification: string(a.DataClassification),
			Versions:       a.Versions, Pools: a.PoolsGranted(identity.EnvProd),
		}
		if prod, ok := a.VersionIn(identity.EnvProd, registry.StateActive); ok {
			v.ProdVersion = prod.Version
		}
		env := "prod"
		if v.ProdVersion == "" {
			env = mostRecentEnv(a)
		}
		key := a.Identity.Name + "|" + env
		v.RequestsLastHour = int64(requests[key])
		v.TokensLastHour = int64(tokens[key])
		v.SpendLast24h = spend[key]
		if v.RequestsLastHour > 0 {
			v.ErrorRate = errors[key] / float64(v.RequestsLastHour)
		}
		if t, ok := trust[a.AgentID+"|"+env]; ok {
			v.Telemetry = t
		}
		if s.opts.Aggregator != nil {
			for _, r := range s.opts.Aggregator.Rollups("day", a.Owner.CostCenter) {
				if r.AgentID == a.AgentID {
					v.SpendMonthToDate += r.CostUSD
				}
			}
		}
		v.Health, v.Issues = s.assess(a, v)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out, nil
}

func mostRecentEnv(a *registry.Agent) string {
	best := "dev"
	rank := map[identity.Environment]int{identity.EnvDev: 0, identity.EnvStaging: 1, identity.EnvProd: 2}
	high := -1
	for _, v := range a.Versions {
		if r := rank[v.Env]; r > high {
			high, best = r, string(v.Env)
		}
	}
	return best
}

// assess turns the joined data into a health verdict and a list of the things
// an owner needs to fix. The verdict is deliberately opinionated: a fleet view
// that reports everything as "green" until it is on fire is a decoration.
func (s *Service) assess(a *registry.Agent, v AgentView) (string, []string) {
	var issues []string
	health := "healthy"
	degrade := func(level, issue string) {
		issues = append(issues, issue)
		if level == "unhealthy" || health == "healthy" {
			health = level
		}
	}
	if a.Owner.OnCall == "" {
		degrade("degraded", "no on-call rotation recorded")
	}
	if a.Owner.CostCenter == "" {
		degrade("degraded", "no cost centre; consumption cannot be charged back")
	}
	if a.SecurityReviewRef == "" && v.ProdVersion != "" {
		degrade("degraded", "running in production with no linked security review")
	}
	if v.Telemetry.SpansReceived == 0 && v.RequestsLastHour > 0 {
		degrade("unhealthy", "gateway traffic observed but no traces received")
	} else if v.Telemetry.Completeness > 0 && v.Telemetry.Completeness < s.opts.MinCompleteness {
		degrade("degraded", fmt.Sprintf("trace completeness %.2f is below the %.2f bar",
			v.Telemetry.Completeness, s.opts.MinCompleteness))
	}
	if v.Telemetry.UnattributedRatio > 0.01 {
		degrade("degraded", fmt.Sprintf("%.1f%% of spans lack ownership attributes",
			v.Telemetry.UnattributedRatio*100))
	}
	if v.Telemetry.ClockSkewP99Sec > 5 {
		degrade("degraded", fmt.Sprintf("clock skew of %.1fs will misorder traces", v.Telemetry.ClockSkewP99Sec))
	}
	if v.ErrorRate > 0.05 {
		degrade("unhealthy", fmt.Sprintf("error rate %.1f%% over the last hour", v.ErrorRate*100))
	}
	for _, ver := range v.Versions {
		if ver.State == registry.StateQuarantined {
			degrade("unhealthy", "version "+ver.Version+" is quarantined")
		}
	}
	return health, issues
}

func (s *Service) byAgent(ctx context.Context, expr string) map[string]float64 {
	out := map[string]float64{}
	if s.opts.Prom == nil {
		return out
	}
	samples, err := s.opts.Prom.Query(ctx, expr)
	if err != nil {
		s.opts.Logger.Debug("prometheus query failed", "expr", expr, "error", err)
		return out
	}
	for _, sm := range samples {
		out[sm.Labels["agent"]+"|"+sm.Labels["env"]] += sm.Value
	}
	return out
}

// --- HTTP handlers --------------------------------------------------------

func (s *Service) handleAgents(w http.ResponseWriter, r *http.Request) {
	views, err := s.Views(r.Context())
	if err != nil {
		httpx.WriteProblem(w, httpx.Errorf(httpx.CodeInternal, "could not build the fleet view: %v", err), "", "")
		return
	}
	team := r.URL.Query().Get("team")
	env := r.URL.Query().Get("env")
	health := r.URL.Query().Get("health")
	filtered := make([]AgentView, 0, len(views))
	for _, v := range views {
		if team != "" && v.Team != team {
			continue
		}
		if health != "" && v.Health != health {
			continue
		}
		if env != "" {
			has := false
			for _, ver := range v.Versions {
				if string(ver.Env) == env {
					has = true
				}
			}
			if !has {
				continue
			}
		}
		filtered = append(filtered, v)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"agents": filtered, "count": len(filtered), "generated_at": time.Now().UTC(),
	})
}

func (s *Service) handleAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	views, err := s.Views(r.Context())
	if err != nil {
		httpx.WriteProblem(w, httpx.Errorf(httpx.CodeInternal, "could not build the fleet view"), "", "")
		return
	}
	for _, v := range views {
		if v.AgentID == id || v.Identity == id {
			httpx.WriteJSON(w, http.StatusOK, v)
			return
		}
	}
	httpx.WriteProblem(w, httpx.Errorf(httpx.CodeNotFound, "unknown agent %q", id), "", "")
}

func (s *Service) handleSLO(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cached, at := s.sloCache, s.sloAt
	s.mu.RUnlock()
	if cached != nil && time.Since(at) < 30*time.Second {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"objectives": cached})
		return
	}
	out := make([]Status, 0, 4)
	for _, o := range DefaultObjectives(s.opts.SLOWindow) {
		out = append(out, Evaluate(r.Context(), s.opts.Prom, o))
	}
	s.mu.Lock()
	s.sloCache, s.sloAt = out, time.Now()
	s.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"objectives": out, "window": s.opts.SLOWindow.String()})
}

func (s *Service) handleTelemetryHealth(w http.ResponseWriter, _ *http.Request) {
	stats := s.opts.Trust.Stats()
	healthy, unhealthy := 0, 0
	for _, t := range stats {
		if t.Healthy(s.opts.MinCompleteness) {
			healthy++
		} else {
			unhealthy++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"agents": stats, "healthy": healthy, "unhealthy": unhealthy,
		"min_completeness": s.opts.MinCompleteness,
	})
}

func (s *Service) handleChargeback(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "day"
	}
	costCenter := r.URL.Query().Get("cost_center")

	var rollups []cost.Rollup
	if s.opts.Aggregator != nil {
		rollups = s.opts.Aggregator.Rollups(period, costCenter)
	}
	if len(rollups) == 0 && s.opts.UsageDir != "" {
		var err error
		rollups, err = rollupFromFiles(s.opts.UsageDir, period, costCenter)
		if err != nil {
			s.opts.Logger.Warn("could not read usage files", "error", err)
		}
	}
	sort.Slice(rollups, func(i, j int) bool { return rollups[i].CostUSD > rollups[j].CostUSD })

	totals := map[string]float64{}
	var grand, savings float64
	for _, r := range rollups {
		totals[r.CostCenter] += r.CostUSD
		grand += r.CostUSD
		savings += r.SavingsUSD
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"period": period, "rollups": rollups, "by_cost_center": totals,
		"total_usd": grand, "savings_usd": savings, "generated_at": time.Now().UTC(),
	})
}

// handleSignals implements the registry.SignalSource contract over HTTP, so
// the control plane's promotion gate reads the same numbers the dashboard
// shows rather than a second computation of them.
func (s *Service) handleSignals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	env := r.URL.Query().Get("env")
	if env == "" {
		env = "staging"
	}
	sig, err := s.Signals(r.Context(), id, identity.Environment(env))
	if err != nil {
		httpx.WriteProblem(w, httpx.Errorf(httpx.CodeInternal, "could not gather signals: %v", err), "", "")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sig)
}

// Signals implements registry.SignalSource in-process.
func (s *Service) Signals(ctx context.Context, agentID string, env identity.Environment) (registry.AgentSignals, error) {
	agent, err := s.opts.Store.GetAgent(ctx, agentID)
	if err != nil {
		return registry.AgentSignals{}, err
	}
	sig := registry.AgentSignals{WindowSeconds: int64((24 * time.Hour).Seconds()), SuccessObjective: 0.99}
	if t, ok := s.opts.Trust.StatsFor(agentID, string(env)); ok {
		sig.TelemetryCompleteness = t.Completeness
		sig.RequestCount = t.GatewayRequests
	}
	name := agent.Identity.Name
	total := s.scalar(ctx, fmt.Sprintf(
		`sum(increase(agentgate_gateway_requests_total{agent="%s",env="%s"}[24h]))`, name, env))
	failed := s.scalar(ctx, fmt.Sprintf(
		`sum(increase(agentgate_gateway_requests_total{agent="%s",env="%s",status=~"5.."}[24h]))`, name, env))
	if total > 0 {
		sig.SuccessRatio = (total - failed) / total
		if sig.RequestCount == 0 {
			sig.RequestCount = int64(total)
		}
	}
	sig.CriticalGuardrailHits = int64(s.scalar(ctx, fmt.Sprintf(
		`sum(increase(agentgate_guardrail_decisions_total{action="block",env="%s"}[7d]))`, env)))
	spend := s.scalar(ctx, fmt.Sprintf(
		`sum(increase(agentgate_gateway_cost_usd_total{agent="%s",env="%s"}[7d]))`, name, env))
	if spend > 0 {
		sig.ProjectedMonthlyCostUSD = spend / 7 * 30
	}
	if att, err := s.opts.Store.Attestation(ctx, agentID, env); err == nil {
		sig.ObservedAttestation = att
	}
	sig.SecurityReviewRef = agent.SecurityReviewRef
	return sig, nil
}

func (s *Service) scalar(ctx context.Context, expr string) float64 {
	if s.opts.Prom == nil {
		return 0
	}
	v, err := s.opts.Prom.Scalar(ctx, expr)
	if err != nil || v != v { // NaN check
		return 0
	}
	return v
}

// rollupFromFiles reads the local usage journal when no warehouse is
// available. It exists so a chargeback report can be produced in a
// disconnected environment, and so the local stack demonstrates the whole
// path end to end.
func rollupFromFiles(dir, period, costCenter string) ([]cost.Rollup, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	agg := map[string]*cost.Rollup{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec cost.Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if costCenter != "" && rec.CostCenter != costCenter {
				continue
			}
			bucket := rec.Timestamp.UTC().Format("2006-01-02")
			if period == "hour" {
				bucket = rec.Timestamp.UTC().Format("2006-01-02T15")
			}
			key := bucket + "|" + rec.Key()
			r, ok := agg[key]
			if !ok {
				r = &cost.Rollup{
					Period: period, Env: rec.Env, Tenant: rec.Tenant, Team: rec.Team,
					AgentID: rec.AgentID, CostCenter: rec.CostCenter,
					Start: rec.Timestamp.UTC().Truncate(24 * time.Hour),
				}
				agg[key] = r
			}
			r.Requests++
			r.InputTokens += int64(rec.InputTokens)
			r.OutputTokens += int64(rec.OutputTokens)
			r.CostUSD += rec.CostUSD
			r.SavingsUSD += rec.SavingsUSD
			if rec.ErrorCode != "" {
				r.Errors++
			}
		}
	}
	out := make([]cost.Rollup, 0, len(agg))
	for _, r := range agg {
		out = append(out, *r)
	}
	return out, nil
}
