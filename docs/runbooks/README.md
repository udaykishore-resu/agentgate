# AgentGate Runbooks

Operational documentation for the AgentGate control plane. Every alert that can page a human
links to exactly one file in this directory via its `runbook_url` annotation. An alert without a
runbook is a bug in the alert, not a gap in the docs — see [Rules](#rules-for-alerts-and-runbooks).

Written for the person who was asleep ninety seconds ago. Commands are copy-pasteable. Read the
[first 5 minutes](#the-runbook-template) section, do that, then think.

---

## 1. Index

### SLO burn and traffic-plane availability

| Runbook | Alert | Sev |
|---|---|---|
| [gateway-availability-burn.md](gateway-availability-burn.md) | `GatewayAvailabilityBurnFast` / `GatewayAvailabilityBurnSlow` | SEV1 / SEV3 |
| [gateway-latency-regression.md](gateway-latency-regression.md) | `GatewayLatencyRegression` | SEV2 |
| [gateway-ttft-regression.md](gateway-ttft-regression.md) | `GatewayTTFTRegression` | SEV2 |
| [no-healthy-backend.md](no-healthy-backend.md) | `NoHealthyBackend` | SEV1 |
| [circuit-breaker-open.md](circuit-breaker-open.md) | `CircuitBreakerOpen` | SEV2 |
| [provider-degradation.md](provider-degradation.md) | `ProviderDegradation` | SEV2 |
| [retry-storm.md](retry-storm.md) | `RetryStorm` | SEV1 |
| [streaming-stalls.md](streaming-stalls.md) | `StreamingStalls` | SEV2 |

### Admission control, quota, cost

| Runbook | Alert | Sev |
|---|---|---|
| [quota-saturation.md](quota-saturation.md) | `QuotaSaturation` | SEV3 |
| [rate-limit-misconfiguration.md](rate-limit-misconfiguration.md) | `RateLimitMisconfiguration` | SEV2 |
| [cost-anomaly.md](cost-anomaly.md) | `CostAnomaly` / `CostCenterDailyCeilingBreached` | SEV3 / SEV2 |
| [runaway-agent.md](runaway-agent.md) | `RunawayAgent` | SEV2 |

### Telemetry plane (the "truth" face)

| Runbook | Alert | Sev |
|---|---|---|
| [telemetry-incomplete.md](telemetry-incomplete.md) | `TelemetryDegraded` | SEV3 |
| [trace-attribution-broken.md](trace-attribution-broken.md) | `TraceAttributionBroken` | SEV3 |
| [collector-backpressure.md](collector-backpressure.md) | `CollectorBackpressure` | SEV2 |

### Guardrails

| Runbook | Alert | Sev |
|---|---|---|
| [guardrail-service-down.md](guardrail-service-down.md) | `GuardrailServiceDown` | SEV1 (fail_closed pools) / SEV2 |
| [guardrail-false-positive-spike.md](guardrail-false-positive-spike.md) | `GuardrailFalsePositiveSpike` | SEV2 |

### Identity, secrets, crypto (the "trust" face)

| Runbook | Alert | Sev |
|---|---|---|
| [controlplane-token-exchange-failures.md](controlplane-token-exchange-failures.md) | `ControlPlaneTokenExchangeFailures` | SEV1 |
| [jwks-rotation-failure.md](jwks-rotation-failure.md) | `JWKSRotationFailure` | SEV1 |
| [secret-rotation-overdue.md](secret-rotation-overdue.md) | `SecretRotationOverdue` | SEV4 |
| [certificate-expiry.md](certificate-expiry.md) | `CertificateExpiringSoon` | SEV3 |

### State and dependencies

| Runbook | Alert | Sev |
|---|---|---|
| [redis-unavailable.md](redis-unavailable.md) | `RedisUnavailable` | SEV1 |
| [postgres-failover.md](postgres-failover.md) | `PostgresFailover` | SEV2 |
| [cache-poisoning-suspected.md](cache-poisoning-suspected.md) | `CachePoisoningSuspected` | SEV1 |

### Delivery and lifecycle

| Runbook | Alert | Sev |
|---|---|---|
| [promotion-gate-blocked.md](promotion-gate-blocked.md) | `PromotionGateBlocked` | SEV4 |
| [migration-canary-rollback.md](migration-canary-rollback.md) | `MigrationCanaryRollback` | SEV2 |

### Process documents

| Document | Purpose |
|---|---|
| [oncall-guide.md](oncall-guide.md) | Rotation model, follow-the-sun handover, wake-up rules |
| [incident-response.md](incident-response.md) | Declaring, roles, comms cadence, regulated-environment templates |
| [postmortem-template.md](postmortem-template.md) | Blameless template plus a worked example |
| [operational-readiness-review.md](operational-readiness-review.md) | Platform ORR and consuming-agent promotion checklist |
| [day-2-operations.md](day-2-operations.md) | Routine changes: pools, tenants, quotas, keys, upgrades, drains |

---

## 2. The runbook template

Every alert runbook in this directory has these ten sections, in this order. Do not reorder them;
the on-call reads them positionally at 03:00.

1. **Alert** — name, expression, severity, page or ticket.
2. **What this means** — one paragraph, plain language.
3. **Impact** — who is affected and how they experience it.
4. **First 5 minutes** — ordered, copy-pasteable triage.
5. **Diagnosis decision tree** — Mermaid `flowchart TD`.
6. **Mitigations** — ordered by blast radius, smallest first. Command, expected effect, verification.
7. **Rollback** — how to undo each mitigation.
8. **Escalation** — when and to whom.
9. **Post-incident** — what to capture while it is fresh.
10. **Related** — other runbooks and dashboards.

A new runbook starts as a copy of the closest existing one. Do not start from a blank file.

---

## 3. Severity definitions

Severity is set from **observed impact**, not from the alert that fired. An alert may fire at SEV2
and be raised to SEV1 in the first five minutes; that is normal and expected.

| Sev | Definition | Response | Ack | IC assigned | Client comms |
|---|---|---|---|---|---|
| **SEV1** | Core capability lost or materially degraded for production agents across more than one tenant or team. Examples: gateway returning 5xx fleet-wide, all backends open-circuit, token exchange failing, Redis unavailable, suspected cache cross-contamination. | Page primary and secondary immediately | 5 min | 10 min | 30 min, then every 30 min |
| **SEV2** | Partial loss: one pool, one tenant, one provider, or an SLO burning fast enough to exhaust the 28d budget inside a week. Examples: one backend open-circuit with failover holding, TTFT regression, guardrail false-positive spike. | Page primary | 15 min | 30 min if not mitigated | 60 min if consumer-visible |
| **SEV3** | Degraded but no immediate consumer-visible impact, or impact confined to a single agent. Examples: telemetry completeness below objective, quota saturation on one agent, certificate 21 days out. | Ticket, worked in business hours | 4 business hours | Not required | Only if the consuming team asks |
| **SEV4** | Hygiene and lifecycle. Examples: secret rotation overdue, promotion gate blocked. | Ticket | 5 business days | Not required | No |

**Error budget rule.** Gateway availability has 40m19s of budget per 28 days. Any single incident
that consumes more than 25% of the remaining budget is a SEV1 regardless of duration, and forces a
postmortem. Latency and TTFT SLOs are "99% of minutes" objectives; three consecutive days of
breach is a SEV2 even if no single day would have paged.

---

## 4. Escalation ladder

```
L0  Alert                       → PagerDuty service PD-AGENTGATE-PRIMARY
L1  Primary on-call             → owns triage and mitigation for 30 min
L2  Secondary on-call           → PD-AGENTGATE-SECONDARY, auto-paged on SEV1 or 15 min unacked
L3  Domain owner                → gateway / controlplane / fleetview owner, see OWNERS in each dir
L4  Platform engineering lead   → SEV1 not mitigated at 45 min, or any decision that reduces safety
L5  Client incident manager     → SEV1 with consumer impact, or any regulatory-reportable event
L6  Duty executive              → SEV1 past 2h, or client-declared major incident
```

Sideways escalations, which are not "up" but are often the right call:

| Situation | Go to |
|---|---|
| One provider degraded in one region | Provider TAM via the vendor bridge, `#vendor-escalation` |
| Suspected data-classification or residency breach | Security duty officer, immediately, before mitigating |
| Guardrail policy decision under pressure | Risk and compliance duty contact — an on-call engineer may not unilaterally weaken `fail_closed` |
| Agent behaving badly, owned by a consuming team | Team's own on-call from the registration record `owner.oncall` |

The consuming team's on-call is in the registry, not in your head:

```bash
psql "$AGENTGATE_PG_URL" -tAc \
  "select display_name, owner->>'team', owner->>'email', owner->>'oncall'
     from agents where identity = 'agent://fsclient/payments-risk/dispute-triage';"
```

---

## 5. Rules for alerts and runbooks

1. **Every alert links to a runbook.** The Prometheus rule must carry
   `annotations.runbook_url: https://docs.internal/agentgate/runbooks/<file>.md`. CI fails the
   rules bundle if any alert lacks the annotation or points at a file that does not exist:

   ```bash
   promtool check rules deploy/prometheus/rules/*.yaml
   scripts/check-runbook-links.sh deploy/prometheus/rules/ docs/runbooks/
   ```

2. **Every runbook names its alert exactly.** The `## 1. Alert` heading carries the literal alert
   name and the literal expression from the rules file. If you change the expression, change the
   runbook in the same pull request.

3. **Every mitigation has a rollback.** If you cannot write the rollback, it is not a mitigation,
   it is an experiment, and it needs a second pair of eyes before you run it in production.

4. **No mitigation in a runbook may weaken a control silently.** Disabling guardrails, widening a
   quota beyond the team envelope, or bypassing the promotion gate requires the approval named in
   the runbook and a recorded change reference.

5. **Runbooks are tested.** Each one is exercised at least once per quarter in a game day, and the
   date of last exercise is recorded in `## 10. Related`. A runbook not exercised in two quarters
   is presumed stale.

---

## 6. Environment conventions used throughout

Export these before working through any runbook. Every command below assumes them.

```bash
export CTX=prod-eastus                      # kubectl context; prod-westus is the paired region
export NS=agentgate                         # namespace for all AgentGate workloads
export PROM=https://prometheus.internal
export GRAFANA=https://grafana.internal
export GW=https://gateway.agentgate.internal
export CP=https://controlplane.agentgate.internal
export FLEET=https://fleetview.agentgate.internal
export AGENTGATE_PG_URL="postgresql://agentgate_ro@pg-agentgate-prod.postgres.internal:5432/agentgate?sslmode=verify-full"
export REDIS_URL="rediss://:$(kubectl --context $CTX -n $NS get secret redis-auth -o jsonpath='{.data.password}' | base64 -d)@redis-agentgate-prod.redis.internal:6380"
```

Workloads:

| Deployment | Ports | Notes |
|---|---|---|
| `gateway` | 8080 traffic, 9090 metrics | HPA on concurrency, PDB minAvailable 75% |
| `controlplane` | 8081 traffic, 9091 metrics | Issues tokens, owns registry and promotion |
| `fleetview` | 8082 traffic, 9092 metrics | Completeness, SLO, chargeback API |
| `guardrails` | 8083 | Callout service; also fronted by a managed content-safety provider |
| `mockprovider` | 8090 | Non-prod and load testing only. Must not exist in prod. |
| `otel-collector-gateway` | 4317 OTLP, 8888 metrics | HA pool, tail sampling |

A read-only shell that is safe to use during an incident:

```bash
kubectl --context "$CTX" -n "$NS" get pods -l app.kubernetes.io/part-of=agentgate -o wide
kubectl --context "$CTX" -n "$NS" logs deploy/gateway --since=10m --tail=200 | jq -c 'select(.level=="error")'
```

---

## 7. Metric reference

SPEC §4.3 names metrics in OpenTelemetry dotted form. Prometheus receives them through the
collector's Prometheus exporter, which applies the standard normalisation: `.` becomes `_`,
counters gain `_total`, histograms gain a unit suffix and `_bucket` / `_sum` / `_count` series.
Use the Prometheus form in PromQL. This is the mapping — it is the single source of truth for
every expression in these runbooks.

| SPEC §4.3 name | Prometheus series | Labels |
|---|---|---|
| `agentgate.gateway.requests` | `agentgate_gateway_requests_total` | `tenant,team,agent,env,pool,backend,status,code` |
| `agentgate.gateway.duration` | `agentgate_gateway_duration_seconds_{bucket,sum,count}` | above `+ stream, phase` |
| `agentgate.gateway.ttft` | `agentgate_gateway_ttft_seconds_{bucket,sum,count}` | `pool,backend` |
| `agentgate.gateway.tokens` | `agentgate_gateway_tokens_total` | above `+ direction` |
| `agentgate.gateway.cost_usd` | `agentgate_gateway_cost_usd_total` | `tenant,team,agent,env,cost_center,backend` |
| `agentgate.gateway.inflight` | `agentgate_gateway_inflight` | `pool` |
| `agentgate.ratelimit.decisions` | `agentgate_ratelimit_decisions_total` | `agent,decision` |
| `agentgate.cache.lookups` | `agentgate_cache_lookups_total` | `pool,result` |
| `agentgate.guardrail.decisions` | `agentgate_guardrail_decisions_total` | `pool,category,action` |
| `agentgate.breaker.state` | `agentgate_breaker_state` | `backend,state` |
| `agentgate.retry.attempts` | `agentgate_retry_attempts_total` | `backend,reason` |
| `agentgate.fleet.agents` | `agentgate_fleet_agents` | `env,state` |
| `agentgate.telemetry.completeness` | `agentgate_telemetry_completeness` | `agent,env` |

Two label conventions matter and are easy to get wrong:

- `status` is the **HTTP status code** as a string (`"200"`, `"429"`, `"503"`). `code` is the
  **stable error code** from SPEC §2.4 (`quota_exceeded`, `no_healthy_backend`, …), empty on
  success. Availability is defined on both: a `503 no_healthy_backend` is excluded from the
  numerator *and* is the thing `NoHealthyBackend` alerts on separately.
- `phase` on `agentgate_gateway_duration_seconds` is `total` (wall clock, includes provider time)
  or `overhead` (everything AgentGate did, excluding time spent inside the provider call). The
  latency SLO in SPEC §5 is defined on `phase="overhead"`. Comparing the wrong one is the single
  most common misreading during a latency incident.

Supporting series emitted by the platform beyond the SPEC §4.3 headline table. These are the
mechanics behind SPEC §4.5, §5 and §8 and are used by the runbooks:

```
agentgate_telemetry_orphan_span_ratio{agent,env}
agentgate_telemetry_unattributed_ratio{agent,env}
agentgate_telemetry_clock_skew_seconds{agent,env,quantile}
agentgate_controlplane_token_exchange_total{result,mode}
agentgate_controlplane_token_exchange_duration_seconds_bucket{mode}
agentgate_controlplane_jwks_refresh_total{result}
agentgate_controlplane_jwks_key_age_seconds{kid}
agentgate_identity_token_validations_total{result,reason}
agentgate_guardrail_callout_duration_seconds_bucket{provider}
agentgate_guardrail_callout_failures_total{provider,reason}
agentgate_guardrail_failmode{pool,mode}
agentgate_gateway_shed_total{priority,reason}
agentgate_stream_intertoken_seconds_bucket{pool,backend}
agentgate_stream_stalls_total{pool,backend}
agentgate_cache_semantic_similarity_bucket{pool}
agentgate_secret_age_seconds{agent_id,kind}
agentgate_certificate_expiry_seconds{host,issuer}
agentgate_promotion_gate_evaluations_total{gate,result,env}
agentgate_usage_records_written_total{result}
agentgate_canary_weight{consumer}
```

Recording rules that the SLO alerts are built on live in
`deploy/prometheus/rules/agentgate-slo.rules.yaml`:

```
agentgate:gateway_availability:ratio_rate5m
agentgate:gateway_error:ratio_rate{5m,30m,1h,6h,3d}
agentgate:gateway_overhead:p95_1m
agentgate:gateway_ttft:p95_5m
agentgate:controlplane_token_exchange:error_ratio_rate5m
agentgate:cost:ewma_1h_by_agent
```

---

## 8. Dashboards

| Dashboard | UID | Opens with |
|---|---|---|
| Gateway SLO and error budget | `agentgate-gateway-slo` | `$GRAFANA/d/agentgate-gateway-slo` |
| Gateway latency breakdown by policy stage | `agentgate-gateway-latency` | `$GRAFANA/d/agentgate-gateway-latency` |
| Pools, backends and breakers | `agentgate-pools` | `$GRAFANA/d/agentgate-pools` |
| Streaming health, TTFT and inter-token | `agentgate-streaming` | `$GRAFANA/d/agentgate-streaming` |
| Telemetry trust | `agentgate-telemetry-trust` | `$GRAFANA/d/agentgate-telemetry-trust` |
| Cost and chargeback | `agentgate-cost` | `$GRAFANA/d/agentgate-cost` |
| Control plane and identity | `agentgate-controlplane` | `$GRAFANA/d/agentgate-controlplane` |
| Guardrails | `agentgate-guardrails` | `$GRAFANA/d/agentgate-guardrails` |
| Dependencies: Redis, Postgres, collectors | `agentgate-infra` | `$GRAFANA/d/agentgate-infra` |
| Migration canary | `agentgate-migration` | `$GRAFANA/d/agentgate-migration` |

Every dashboard has a variable `env` (default `prod`) and `tenant` (default `fsclient`). Set them
before you read a number, because a graph filtered to staging looks exactly like a healthy prod.
