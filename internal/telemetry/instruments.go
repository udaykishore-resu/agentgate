package telemetry

// Instruments is the AgentGate metric catalogue, created once per process.
//
// Label sets are deliberate. Per-agent labels appear on counters, where the
// cardinality cost is one series per agent, but not on histograms, where it
// would be one series per agent per bucket. The cardinality budget and the
// reasoning are in docs/06-telemetry-schema.md.
type Instruments struct {
	// Gateway traffic plane.
	Requests     *Counter   // env, tenant, team, agent, pool, backend, status, code, stream, cache, attribution_corrected
	Duration     *Histogram // env, pool, backend, stream — total request time
	Overhead     *Histogram // env, pool, stream — gateway time excluding provider
	TTFT         *Histogram // env, pool, backend — time to first token
	InterToken   *Histogram // env, pool, backend
	StreamStalls *Counter   // env, pool, backend
	Tokens       *Counter   // env, tenant, team, agent, backend, direction
	CostUSD      *Counter   // env, tenant, team, agent, cost_center, backend
	CacheSavings *Counter   // env, tenant, team, cost_center
	Inflight     *Gauge     // env, pool
	Shed         *Counter   // env, pool, priority

	// Policy outcomes.
	RateLimit      *Counter   // env, tenant, team, agent, decision
	QuotaUsedRatio *Gauge     // env, tenant, team, agent
	CacheLookups   *Counter   // env, pool, result
	CacheSimilar   *Histogram // env, pool
	Guardrail      *Counter   // env, pool, category, action
	GuardrailFail  *Counter   // env, pool, mode
	GuardrailMode  *Gauge     // env, pool, mode
	GuardrailLat   *Histogram // env, pool
	GuardrailErr   *Counter   // env, pool, reason
	Retries        *Counter   // env, backend, reason
	Breaker        *Gauge     // env, pool, backend, state

	// Control plane.
	TokenExchange    *Counter   // env, result, grant_type
	TokenExchangeLat *Histogram // env
	TokenValidations *Counter   // env, result, reason
	JWKSRefresh      *Counter   // env, result
	JWKSKeyAge       *Gauge     // env, kid
	SecretAge        *Gauge     // env, agent
	CertExpiry       *Gauge     // env, host
	PromotionGate    *Counter   // env, from_env, to_env, gate, result

	// Observability plane.
	FleetAgents      *Gauge   // env, state
	Completeness     *Gauge   // env, agent
	OrphanRatio      *Gauge   // env, agent
	UnattributedRate *Gauge   // env, agent
	ClockSkew        *Gauge   // env, agent
	CostAnomalies    *Counter // env, tenant, team, agent
	DailyCeiling     *Gauge   // env, cost_center
	UsageRecords     *Counter // env, result
	SpansExported    *Counter // env, result
	CanaryWeight     *Gauge   // env, consumer
}

