# 07 — SLIs, SLOs, Error Budgets and Alerting

**Audience:** platform engineers, on-call, the client's service management function.

Everything here is written to be implementable without further interpretation: each SLI states its
numerator, denominator and exclusions; each alert states its expression, severity, routing and
runbook.

---

## 1. Principles

1. **An SLI is a ratio of good events to valid events.** Not an average, not a gauge.
2. **Exclusions are written down.** Anything excluded from a denominator is named here. Silent
   exclusions are how an SLO becomes a fiction.
3. **The user of the SLI is the consuming agent, not the platform.** Where the platform's convenience
   and the consumer's experience diverge, the consumer's experience defines the SLI.
4. **Alerts are on burn rate, not on thresholds**, except where the condition is not budget-shaped
   (breaker open, telemetry degraded, cost anomaly).
5. **Every alert has a runbook, and the runbook link is in the alert annotation.** An alert without a
   runbook is deleted, not documented later.
6. **Pages are for things a human must act on now.** Everything else is a ticket.

---

## 2. SLI definitions

### 2.1 Gateway availability

| | |
|---|---|
| **Statement** | The proportion of valid gateway requests that did not fail for a reason attributable to the platform. |
| **Numerator** | Requests where `status` is not 5xx **and** `code` is not `no_healthy_backend`. |
| **Denominator** | All valid requests, as defined by the exclusions below. |
| **Window** | 28 days rolling. |
| **Objective** | **99.9%** |
| **Error budget** | 0.1% of 40,320 minutes = **40.32 minutes = 40m 19s** of full outage equivalent. |

**Exclusions from the denominator:**

| Excluded | Why |
|---|---|
| 400 `invalid_request` | Caller sent a malformed request. Not a platform failure |
| 401 `unauthenticated` | Caller's token problem |
| 403 `forbidden_pool`, `agent_not_promoted` | Correct enforcement of policy |
| 403 `guardrail_blocked` | Correct enforcement of policy |
| 404 `unknown_model` | Caller error |
| 409 `idempotency_conflict` | Caller error |
| 413 `context_too_large` | Caller error |
| 429 `rate_limited`, `quota_exceeded` | Correct enforcement of a limit the caller agreed to |
| 499 `client_closed_request` | Caller disconnected. The platform did not fail |
| Requests to `/healthz`, `/readyz`, `/metrics` | Infrastructure endpoints |
| Requests marked `x-agentgate-shadow: true` | Migration shadow traffic serves no consumer |

**Included as failures:**

| Included | Why |
|---|---|
| 502 `provider_error` | The platform is responsible for provider selection, retries and failover. A provider failure that reaches the caller is a platform failure |
| 503 `no_healthy_backend` | Explicitly named in the SLI |
| 504 `provider_timeout` | As above |
| 408 `client_timeout` | **[Decision] Included.** A caller's deadline being exceeded is usually the platform being slow. Excluding it would let the platform hide latency failures behind caller impatience |
| Any unhandled 5xx | Obviously |

This is the most consequential judgement in the document: **provider failures count against us**. The
alternative — excluding them as third-party — would make the SLO measure something no consumer cares
about. A consumer does not experience "Azure OpenAI was down"; they experience "the gateway did not
answer". Owning provider failures is what makes multi-backend pools, failover tiers and circuit
breakers worth building.

**PromQL:**

```promql
# Good events
sum(rate(agentgate_gateway_requests_total{
      code!~"invalid_request|unauthenticated|forbidden_pool|agent_not_promoted|guardrail_blocked|unknown_model|idempotency_conflict|context_too_large|rate_limited|quota_exceeded|client_closed_request",
      status!~"5..",
      shadow="false"
    }[28d]))
/
# Valid events
sum(rate(agentgate_gateway_requests_total{
      code!~"invalid_request|unauthenticated|forbidden_pool|agent_not_promoted|guardrail_blocked|unknown_model|idempotency_conflict|context_too_large|rate_limited|quota_exceeded|client_closed_request",
      shadow="false"
    }[28d]))
```

