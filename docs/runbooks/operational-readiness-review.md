# Operational Readiness Review

Two checklists that look similar and serve different purposes:

- **Part A** — what the AgentGate platform itself must pass before it carries production traffic,
  and re-pass at each material change. Reviewed by the platform lead, the client's architecture
  function, and security.
- **Part B** — what a consuming agent must pass before promotion to production. Largely automated by
  the promotion gate (SPEC §1.4); the manual items are the ones a machine cannot judge.

An item is either **met**, **not met**, or **exception**. There is no "partially". An exception
carries an owner, an expiry and a change reference, and is reviewed at expiry rather than quietly
extended.

---

# Part A — Platform readiness

## A1. Contract and compatibility

| # | Item | Evidence | Status |
|---|---|---|---|
| A1.1 | `api/openapi/gateway.v1.yaml` is published, frozen, and matches deployed behaviour | Contract test suite green against the deployed build | |
| A1.2 | Every error code in SPEC §2.4 is produced with the documented HTTP status, and no other status | Integration test per code | |
| A1.3 | Every response header in SPEC §2.3 is present on success **and** on error responses | Header assertion suite, including 4xx and 5xx paths | |
| A1.4 | Golden request/response corpus captured from production legacy traffic, replayed against AgentGate with byte-level diff on body, headers and error mapping | `scripts/compat-suite.sh --corpus golden/all --fail-on-diff` green | |
| A1.5 | SSE contract verified: `data:` frames, `[DONE]` terminator, `agentgate.usage` frame when requested, `agentgate.failover` only before first content byte, heartbeat every 15s | `test/load/k6/streaming.js` green | |
| A1.6 | A strict OpenAI client, unmodified, works against the gateway including streaming | Recorded demonstration with at least two SDK versions | |
| A1.7 | Compatibility rule enforced in CI: fields may be added, never removed or retyped; no status change for an existing code | Schema diff gate in the pipeline | |

## A2. SLOs, alerting and observability