// NewInstruments registers the whole catalogue against a registry.
func NewInstruments(r *Registry) *Instruments {
	return &Instruments{
		Requests: r.Counter("agentgate_gateway_requests_total",
			"Gateway requests by outcome, routing decision and owning team.",
			"env", "tenant", "team", "agent", "pool", "backend", "status", "code", "stream", "cache", "attribution_corrected"),
		Duration: r.Histogram("agentgate_gateway_duration_seconds",
			"End-to-end gateway request duration including provider time.",
			DefaultLatencyBuckets, "env", "pool", "backend", "stream"),
		Overhead: r.Histogram("agentgate_gateway_overhead_seconds",
			"Gateway processing time excluding provider time. This is the latency SLI.",
			OverheadBuckets, "env", "pool", "stream"),
		TTFT: r.Histogram("agentgate_gateway_ttft_seconds",
			"Time from request receipt to first content token on a streaming response.",
			TTFTBuckets, "env", "pool", "backend"),
		InterToken: r.Histogram("agentgate_stream_intertoken_seconds",
			"Gap between consecutive content chunks on a streaming response.",
			[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5}, "env", "pool", "backend"),
		StreamStalls: r.Counter("agentgate_stream_stalls_total",
			"Streams where no content arrived within the stall threshold.",
			"env", "pool", "backend"),
		Tokens: r.Counter("agentgate_gateway_tokens_total",
			"Billed tokens by direction and owning team.",
			"env", "tenant", "team", "agent", "backend", "direction"),
		CostUSD: r.Counter("agentgate_gateway_cost_usd_total",
			"Attributed inference spend in USD.",
			"env", "tenant", "team", "agent", "cost_center", "backend"),
		CacheSavings: r.Counter("agentgate_gateway_cache_savings_usd_total",
			"Spend avoided by cache hits, in USD.",
			"env", "tenant", "team", "cost_center"),
		Inflight: r.Gauge("agentgate_gateway_inflight",
			"Requests currently in flight per pool.", "env", "pool"),
		Shed: r.Counter("agentgate_gateway_shed_total",
			"Requests shed by the load shedder before reaching a backend.",
			"env", "pool", "priority"),

		RateLimit: r.Counter("agentgate_ratelimit_decisions_total",
			"Rate limit and quota decisions.",
			"env", "tenant", "team", "agent", "decision"),
		QuotaUsedRatio: r.Gauge("agentgate_quota_monthly_budget_used_ratio",
			"Fraction of the agent's monthly token budget consumed.",
			"env", "tenant", "team", "agent"),
		CacheLookups: r.Counter("agentgate_cache_lookups_total",
			"Cache lookups by result.", "env", "pool", "result"),
		CacheSimilar: r.Histogram("agentgate_cache_semantic_similarity",
			"Cosine similarity of the best semantic cache candidate.",
			[]float64{0.5, 0.7, 0.8, 0.9, 0.95, 0.97, 0.98, 0.99, 1.0}, "env", "pool"),
		Guardrail: r.Counter("agentgate_guardrail_decisions_total",
			"Guardrail decisions by category and action.",
			"env", "pool", "category", "action"),
		GuardrailFail: r.Counter("agentgate_guardrail_failopen_total",
			"Requests allowed through because the guardrail service was unavailable and the pool is fail-open.",
			"env", "pool", "mode"),
		GuardrailMode: r.Gauge("agentgate_guardrail_failmode",
			"Configured guardrail failure mode: 1 for the active mode.",
			"env", "pool", "mode"),
		GuardrailLat: r.Histogram("agentgate_guardrail_callout_duration_seconds",
			"Guardrail callout latency.",
			[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2}, "env", "pool"),
		GuardrailErr: r.Counter("agentgate_guardrail_callout_failures_total",
			"Guardrail callout failures by reason.", "env", "pool", "reason"),
		Retries: r.Counter("agentgate_retry_attempts_total",
			"Backend retry attempts by reason.", "env", "backend", "reason"),
		Breaker: r.Gauge("agentgate_breaker_state",
			"Circuit breaker state, 1 for the active state.",
			"env", "pool", "backend", "state"),

		TokenExchange: r.Counter("agentgate_controlplane_token_exchange_total",
			"Token exchange attempts by result.", "env", "result", "grant_type"),
		TokenExchangeLat: r.Histogram("agentgate_controlplane_token_exchange_duration_seconds",
			"Token exchange latency.", OverheadBuckets, "env"),
		TokenValidations: r.Counter("agentgate_identity_token_validations_total",
			"Gateway token validations by result.", "env", "result", "reason"),
		JWKSRefresh: r.Counter("agentgate_controlplane_jwks_refresh_total",
			"JWKS refresh attempts by result.", "env", "result"),
		JWKSKeyAge: r.Gauge("agentgate_controlplane_jwks_key_age_seconds",
			"Age of each active signing key.", "env", "kid"),
		SecretAge: r.Gauge("agentgate_secret_age_seconds",
			"Age of each agent client secret, for rotation alerting.", "env", "agent"),
		CertExpiry: r.Gauge("agentgate_certificate_expiry_seconds",
			"Seconds until certificate expiry.", "env", "host"),
		PromotionGate: r.Counter("agentgate_promotion_gate_evaluations_total",
			"Promotion gate evaluations by gate and result.",
			"env", "from_env", "to_env", "gate", "result"),

		FleetAgents: r.Gauge("agentgate_fleet_agents",
			"Registered agents by environment and lifecycle state.", "env", "state"),
		Completeness: r.Gauge("agentgate_telemetry_completeness",
			"Ratio of traces received to gateway requests observed, per agent.", "env", "agent"),
		OrphanRatio: r.Gauge("agentgate_telemetry_orphan_span_ratio",
			"Ratio of spans whose parent never arrived, per agent.", "env", "agent"),
		UnattributedRate: r.Gauge("agentgate_telemetry_unattributed_ratio",
			"Ratio of spans missing owner or cost centre attributes, per agent.", "env", "agent"),
		ClockSkew: r.Gauge("agentgate_telemetry_clock_skew_seconds",
			"p99 difference between agent-reported and gateway-observed timestamps.", "env", "agent"),
		CostAnomalies: r.Counter("agentgate_fleet_cost_anomalies_total",
			"Cost anomalies detected.", "env", "tenant", "team", "agent"),
		DailyCeiling: r.Gauge("agentgate_cost_center_daily_ceiling_usd",
			"Configured hard daily spend ceiling per cost centre.", "env", "cost_center"),
		UsageRecords: r.Counter("agentgate_usage_records_written_total",
			"Chargeback usage records written by result.", "env", "result"),
		SpansExported: r.Counter("agentgate_spans_exported_total",
			"Spans handed to the collector by result.", "env", "result"),
		CanaryWeight: r.Gauge("agentgate_canary_weight",
			"Fraction of a consumer's traffic currently served by AgentGate during migration.",
			"env", "consumer"),
	}
}
