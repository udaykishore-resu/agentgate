# Postmortem Template

Blameless. The purpose is to change the system, not to establish who typed the command.

**Blameless means specific things here, not a tone of voice:**

- Name systems and decisions, not people. "The runbook did not say the failover tier was drained"
  rather than "<name> forgot to undrain it."
- Assume every person acted reasonably given what they could see. If an action looks wrong in
  hindsight, the interesting question is what made it look right at the time.
- "Human error" is never a root cause. It is the point where you stop looking too early.
- Action items land on systems, documents and automation. "Be more careful" is not an action item.
- The record is read by auditors and the client's risk function. Write plainly, state facts, and do
  not editorialise.

**Required for:** every SEV1; every SEV2 that was consumer-visible or consumed error budget; any
incident that recurs; any incident involving a control failure regardless of severity.

**Due:** five business days after resolution. A late postmortem is worth less than a rough one on
time — memory decays and evidence rotates out.

---

## Template

```markdown
# Postmortem: <short descriptive title>

| Field | Value |
|---|---|
| Incident | INC-#### |
| Severity | SEV# |
| Impact window (UTC) | start / end |
| Detection lag | impact start to alert fire |
| Time to mitigate | detection to impact end |
| Time to resolve | detection to root cause removed |
| Error budget consumed | % of the 28d gateway availability budget |
| Author | |
| Reviewers | IC, domain owner, one person not involved |
| Status | draft / in review / final |
| Date of review meeting | |

## 1. Summary
Three to five sentences. What broke, who felt it, how long, what fixed it. Written so someone with
no context understands it. This is the paragraph that gets quoted.

## 2. Impact
- Consumer impact in their terms, not ours.
- Agents, teams, tenants, pools affected, with counts.
- Requests affected, with numbers.
- Error budget consumed.
- Financial impact if any.
- Data impact: state explicitly, including when the answer is "none, confirmed by X".
- Control impact: any window where a control was not enforced, with exact UTC bounds.

## 3. Timeline
UTC. From the incident record, not from memory. Include what was looked at and rejected, not only
what was done. Mark [detect] [assess] [decide] [act] [verify] [comms] [escalate].

## 4. What happened
Narrative. The mechanism, in enough detail that a new team member could follow it. Include the
diagnostic path that did not pan out — that path is usually where the observability gap is.

## 5. Contributing factors
Not "the root cause". Incidents in a system like this have several.
- Trigger: what started it.
- Amplifiers: what made it worse or longer than it needed to be.
- Detection: why it took as long as it did to notice.
- Mitigation: what slowed the response.
- Latent: what had been true for a long time and was waiting.

## 6. What went well
Genuinely. If failover held, or a runbook was accurate, or someone escalated early, record it. This
is how good practice survives turnover.

## 7. Where we got lucky
The most valuable section and the one most often skipped. What would have made this materially
worse if it had been slightly different? Luck is a dependency you have not paid for yet.

## 8. Five whys / causal chain
Push past the first mechanical answer. Stop when you reach something you can actually change.

## 9. Action items
| ID | Action | Type | Owner | Due | Priority | Ticket |
|---|---|---|---|---|---|---|

Type: prevent / detect / mitigate / process / document.
Every action has a named individual owner and a date. Priority 1 items are tracked to completion in
the weekly platform review. An action item without a ticket does not exist.

## 10. Evidence
Links and paths: timeline export, metric range exports, log captures, audit log, vendor ticket,
change references, dashboards with the incident time range preselected.

## 11. Compliance record
Only where a control was affected. Windows in UTC, approvers, change references, and the
confirmation that every control has been restored.
```

---

# Worked example

The following is a complete, filled-in postmortem for a plausible AgentGate incident. It is a
teaching example: read the contributing factors and the "where we got lucky" section closely, they
are the parts people write badly.

---

# Postmortem: Provider region degradation exhausted the retry budget and regressed latency fleet-wide