| # | Item | Evidence | Status |
|---|---|---|---|
| A2.1 | Every SLO in SPEC §5 has recording rules, an error budget calculation, and a dashboard | `promtool check rules` green; dashboards listed in [README §8](README.md#8-dashboards) | |
| A2.2 | Multi-window multi-burn-rate alerting on availability and latency: 2%/1h, 5%/6h paging; 10%/3d ticketing | Rules reviewed; burn-rate maths verified against the 99.9% objective | |
| A2.3 | Every alert carries a `runbook_url` annotation pointing at a file that exists | `scripts/check-runbook-links.sh` green in CI | |
| A2.4 | Every runbook has been exercised in a game day within the last two quarters | Exercise dates recorded in each runbook's §10 | |
| A2.5 | Per-policy-stage timing is emitted, so "where did the latency go" is answerable without a code change | `agentgate_gateway_policy_duration_seconds` present for all 16 stages | |
| A2.6 | Telemetry completeness, orphan span ratio, unattributed ratio and clock skew are computed per agent per env every 60s | Fleet service metrics present and non-stale | |
| A2.7 | Alert noise measured: fewer than 2 non-actionable pages per on-call week over the last quarter | Page review from the rotation record | |
| A2.8 | Global concurrency utilisation is alerted on, not merely graphed | Rule exists and has been tested | |

## A3. Resilience

| # | Item | Evidence | Status |
|---|---|---|---|
| A3.1 | Every pool has at least two backends, and at least one at a different priority tier | `agentctl pool show --all`; no single-backend pool in prod | |
| A3.2 | Failover verified under load, including that no request returns a 5xx which should have failed over | `test/load/k6/failover.js` green at production-representative volume | |
| A3.3 | Circuit breaker verified: trips at threshold, opens for 30s, half-open admits 5 probes, closes on success | Game-day evidence, not unit tests alone | |
| A3.4 | Retry budget enforced per request (25% of deadline) and fleet-wide (10% of volume) | `test/load/k6/failover.js`, plus a timeout-shaped failure case | |
| A3.5 | Load shedding verified: `batch` priority sheds before `interactive` | `test/load/k6/spike.js` green | |
| A3.6 | Deadline propagation verified: each attempt receives `min(remaining, backend_timeout)` | Integration test with instrumented backend | |
| A3.7 | Regional failover exercised end to end within the last two quarters, with a measured recovery time | Game-day record | |
| A3.8 | Rollback for every deployable component is a weight or config change where possible, and a documented single command otherwise | Runbook §7 in every runbook | |

## A4. Identity and access

| # | Item | Evidence | Status |
|---|---|---|---|
| A4.1 | Token exchange (RFC 8693) works for every supported runtime: AKS, EKS, ACA, Lambda, SPIFFE | One verified exchange per runtime | |
| A4.2 | Every claim in SPEC §1.2 is present and validated; a token without `cost_center` cannot reach `env=prod` | Negative test in the integration suite | |
| A4.3 | JWKS rotation procedure documented, automated, and exercised without invalidating live tokens | Game-day record; see [jwks-rotation-failure.md](jwks-rotation-failure.md) | |
| A4.4 | JTI replay window enforced and its storage failure mode defined and tested | Replay attempt rejected in test | |
| A4.5 | Client-credential secrets rotate on a 90-day clock, with overlap, never returned twice | Rotation exercised on a real agent | |
| A4.6 | Percentage of production agents using federated workload identity is recorded, with a target and a trend | Fleet report | |
| A4.7 | The break-glass credential path exists, is sealed, requires two-party approval, and has been tested | Security-owned record | |
| A4.8 | No path exists to disable token validation at the gateway via configuration | Code review sign-off; this must be structurally impossible, not merely discouraged | |

## A5. Data protection and residency

| # | Item | Evidence | Status |
|---|---|---|---|
| A5.1 | Cache keys include tenant, structurally and unavoidably; cross-tenant isolation proven under load | `test/load/k6/multitenant.js` asserts it; see [cache-poisoning-suspected.md](cache-poisoning-suspected.md) | |
| A5.2 | Semantic cache is off by default and requires an explicit data-classification allowance to enable | Config default verified; enablement path requires the allowance | |
| A5.3 | Prompt and completion content is off spans by default in prod; the content pipeline is separate, access-controlled and has its own retention | `AGENTGATE_CAPTURE_CONTENT=off` in prod; pipeline reviewed by security | |
| A5.4 | Classification and residency filters are applied at backend selection, and a restricted request cannot route to a non-compliant backend | Negative test; the filter must fail closed | |
| A5.5 | Every network path in SPEC §8 has its diagram, FQDNs, ports, data classification and review artefact | Security review pack complete | |
| A5.6 | Guardrail failure mode is `fail_closed` by default for `data_classification=restricted` | Config default verified per pool | |
| A5.7 | Chargeback and usage records contain no prompt or completion content | Schema review | |

## A6. State and dependencies

| # | Item | Evidence | Status |
|---|---|---|---|
| A6.1 | Redis failure mode is explicitly configured — fail-open or fail-closed — and the choice is recorded with its rationale | `ratelimit.on_backend_failure` in the committed config | |
| A6.2 | Redis failover exercised under load, in both admission modes | Game-day record | |
| A6.3 | Postgres HA verified; the gateway's authz cache runway measured and documented | Game-day record; runway figure in [postgres-failover.md](postgres-failover.md) | |
| A6.4 | Connection pool sizes across all services sum to less than `max_connections` with at least 30% headroom | Arithmetic recorded in `test/load/capacity-model.md` | |
| A6.5 | Usage stream retention exceeds the maximum tolerable Postgres outage, with the margin stated | Retention setting and the arithmetic | |
| A6.6 | Collector pool sized against measured spans per request at target volume | `test/load/capacity-model.md` | |
| A6.7 | Backup and restore of the registry tested, with a measured restore time | Restore drill record | |

## A7. Capacity

| # | Item | Evidence | Status |
|---|---|---|---|
| A7.1 | Capacity model exists with shown arithmetic and a sizing table for 100 / 1,000 / 10,000 rpm | `test/load/capacity-model.md` | |
| A7.2 | Baseline load test passes SLO thresholds at expected production volume | `test/load/k6/baseline.js` green | |
| A7.3 | The knee is known: the concurrency at which p95 breaches, measured not estimated | `test/load/k6/ramp.js` result recorded | |
| A7.4 | 4-hour soak shows no memory growth trend and no latency drift | `test/load/k6/soak.js` green | |
| A7.5 | 10x spike handled with load shedding rather than collapse | `test/load/k6/spike.js` green | |
| A7.6 | Headroom policy stated: run at no more than X% of the measured knee, with X justified | Written and agreed with the platform lead | |
| A7.7 | Per-stream memory measured, and streaming concurrency ceiling derived from it | `test/load/capacity-model.md` | |

## A8. Operations

| # | Item | Evidence | Status |
|---|---|---|---|
| A8.1 | On-call rotation staffed with primary and secondary, 24/7, with follow-the-sun handover | Rotation record | |
| A8.2 | Every alert has a runbook; every runbook has been read by every on-call engineer | Sign-off list | |
| A8.3 | Incident response process documented, with roles and comms cadence | [incident-response.md](incident-response.md) | |
| A8.4 | Client-facing comms templates approved by the client's incident manager in advance | Approval on file | |
| A8.5 | Day-2 operations documented for every routine change | [day-2-operations.md](day-2-operations.md) | |
| A8.6 | Zero-downtime upgrade demonstrated under load | Game-day record | |
| A8.7 | Every `agentctl` mutation is audited with actor, reason and timestamp | Audit log verified | |
| A8.8 | Postmortem process defined, blameless, with action items tracked to completion | [postmortem-template.md](postmortem-template.md) and the tracking board | |

## A9. Migration readiness (SPEC §7)

| # | Item | Evidence | Status |
|---|---|---|---|
| A9.1 | Legacy contract frozen and published as `gateway.v1.yaml` | Published | |
| A9.2 | Compatibility suite green on the full golden corpus | CI | |
| A9.3 | Shadow mode running with diffs reported and triaged to zero | Diff report | |
| A9.4 | Canary weights controllable per consumer at the front door, with rollback as a weight change | Demonstrated | |
| A9.5 | Automatic rollback on SLO burn configured and tested | Game-day record | |
| A9.6 | Decommission criterion agreed: 30 days at 100% with zero contract diffs | Written and agreed | |

## Platform sign-off

| Role | Name | Date | Decision |
|---|---|---|---|
| Platform engineering lead | | | |
| Security | | | |
| Risk and compliance | | | |
| Client architecture | | | |
| Client incident manager | | | |

Re-review is required on: a change to the wire contract, a new provider or backend type, a change to
the identity model, a change to guardrail defaults, a new region, or any SEV1 whose postmortem
identifies a readiness gap.

---

# Part B — Consuming agent readiness

Most of this is evaluated automatically by the promotion gate (SPEC §1.4). Check the gate first:

```bash
agentctl promotion status --agent agent://fsclient/payments-risk/dispute-triage \
  --version 2.5.0 --env prod -o json | jq '.gates'
```

## B1. Automated gates — must all pass

| Gate | Rule | Verify |
|---|---|---|
| `registration_complete` | Owner, on-call, cost centre, data classification present | `psql -c "select owner, data_classification from agents where identity='...'"` |
| `identity_attested` | Authenticated at least once with `attestation=workload-identity` in the source env | `sum by (attestation) (increase(agentgate_controlplane_token_exchange_total{agent_id="..."}[7d]))` |
| `telemetry_healthy` | ≥95% complete, correctly-attributed traces over 24h, ≥100 requests observed | `agentgate_telemetry_completeness{agent="...",env="staging"}` |
| `error_budget` | The agent's own success SLI ≥ its objective over 7d in the source env | See [promotion-gate-blocked.md](promotion-gate-blocked.md) §4 |
| `guardrail_clean` | No unresolved critical guardrail violations in 7d | `increase(agentgate_guardrail_decisions_total{agent="...",action="block"}[7d])` |
| `quota_declared` | Requested quota ≤ the team's envelope, or an attached exception | `agentctl promotion projection` |
| `cost_projection` | Projected monthly spend within the team's budget, or an attached exception | `agentctl promotion projection` |
| `security_review` | Prod only: a linked, non-expired ServiceNow CHG/RITM reference | `select security_review_ref, security_review_expires_at from agent_versions ...` |

## B2. Human approval — prod only

Two parties, neither of whom is the requester: one owning-team approver and one platform approver
(SPEC §1.4). Recorded with actor, timestamp and the evaluated gate snapshot.

```bash
agentctl promotion status --agent agent://... --version 2.5.0 --env prod -o json \
  | jq '{approvals, gate_snapshot_hash}'
```

## B3. What a machine cannot judge — the platform approver's checklist

These are the questions the platform approver actually has to think about. They are the reason human
approval exists at all.

**Ownership and support**

- [ ] The named on-call rota is real and staffed. Test it: page it once, in business hours, with
      warning, and confirm a human answers.
- [ ] The owning team knows they will be called if their agent misbehaves at 03:00, and has agreed.
- [ ] There is a named person, not only a team alias, who understands what this agent does.

**Behaviour under failure**

- [ ] The agent handles `429` with `Retry-After` by waiting, not by retrying immediately.
- [ ] Retries use exponential backoff **with jitter**. Ask to see the code. A client without jitter
      will eventually cause a retry storm and this is the only point at which it is cheap to fix.
- [ ] The agent has a bounded step or iteration count. An unbounded plan loop is the mechanism in
      [runaway-agent.md](runaway-agent.md).
- [ ] Streaming callers validate the `[DONE]` terminator and do not treat a truncated stream as a
      complete response.
- [ ] The agent handles `403 guardrail_blocked` as a business outcome, not as a transient error to
      retry.

**Resource declarations**

- [ ] `max_tokens` is realistic, not a copied-in ceiling. Quota reservation uses the requested value
      (SPEC §3.4), so an agent asking for 4096 and using 200 consumes 20x its real need.
- [ ] Requested quota is derived from measured staging behaviour, with the measurement shown.
- [ ] The projected monthly cost has been seen and acknowledged by whoever owns the cost centre.
- [ ] Batch workloads set `x-agentgate-request-priority: batch`. An interactive-priority batch job
      is a load-shedding problem waiting to happen.

**Telemetry**

- [ ] Resource attributes are set correctly: `service.name`, `service.version`,
      `service.namespace`, `deployment.environment.name`.
- [ ] The agent does **not** set `agentgate.*` ownership attributes itself. The gateway stamps them
      from the verified token; an agent setting them produces correction events and confusion.
- [ ] `x-agentgate-session-id` is set where the agent has a session concept, so multi-step runs are
      correlatable.
- [ ] Trace context is propagated: the agent continues `traceparent` rather than starting fresh.

**Data handling**

- [ ] The declared `data_classification` matches what the agent actually sends. Ask for an example.
- [ ] If the classification is `restricted`, the agent is on a `fail_closed` guardrail pool and its
      team understands what that means when the guardrail service is down: their agent stops.
- [ ] The agent does not log prompts or completions in its own logs, which sit outside our
      access-controlled content pipeline.

**Operational history**

- [ ] The agent has run in staging for long enough to have a meaningful error budget history, not
      merely long enough to satisfy the 100-request minimum.
- [ ] Any prior incident involving this agent has closed action items.
- [ ] The version being promoted is the version that was tested, byte for byte.

## B4. Exceptions

`quota_declared` and `cost_projection` may carry an attached exception approved by the budget owner.
The remaining gates may not be exempted by an on-call engineer under any circumstances — see
[promotion-gate-blocked.md §6.4](promotion-gate-blocked.md#64-grant-a-time-boxed-recorded-exception-blast-radius-governance--approval-required)
for the list and the reasoning.

## B5. Post-promotion — first 48 hours

The owning team, not the platform team, watches:

```bash
export AGENT='agent://fsclient/payments-risk/dispute-triage'
promtool query instant "$PROM" 'agentgate_telemetry_completeness{agent="'"$AGENT"'",env="prod"}'
promtool query instant "$PROM" 'sum by (code) (rate(agentgate_gateway_requests_total{agent="'"$AGENT"'",env="prod"}[5m]))'
promtool query instant "$PROM" 'sum(increase(agentgate_gateway_cost_usd_total{agent="'"$AGENT"'",env="prod"}[24h]))'
promtool query instant "$PROM" 'sum by (decision) (rate(agentgate_ratelimit_decisions_total{agent="'"$AGENT"'"}[5m]))'
```

Compare day-one cost against the projection. A cost projection that is wrong by more than 2x is a
finding against the gate, not merely against the agent — feed it back so the projection improves.

---

## Related

- [promotion-gate-blocked.md](promotion-gate-blocked.md)
- [day-2-operations.md](day-2-operations.md)
- [incident-response.md](incident-response.md)
- `test/load/README.md` — the capacity evidence behind Part A section A7
- `test/load/capacity-model.md`