Recorded as `agentgate:gateway:availability_sli:ratio_rate28d`, with `5m`, `30m`, `1h`, `6h`, `3d`
variants for burn-rate alerting.

---

### 2.2 Gateway latency — unary overhead

| | |
|---|---|
| **Statement** | The proportion of minutes in which p95 gateway overhead, excluding provider time, was under 60 ms. |
| **Overhead defined** | `agentgate.overhead_ms` = total time inside the gateway minus the time spent in the `invoke` stage waiting on the provider. It is the sum of the other 15 stage durations plus routing and framing. |
| **Numerator** | Minutes where the p95 of `agentgate.overhead_ms` over that minute was < 60 ms. |
| **Denominator** | Minutes with at least **[Decision] 30 valid unary requests**. Below that, p95 is not meaningful and the minute is not counted. |
| **Window** | 28 days rolling. |
| **Objective** | **99% of qualifying minutes.** |

**Exclusions:** streaming requests (measured separately by TTFT), cache hits (**[Decision]** excluded,
since a cache hit has near-zero overhead and including them would flatter the number), requests
excluded from availability, shadow traffic.

Excluding provider time is the point. Provider latency is not something the platform controls on a
per-request basis; overhead is entirely ours. A latency SLI that included provider time would be a
measure of the provider's health wearing the platform's name.

**PromQL:**

```promql
histogram_quantile(0.95,
  sum by (le) (
    rate(agentgate_gateway_overhead_ms_bucket{stream="false", cache="miss", shadow="false"}[1m])
  )
) < 60
```

Evaluated per minute; the SLI is the fraction of qualifying minutes where this holds, recorded as
`agentgate:gateway:overhead_p95_ok:ratio_rate28d`.

---

### 2.3 Gateway TTFT — streaming

| | |
|---|---|
| **Statement** | The proportion of streaming requests whose time-to-first-token was under 1200 ms. |
| **Numerator** | Streaming requests where `agentgate.ttft_ms` < 1200. |
| **Denominator** | All valid streaming requests. |
| **Window** | 28 days rolling. |
| **Objective** | **p95 < 1200 ms, at 99%.** Implemented as: 99% of qualifying minutes have p95 TTFT < 1200 ms. |

**TTFT is measured at the moment the first content byte is released to the caller**, not at the
moment the provider sent its first token. The difference is the guardrail output window — roughly
256 tokens of buffering — and it is genuinely part of the consumer's experience, so it belongs in the
number.

**Exclusions:** cache hits, requests that never produced a first token because they failed (counted
in availability instead), shadow traffic.

**PromQL:**

```promql
histogram_quantile(0.95,
  sum by (le) (rate(agentgate_gateway_ttft_ms_bucket{shadow="false"}[1m]))
) < 1200
```

---

### 2.4 Control plane

| | |
|---|---|
| **Statement** | The proportion of token-exchange requests that succeeded, and the p95 latency of those exchanges. |
| **Numerator** | Token exchanges returning a token, plus exchanges correctly rejected for a caller-side reason (invalid subject token, unregistered agent). |
| **Denominator** | All token-exchange requests. |
| **Window** | 28 days rolling. |
| **Objective** | **99.95% success, p95 < 150 ms.** |
| **Error budget** | 0.05% of 40,320 minutes = **20.16 minutes = 20m 10s**. |

**Exclusions:** requests from unregistered issuers (rejected before processing), health endpoints.

A correct rejection counts as a success because the control plane did its job. The SLI measures
whether the control plane is *working*, not whether every caller is entitled.

**PromQL:**

```promql
sum(rate(agentgate_controlplane_token_exchange_total{outcome=~"issued|rejected"}[28d]))
/
sum(rate(agentgate_controlplane_token_exchange_total[28d]))
```

---

### 2.5 Telemetry completeness

| | |
|---|---|
| **Statement** | The proportion of agents whose telemetry is trustworthy. |
| **Numerator** | Agent-environment pairs with `completeness_ratio` ≥ 0.98 over the evaluation window. |
| **Denominator** | All agent-environment pairs with at least **[Decision] 100 gateway requests** in the window. |
| **Window** | 28 days rolling, evaluated on 60-second computations. |
| **Objective** | **99% of qualifying agents.** |