| Field | Value |
|---|---|
| Incident | INC-1187 |
| Severity | SEV1 (declared SEV2 at 02:14Z, raised to SEV1 at 02:31Z) |
| Impact window (UTC) | 2026-08-11 02:06Z – 03:12Z (66 minutes) |
| Detection lag | 8 minutes (impact 02:06Z, page 02:14Z) |
| Time to mitigate | 44 minutes (02:14Z page, 02:58Z error ratio normal) |
| Time to resolve | 66 minutes (03:12Z, retry budget restored and verified) |
| Error budget consumed | 31% of the 28-day gateway availability budget (12m34s of 40m19s) |
| Author | Platform on-call, Shift B |
| Reviewers | IC, gateway domain owner, one engineer not on the incident |
| Status | Final |
| Date of review meeting | 2026-08-14 |

## 1. Summary

At 02:06Z the `azure-openai` `eastus` deployment began timing out on roughly 45% of requests without
returning a response body. AgentGate retried those requests as designed, but because the failures
were timeouts rather than fast errors, each retry consumed the full 20-second backend timeout before
being retried. The fleet-wide retry budget rose from a normal 2% to 11% within eight minutes,
saturating gateway concurrency. Requests to **every** pool, including pools with no dependency on
the failing provider, queued behind the exhausted concurrency and their p95 latency rose from 42ms
to 3.4 seconds. Nine consuming teams were affected, of which only two used the degraded pool. The
incident was mitigated by cutting the retry budget to 0.05 and draining the failing backend, and
resolved when the retry budget was restored to its default after the provider recovered.

## 2. Impact

**Consumer impact.** Nine of twenty-three production agent teams saw elevated latency or errors.
For seven of them the platform was slow but working; for two on the `general-chat` pool it was
returning 502 and 504. The `payments-risk` `dispute-triage` agent failed 1,840 runs, each of which
represents a dispute case that was not classified within its own SLA; that team re-ran the backlog
between 04:00Z and 06:00Z with no further failures.

**Numbers.**

| Measure | Value |
|---|---|
| Requests affected | 214,000 across all pools |
| Requests failed with 5xx | 11,900 (5.6%) |
| Requests degraded, latency above 1s | 202,100 |
| Peak error ratio | 6.1% at 02:29Z |
| Peak p95 total latency | 3.4s, baseline 1.9s |
| Peak p95 gateway overhead | 3.36s, baseline 42ms — the fleet-wide signal |
| Peak fleet retry ratio | 11.2%, cap 10%, normal 2% |
| Error budget consumed | 12m34s of 40m19s (31%) |

**Financial.** $412 of provider spend on retried requests that returned no answer to a caller. A
further $180 of increased unit cost while `general-chat` ran on the more expensive alternative
backend for six hours.

**Data impact.** None. No data loss, no exposure. Usage records reconciled exactly against gateway
request counts for the window (214,004 requests, 214,004 usage records). Failed requests returned
errors to callers and were not partially processed. Two streaming requests were terminated after
first content byte and ended with an SSE `error` frame per the contract; neither caller acted on the
partial content.

**Control impact.** None. Rate limiting, quota, guardrails and authentication all enforced normally
throughout. Guardrail decisions continued at their normal rate for the traffic that reached them.

## 3. Timeline

```
02:06Z  [impact]   azure-openai eastus begins timing out. ~45% of requests to that deployment.
                   No alert. Below the ProviderDegradation 5-minute window.
02:11Z  [detect]   ProviderDegradation fires for azure-openai/gpt-4o-mini.
02:14Z  [detect]   RetryStorm fires. Fleet retry ratio 10.4%. On-call paged.
02:15Z  [assess]   On-call ack. Declares INC-1187 SEV2. Assumes single-pool impact.
02:17Z  [observe]  Confirms azure-openai/gpt-4o-mini error ratio 44%. bedrock and onprem clean.
02:19Z  [act]      Begins provider-degradation runbook at 6.2, shifting weight to bedrock 90/10.
02:22Z  [verify]   Weight applied. general-chat error ratio falls 6.1% -> 2.8%. NOT to normal.
02:24Z  [observe]  GatewayLatencyRegression fires. p95 overhead 2.9s on long-context and
                   embeddings pools, which have NO azure backend. First sign this is not
                   confined to one pool.
02:26Z  [assess]   Checks gateway inflight: 1,940 of a 2,000 concurrency ceiling across 16 pods.
                   Concurrency is exhausted, not the pool.
02:29Z  [observe]  Peak error ratio 6.1%. Retry ratio 11.2%.
02:31Z  [escalate] Raises to SEV1. Pages L2 secondary. Assigns roles: IC, ops, comms.
02:33Z  [decide]   IC: retries are the amplifier, not the trigger. Cut the retry budget first,
                   before any further routing changes.
02:34Z  [act]      agentctl retry set-budget --ratio 0.05
02:37Z  [verify]   Retry ratio 11.2% -> 4.1%. Inflight 1,940 -> 1,120. p95 overhead 2.9s -> 310ms.
02:39Z  [act]      agentctl backend drain --pool general-chat --backend azure-openai/gpt-4o-mini
02:42Z  [verify]   Backend at 0 req/s. Error ratio 2.8% -> 0.4%.
02:44Z  [comms]    Client update 1 sent. Consuming teams notified in #agentgate-consumers.
02:46Z  [act]      Vendor bridge opened. VEN-88104. Provider confirms a regional capacity event.
02:58Z  [verify]   Error ratio 0.02%, p95 overhead 44ms. MITIGATED.
03:04Z  [observe]  Provider reports recovery. Verified independently from a gateway pod: 200 OK,
                   1.6s total, ten consecutive probes.
03:08Z  [act]      Retry budget restored to 0.25. Backend left drained pending a staged return.
03:12Z  [verify]   Retry ratio 2.1%, error ratio 0.02%, p95 overhead 41ms. RESOLVED.
09:20Z  [act]      Backend undrained and weight ramped 10 -> 30 -> 60 over 40 minutes, clean.
```

## 4. What happened

The `azure-openai` `eastus` deployment entered a capacity event that manifested as request timeouts
rather than fast errors — connections were accepted, requests were read, and no response was
returned within our 20-second per-backend timeout.

Our retry policy retries timeouts that occur before first byte, which is correct: a timeout before
first byte is safely retryable and retrying it is usually the right thing. The interaction that
caused this incident is that **a timeout consumes the full backend timeout before the retry
begins**, whereas a fast 503 consumes milliseconds. Each affected request therefore occupied a
concurrency slot for 20 seconds, then another 20 for the retry, then failed over to another backend
and occupied a slot there too. Effective concurrency cost per affected request rose roughly
fortyfold.

Gateway concurrency is a **global** ceiling, not a per-pool one. Once requests to `general-chat`
consumed it, requests to `long-context` and `embeddings` — pools with no azure backend at all —
queued for admission. Their gateway overhead, which is the SLO metric excluding provider time,
became dominated by queue wait. That is why the latency regression appeared fleet-wide while the
error regression stayed confined to one pool, and it is why the first diagnosis was wrong.

The fleet-wide retry budget cap of 10% did engage: `agentgate_retry_budget_exhausted_total`
incremented from 02:12Z. It capped retry *volume* but not retry *cost*. The budget is expressed as a
ratio of request count, and a timeout retry and a fast-error retry count identically against it
while costing forty times as much concurrency. The cap was doing exactly what it was specified to do
and the specification was insufficient for this failure shape.

The path that did not pan out, and the reason it took 17 minutes to reach the right hypothesis: the
first responder followed the provider-degradation runbook, shifted weight, and saw the error ratio
fall by half. That was genuine progress and reinforced the single-pool hypothesis. Only when
`GatewayLatencyRegression` fired for pools with no azure backend did the shared-resource nature of
the problem become visible. Nothing in the alert set said "your global concurrency is exhausted."

## 5. Contributing factors

**Trigger.** A provider regional capacity event presenting as timeouts rather than errors. Outside
our control; must be assumed to recur.

**Amplifier 1 — retry cost is not accounted for.** The retry budget counts attempts, not the
concurrency-seconds those attempts consume. A timeout-shaped failure is roughly forty times more
expensive per retry than an error-shaped one, and the budget cannot see the difference.

**Amplifier 2 — global concurrency with no per-pool reservation.** One pool's pathology consumed the
admission capacity of every other pool. The blast radius of a single-provider failure was the whole
platform.