**Exclusions:** agents below the request threshold (a ratio computed on 3 requests is noise), agents
in `dev` (**[Decision]** dev telemetry quality is the team's own concern until they want to promote,
at which point the gate enforces it directly).

**PromQL:**

```promql
count(agentgate_telemetry_completeness{env=~"staging|prod"} >= 0.98)
/
count(agentgate_telemetry_completeness{env=~"staging|prod"})
```

---

### 2.6 Cost anomaly detection

| | |
|---|---|
| **Statement** | The proportion of genuine cost anomalies that the platform surfaced before the consuming team reported them. |
| **Numerator** | Anomalies where an alert fired before a human report was logged. |
| **Denominator** | All confirmed cost anomalies, however discovered. |
| **Window** | 90 days rolling — anomalies are rare, so a 28-day window has too few events. |
| **Objective** | **95%.** |

**This SLI requires human input to compute.** Confirmed anomalies and their discovery paths are
recorded during triage; **[Decision]** the on-call records the discovery path in the incident record,
and the SLI is computed from those records monthly. A metric that cannot be computed automatically is
still worth having if the alternative is not measuring the thing at all — but it must be honest about
how it is produced.

---

## 3. Error budgets

| Service | Objective | Window | Budget | Budget as downtime |
|---|---|---|---|---|
| Gateway availability | 99.9% | 28 d | 0.1% of 40,320 min | **40m 19s** |
| Control plane | 99.95% | 28 d | 0.05% of 40,320 min | **20m 10s** |
| Gateway latency, unary | 99% of qualifying minutes | 28 d | 1% of qualifying minutes | ~403 min of qualifying minutes |
| Gateway TTFT, stream | 99% of qualifying minutes | 28 d | 1% of qualifying minutes | ~403 min of qualifying minutes |
| Telemetry completeness | 99% of agents | 28 d | 1% of agents | 3 agents in a 300-agent fleet |

Budget consumption is displayed as a percentage remaining and as an absolute figure, because "62%
remaining" and "25 minutes left" prompt different conversations.

---

## 4. Burn-rate alerting

Multi-window, multi-burn-rate, on availability and latency. The arithmetic is shown so the thresholds
can be re-derived when an objective changes.

### 4.1 The arithmetic

Burn rate is the multiple of the budget-consumption rate that would exactly exhaust the budget over
the objective window.

```
budget_window          = 28 d = 40,320 minutes
error_budget_fraction  = 1 - SLO = 0.001   for 99.9%
budget_minutes         = 40,320 x 0.001 = 40.32 minutes

For "consume X% of the budget in T minutes":
  budget_minutes_consumed = X x budget_minutes
  error_rate_required     = budget_minutes_consumed / T
  burn_rate               = error_rate_required / error_budget_fraction
```

| Target | X | T | Budget minutes consumed | Error rate required | **Burn rate** |
|---|---|---|---|---|---|
| Fast page | 2% | 1 h = 60 min | 0.8064 | 1.344% | **13.44×** |
| Medium page | 5% | 6 h = 360 min | 2.016 | 0.560% | **5.6×** |
| Slow ticket | 10% | 3 d = 4,320 min | 4.032 | 0.0933% | **0.933×** |

Each alert uses a **long window** for the condition and a **short window** for reset speed. The short
window is conventionally 1/12 of the long window, which bounds how long an alert continues to fire
after the condition clears.

| Alert | Long window | Short window | Burn-rate threshold | Time to fire at exactly the threshold | Detection budget spend |
|---|---|---|---|---|---|
| `AgentGateAvailabilityFastBurn` | 1 h | 5 m | 13.44 | ~5 min | ~0.17% of budget |
| `AgentGateAvailabilityMediumBurn` | 6 h | 30 m | 5.6 | ~30 min | ~0.42% of budget |
| `AgentGateAvailabilitySlowBurn` | 3 d | 6 h | 0.933 | ~6 h | ~1.4% of budget |

Both windows must exceed the threshold for the alert to fire. This is what suppresses a single
one-minute spike from paging.