**Amplifier 3 — no fast-fail on a saturating backend.** The circuit breaker trips on failure ratio
and consecutive failures, both of which require failures to *complete*. A backend that hangs for 20
seconds trips the breaker slowly, because each data point takes 20 seconds to produce. The breaker
opened at 02:31Z, 25 minutes after impact began.

**Detection — 8 minutes, and the wrong alert first.** `ProviderDegradation` requires a 5-minute
window and fired at 02:11Z. `RetryStorm` fired at 02:14Z. The fleet-wide latency effect, which was
the actual scope of the incident, did not alert until 02:24Z — 18 minutes after impact began. There
was no alert at all on global concurrency utilisation, which was the single most diagnostic signal
available and was visible on a dashboard nobody had open.

**Mitigation — runbook ordering.** The provider-degradation runbook lists "cap retries first if the
retry ratio is climbing" as mitigation 6.1, and that is correct. The responder went to 6.2 because
the retry ratio was 10.4% and the runbook's threshold text says "above 8%", which reads as advisory
rather than as a gate. Ordering was right; the emphasis was not.

**Latent — the load test could not have caught this.** `test/load/k6/failover.js` injects fast
failures, not hangs. No test in the suite produced a timeout-shaped backend failure, so the
concurrency amplification had never been observed before production.

## 6. What went well

- Failover worked. `bedrock` absorbed the shifted traffic without saturating and without a quality
  complaint from any consuming team.
- The IC's decision at 02:33Z to treat retries as the amplifier and cut the budget *before* further
  routing changes was the correct call and produced immediate recovery. It was made against the
  instinct to keep adjusting routing.
- Escalation to SEV1 and to L2 happened at 02:31Z, promptly, on the evidence rather than after a
  delay for certainty.
- Usage records reconciled exactly. Chargeback data was complete despite the failure rate, which
  vindicates the decision to write usage through the stream rather than synchronously.
- The vendor bridge was opened in parallel at 02:46Z rather than after mitigation, and the provider
  confirmed the event within four minutes.
- Client comms went out at 02:44Z, inside the 30-minute SEV1 commitment.

## 7. Where we got lucky

- **It happened at 02:06Z on a Tuesday.** Fleet volume was roughly 30% of the weekday peak. At
  14:00Z the same failure would have exhausted concurrency in well under two minutes and the error
  ratio would have been several times higher.
- **`bedrock` had headroom.** It was running at 38% of its concurrency cap. Had it been at 70%, the
  weight shift at 02:19Z would have saturated it and taken out the failover path as well.
- **The `onprem-vllm` failover tier was healthy and undrained.** It had been drained for maintenance
  the previous week and undrained on schedule. Had that undrain been forgotten, `general-chat` would
  have had no viable backend and the incident would have been a total pool outage.
- **No regulated-classification pool was involved.** `regulated-chat` uses a different backend set.
  Had it been affected, the same concurrency exhaustion would have caused guardrail callout timeouts
  on a `fail_closed` pool, which is a compliance event on top of an availability one.
- **Nobody tried to "fix" the latency by scaling out.** Adding pods would have added concurrency
  slots for retries to consume and lengthened the incident. It was considered at 02:26Z and rejected
  because the IC recognised the queue was downstream-bound. That was judgement, but a different
  responder could reasonably have gone the other way, and the runbook did not warn against it.

## 8. Causal chain

1. **Why did every pool slow down?** Global gateway concurrency was exhausted.
2. **Why was it exhausted?** Requests to one degraded backend occupied slots for 20 seconds per
   attempt, with retries, at roughly forty times normal concurrency cost per request.
3. **Why was that allowed to consume the whole ceiling?** Concurrency is a single global pool with
   no per-pool reservation, so one pool's pathology is every pool's problem.
4. **Why did the retry budget not contain it?** The budget caps attempt *count* as a ratio of
   requests. It has no notion of the concurrency-seconds an attempt costs, so a timeout retry and a
   fast-error retry are indistinguishable to it.
5. **Why did we not know this before?** No load test produces timeout-shaped backend failures, and
   there is no alert on global concurrency utilisation, so the amplification had never been observed
   or measured.

Stopping point: items 3, 4 and 5 are all changeable, and each is an action item below.

## 9. Action items