### 4.2 Control-plane burn rates

Same method, `error_budget_fraction = 0.0005`, `budget_minutes = 20.16`.

| Target | X | T | Error rate required | **Burn rate** |
|---|---|---|---|---|
| Fast page | 2% | 1 h | 0.672% | **13.44×** |
| Medium page | 5% | 6 h | 0.280% | **5.6×** |
| Slow ticket | 10% | 3 d | 0.0467% | **0.933×** |

The burn-rate multipliers are identical because they are a function of the window ratios, not of the
objective. Only the absolute error rate that triggers them differs.

---

## 5. Alert catalogue

Severity: **P1** pages immediately, 24×7. **P2** pages during business hours, ticket out of hours.
**P3** ticket only.

Every alert carries `runbook_url` in its annotations, pointing at `docs/runbooks/<name>.md`.

### 5.1 Availability and latency

| Name | Severity | Page or ticket | Expression | Runbook |
|---|---|---|---|---|
| `AgentGateAvailabilityFastBurn` | P1 | Page | see below | `runbooks/availability-burn.md` |
| `AgentGateAvailabilityMediumBurn` | P1 | Page | see below | `runbooks/availability-burn.md` |
| `AgentGateAvailabilitySlowBurn` | P3 | Ticket | see below | `runbooks/availability-burn.md` |
| `AgentGateLatencyFastBurn` | P2 | Page | see below | `runbooks/latency-burn.md` |
| `AgentGateTTFTDegraded` | P2 | Page | see below | `runbooks/ttft-degraded.md` |

```promql
# AgentGateAvailabilityFastBurn — P1, page
(
  (1 - agentgate:gateway:availability_sli:ratio_rate1h)  > (13.44 * 0.001)
  and
  (1 - agentgate:gateway:availability_sli:ratio_rate5m)  > (13.44 * 0.001)
)
```

```promql
# AgentGateAvailabilityMediumBurn — P1, page
(
  (1 - agentgate:gateway:availability_sli:ratio_rate6h)  > (5.6 * 0.001)
  and
  (1 - agentgate:gateway:availability_sli:ratio_rate30m) > (5.6 * 0.001)
)
```

```promql
# AgentGateAvailabilitySlowBurn — P3, ticket
(
  (1 - agentgate:gateway:availability_sli:ratio_rate3d)  > (0.933 * 0.001)
  and
  (1 - agentgate:gateway:availability_sli:ratio_rate6h)  > (0.933 * 0.001)
)
```

```promql
# AgentGateLatencyFastBurn — P2, page
# p95 gateway overhead above the 60ms objective, sustained.
(
  histogram_quantile(0.95,
    sum by (le) (rate(agentgate_gateway_overhead_ms_bucket{stream="false",cache="miss",shadow="false"}[1h]))
  ) > 60
  and
  histogram_quantile(0.95,
    sum by (le) (rate(agentgate_gateway_overhead_ms_bucket{stream="false",cache="miss",shadow="false"}[5m]))
  ) > 60
)
```

```promql
# AgentGateTTFTDegraded — P2, page
(
  histogram_quantile(0.95,
    sum by (le) (rate(agentgate_gateway_ttft_ms_bucket{shadow="false"}[1h]))
  ) > 1200
  and
  histogram_quantile(0.95,
    sum by (le) (rate(agentgate_gateway_ttft_ms_bucket{shadow="false"}[5m]))
  ) > 1200
)
```

### 5.2 Control plane

| Name | Severity | Page or ticket | Runbook |
|---|---|---|---|
| `AgentGateControlPlaneFastBurn` | P1 | Page | `runbooks/controlplane-burn.md` |
| `AgentGateControlPlaneLatency` | P2 | Page | `runbooks/controlplane-latency.md` |
| `AgentGateJWKSUnavailable` | P1 | Page | `runbooks/jwks-unavailable.md` |

```promql
# AgentGateControlPlaneFastBurn — P1, page
(
  (1 - agentgate:controlplane:token_exchange_sli:ratio_rate1h) > (13.44 * 0.0005)
  and
  (1 - agentgate:controlplane:token_exchange_sli:ratio_rate5m) > (13.44 * 0.0005)
)
```

```promql
# AgentGateControlPlaneLatency — P2, page
histogram_quantile(0.95,
  sum by (le) (rate(agentgate_controlplane_token_exchange_duration_ms_bucket[10m]))
) > 150
```

```promql
# AgentGateJWKSUnavailable — P1, page
# Gateways are serving stale JWKS. Traffic is fine now; it will not be in 24h.
max(agentgate_gateway_jwks_cache_age_seconds) > 3600
```

The JWKS alert fires at one hour of staleness even though stale serving is permitted for 24 hours.
The point is to have 23 hours of margin, not to be told at hour 23.

### 5.3 Resilience and dependencies

| Name | Severity | Page or ticket | Expression | Runbook |
|---|---|---|---|---|
| `AgentGateBreakerOpen` | P2 | Page | `max by (backend) (agentgate_breaker_state{state="open"}) == 1` for 5m | `runbooks/breaker-open.md` |
| `AgentGatePoolAllBackendsOpen` | P1 | Page | `count by (pool) (agentgate_breaker_state{state="open"} == 1) == count by (pool) (agentgate_backend_configured)` | `runbooks/pool-exhausted.md` |
| `AgentGateRetryBudgetExhausted` | P2 | Page | `sum(rate(agentgate_retry_attempts_total[5m])) / sum(rate(agentgate_gateway_requests_total[5m])) > 0.10` | `runbooks/retry-storm.md` |
| `AgentGateRedisUnavailable` | P1 | Page | `agentgate_ratelimit_degraded == 1` for 2m | `runbooks/redis-unavailable.md` |
| `AgentGateGuardrailUnavailable` | P1 | Page | `sum(rate(agentgate_guardrail_decisions_total{action="unavailable"}[5m])) > 0` for 5m | `runbooks/guardrail-unavailable.md` |
| `AgentGateGuardrailBypassed` | P1 | Page | `sum(rate(agentgate_guardrail_decisions_total{action="bypassed"}[5m])) > 0` | `runbooks/guardrail-bypassed.md` |
| `AgentGateLoadShedding` | P2 | Page | `sum(rate(agentgate_gateway_requests_total{code="rate_limited",reason="load_shed"}[5m])) > 0` for 5m | `runbooks/load-shedding.md` |
| `AgentGateUsageStreamBuffering` | P2 | Ticket, page after 1h | `agentgate_usage_buffer_depth > 0` for 15m | `runbooks/usage-stream.md` |

`AgentGateGuardrailBypassed` is a **P1 page with a zero threshold**. Any request served without a
guardrail verdict on a pool that expected one is a compliance event, not a performance event. This is
the only alert in the catalogue with no tolerance band, and that is deliberate.

### 5.4 Telemetry trust

| Name | Severity | Page or ticket | Expression | Runbook |
|---|---|---|---|---|
| `AgentGateTelemetryDegraded` | P2 | Ticket | `agentgate_telemetry_completeness{env="prod"} < 0.95` for 15m | `runbooks/telemetry-degraded.md` |
| `AgentGateTelemetryFleetDegraded` | P2 | Page | `count(agentgate_telemetry_completeness{env="prod"} < 0.95) > 10` | `runbooks/telemetry-degraded.md` |
| `AgentGateOrphanSpans` | P3 | Ticket | `agentgate_telemetry_orphan_span_ratio > 0.05` for 15m | `runbooks/orphan-spans.md` |
| `AgentGateUnattributedSpans` | P2 | Ticket | `agentgate_telemetry_unattributed_ratio > 0.001` for 15m | `runbooks/unattributed-spans.md` |
| `AgentGateClockSkew` | P3 | Ticket | `agentgate_telemetry_clock_skew_p99_seconds > 5` for 30m | `runbooks/clock-skew.md` |
| `AgentGateCollectorQueueSaturated` | P2 | Page | `otelcol_exporter_queue_size / otelcol_exporter_queue_capacity > 0.8` for 10m | `runbooks/collector-saturated.md` |
| `AgentGateMetricCardinality` | P2 | Ticket | `count({__name__=~"agentgate_.*"}) > 120000` | `runbooks/cardinality.md` |