| ID | Action | Type | Owner | Due | Pri | Ticket |
|---|---|---|---|---|---|---|
| A1 | Add per-pool concurrency reservations so no pool can consume more than its share of the global ceiling under saturation | prevent | gateway owner | 2026-09-12 | 1 | AGP-921 |
| A2 | Make the retry budget cost-aware: account for elapsed concurrency-seconds per attempt, not attempt count | prevent | gateway owner | 2026-09-26 | 1 | AGP-922 |
| A3 | Add a fast-fail path that opens the breaker on N consecutive attempts exceeding 80% of the backend timeout, without waiting for them to complete | mitigate | gateway owner | 2026-09-19 | 1 | AGP-923 |
| A4 | Alert on global concurrency utilisation above 80% of ceiling, and add it as the first panel on the SLO dashboard | detect | observability owner | 2026-09-05 | 1 | AGP-924 |
| A5 | Add a timeout-shaped failure mode to `test/load/k6/failover.js` and assert that other pools' p95 overhead stays within SLO while one backend hangs | detect | platform | 2026-09-12 | 1 | AGP-925 |
| A6 | Restate mitigation 6.1 in `provider-degradation.md` as a gate: "if retry ratio is above 8%, cap retries before any routing change" | document | on-call author | 2026-08-21 | 2 | AGP-926 |
| A7 | Add "do not scale out to fix a downstream-bound queue" to `gateway-latency-regression.md` with the reasoning | document | on-call author | 2026-08-21 | 2 | AGP-927 |
| A8 | Reduce the `ProviderDegradation` evaluation window from 5m to 2m for timeout-class errors specifically | detect | observability owner | 2026-09-05 | 2 | AGP-928 |
| A9 | Add a per-backend concurrency utilisation panel to the pools dashboard so headroom on the failover target is visible before a weight shift | detect | observability owner | 2026-09-05 | 2 | AGP-929 |
| A10 | Game-day: timeout-shaped provider failure at production-representative volume, exercising A1–A4 | process | platform lead | 2026-10-03 | 2 | AGP-930 |

## 10. Evidence

| Artefact | Location |
|---|---|
| Incident timeline export | `incidents/INC-1187/timeline.md` |
| Error ratio and retry ratio range exports | `incidents/INC-1187/metrics/` |
| Gateway logs, 02:00Z–04:00Z | `incidents/INC-1187/logs/gateway.log.gz` |
| Audit log of all mutations | `incidents/INC-1187/audit.json` |
| Usage record reconciliation | `incidents/INC-1187/usage-reconciliation.txt` |
| Vendor ticket | VEN-88104, provider statement attached |
| Change references | CHG0045498 (retry budget), CHG0045499 (backend drain) |
| Dashboards, time range preselected | `$GRAFANA/d/agentgate-gateway-slo?from=1786?&to=1786?` |

## 11. Compliance record

No control was weakened or bypassed during this incident.

| Control | Status during window | Evidence |
|---|---|---|
| Authentication | Enforced throughout | `agentgate_identity_token_validations_total{result="fail"}` flat |
| Authorization and entitlement | Enforced throughout | No `authz.serve_stale_on_error` change made |
| Rate limiting and quota | Enforced throughout | Redis healthy, `agentgate_ratelimit_backend_info` = redis for the full window |
| Guardrails | Enforced throughout, no fail-open | `agentgate_guardrail_failmode` unchanged, decisions continuous |
| Data residency and classification routing | Enforced throughout | All failover targets within the same residency zone; verified from routing logs |
| Telemetry and usage records | Complete | 214,004 requests, 214,004 usage records, reconciled |

Two mitigations were time-boxed and both were restored and verified before resolution: the retry
budget (0.05 at 02:34Z, restored to 0.25 at 03:08Z) and the backend drain (02:39Z, lifted 09:20Z
with a staged weight ramp).

---

## Related

- [incident-response.md](incident-response.md)
- [oncall-guide.md](oncall-guide.md)
- [provider-degradation.md](provider-degradation.md)
- [retry-storm.md](retry-storm.md)
- [gateway-latency-regression.md](gateway-latency-regression.md)
- `test/load/scenarios.md` — where A5 lands