One agent's telemetry degrading is that team's problem and is a ticket. Ten agents degrading at once
is a pipeline problem and pages.

### 5.5 Cost

| Name | Severity | Page or ticket | Expression | Runbook |
|---|---|---|---|---|
| `AgentGateCostAnomaly` | P2 | Ticket to owning team, page FinOps at 10× | `agentgate_cost_hourly_usd > (agentgate_cost_hourly_ewma_usd + 3 * agentgate_cost_hourly_stddev_usd)` | `runbooks/cost-anomaly.md` |
| `AgentGateCostCeilingBreach` | P1 | Page | `agentgate_cost_daily_usd_by_cost_center > agentgate_cost_daily_ceiling_usd` | `runbooks/cost-ceiling.md` |
| `AgentGateMonthlyBudget80` | P3 | Ticket | `agentgate_monthly_tokens_used / agentgate_monthly_token_budget > 0.8` | `runbooks/budget-warning.md` |
| `AgentGateCacheHitRateCollapse` | P3 | Ticket | `agentgate:cache:hit_ratio_1h < 0.5 * agentgate:cache:hit_ratio_7d` | `runbooks/cache-collapse.md` |

The EWMA-plus-3σ detector catches gradual anomalies; the hard daily ceiling catches the case where
the EWMA has already absorbed the growth. Both are needed — a runaway agent that ramps over a week
never trips 3σ.

### 5.6 Security

| Name | Severity | Page or ticket | Expression | Runbook |
|---|---|---|---|---|
| `AgentGateContentCaptureEnabledInProd` | P1 | Page | `agentgate_content_capture_mode{env="prod"} != 0` | `runbooks/content-capture.md` |
| `AgentGateAuthFailureSpike` | P2 | Ticket | `sum(rate(agentgate_gateway_requests_total{code="unauthenticated"}[5m])) > 10 * avg_over_time(...[7d])` | `runbooks/auth-failures.md` |
| `AgentGateReplayDetected` | P2 | Ticket | `sum(rate(agentgate_authn_replay_rejected_total[5m])) > 0` | `runbooks/replay-detected.md` |
| `AgentGateCertificateExpiring` | P2 | Ticket at 21 d, page at 7 d | `agentgate_certificate_expiry_seconds < 21 * 86400` | `runbooks/certificate-expiry.md` |
| `AgentGateSecretRotationOverdue` | P2 | Ticket at day 88 | `agentgate_client_secret_age_days > 88` | `runbooks/secret-rotation.md` |
| `AgentGateUnpromotedVersionAttempts` | P3 | Ticket to owning team | `sum by (agent) (rate(agentgate_gateway_requests_total{code="agent_not_promoted"}[15m])) > 0` | `runbooks/unpromoted-attempts.md` |

`AgentGateContentCaptureEnabledInProd` is the third of the three independent controls described in
`06-telemetry-schema.md` §7.2. It pages because the other two failing simultaneously would mean
restricted content is being written somewhere it should not be.

---

## 6. Page versus ticket

| Page when | Ticket when |
|---|---|
| Consumers are experiencing failures now | Budget is burning slowly |
| Error budget will be exhausted within hours | The condition is degradation without impact |
| A safety control has failed open | The condition affects one agent |
| Restricted content may be leaving its boundary | Telemetry is degraded but traffic is fine |
| A whole pool has no healthy backend | A single backend's breaker is flapping |
| A cost ceiling has been breached | A cost anomaly is detected within a normal band |

**[Decision] Page budget: at most 2 pages per on-call week in steady state.** More than that is
alert-quality debt and is treated as such. The remedy is to fix the alert or fix the system, not to
route it to a channel where it is ignored.

---

## 7. On-call model

| Aspect | Model |
|---|---|
| Rota | Platform team, one primary and one secondary, weekly |
| Hours | 24×7 for P1. P2 pages during the client's business hours, ticket out of hours |
| Acknowledgement SLA | P1 5 minutes, P2 30 minutes during business hours |
| Escalation | Secondary at 10 minutes unacknowledged; platform lead at 20 minutes; client incident manager for any incident exceeding 30 minutes without mitigation |
| Handover | Written, at the start of each rota. Open incidents, error-budget position, in-flight canaries or migrations, known-degraded dependencies, anything deliberately silenced and until when |
| Agent-side incidents | Routed to the owning team's rota, from `owner.oncall` in the registration record. The platform on-call is not the responder for one agent's problem |
| Distributed team | With limited daily overlap, the follow-the-sun handover is written and asynchronous. See `11-delivery-plan.md` §7 |

**Alert routing:**

| Condition | Routes to |
|---|---|
| Platform-wide, any severity | Platform on-call |
| Scoped to one agent identity | The agent's `owner.oncall`, with the platform on-call informed but not paged |
| Cost anomaly | The owning team's on-call, plus FinOps |
| Cost ceiling breach | Platform on-call and FinOps, both paged |
| Security alerts | Platform on-call and the client's security operations function |

---

## 8. Error-budget policy

The policy exists so that reliability is a constraint on what the team may work on, not an
aspiration it may deprioritise.

### 8.1 States

| Budget remaining | State | What changes |
|---|---|---|
| > 50% | **Healthy** | Normal operation. Ship features, run experiments, take reasonable risks |
| 25–50% | **Caution** | Deploys still allowed. Canary steps require a full soak at each weight; no stacked changes. Any postmortem action item tagged `prevents-recurrence` is prioritised above new feature work |
| 10–25% | **Constrained** | Feature deploys require the platform lead's explicit approval. Reliability work takes the top of the backlog. Migration canary advancement is paused |
| < 10% | **Exhausted** | See §8.2 |
| Exhausted **and** still burning | **Freeze** | See §8.2, plus an incident is declared for the budget breach itself |

### 8.2 What the team is forbidden from doing when the budget is exhausted

Binding, not advisory:

1. **No feature deploys to the gateway.** Only changes that reduce risk: reliability fixes,
   rollbacks, and configuration changes that reduce load or blast radius.
2. **No advancement of any migration canary weight.** Weights may be reduced, never increased.
3. **No new agent onboarding to production.** Promotions to prod are paused. Staging promotions
   continue — they do not consume the gateway's budget.
4. **No new backends, pools or providers added to production configuration.** New configuration is
   new risk.
5. **No experiments in production.** No shadow load tests, no traffic replay at production volume, no
   chaos exercises.
6. **No reduction of alerting or of SLO strictness to recover the budget on paper.** Changing an
   objective to make a breach disappear is prohibited. An objective may only be changed with the
   client's service-management agreement and never retroactively.
7. **No silencing of an alert that contributed to the burn** without a documented replacement
   control.

### 8.3 What the team must do

1. Reliability work occupies the entire backlog until the budget is positive over a rolling 7-day
   window.
2. Every incident that consumed budget gets a postmortem within 5 working days, and its action items
   get owners and dates before the postmortem is closed.
3. The error-budget position is reported to the client weekly during any Constrained or Exhausted
   state, with the top three consumers of budget named.
4. Exiting Exhausted requires the budget to be positive **and** a written statement of what changed,
   reviewed by the platform lead. Waiting for the rolling window to move on its own is not exiting
   the state; it is waiting for the evidence to expire.

### 8.4 Exceptions

Exactly two:

| Exception | Conditions |
|---|---|
| **Security fix** | A change that remediates an active security issue may ship in any state, with the platform lead informed and the change recorded |
| **Reliability fix with a named hypothesis** | A change intended to stop the ongoing burn may ship, provided the hypothesis is written down before deploying and the deploy is canaried |

No other exception exists. In particular, a business deadline is not an exception; the budget is what
makes the reliability commitment real, and a commitment that yields to schedule pressure was never a
commitment.

### 8.5 Budget for a new service

**[Decision]** A newly launched component has no meaningful budget history, so for its first 28 days
it operates in Caution regardless of measured burn, and its objectives are provisional. Objectives
become binding at the first full 28-day window. This prevents both false confidence from a quiet
first week and a punitive freeze from a single launch-week incident.
