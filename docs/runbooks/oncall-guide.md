# AgentGate On-Call Guide

The on-call model, the handover discipline that holds it together across time zones, and the rules
for when you are allowed to wake someone.

---

## 1. The on-call model

AgentGate is operated by one platform team split across two locations with roughly three hours of
daily overlap. That constraint drives everything in this document: for most of the day the person
on-call is the only person on-call, and the written record is the only thing connecting one shift
to the next.

| Rotation | Coverage | Who |
|---|---|---|
| **Primary** | 24/7, follow-the-sun, two shifts per day | Platform engineers, minimum six months on the team |
| **Secondary** | 24/7, same shift boundaries | Senior platform engineers; auto-paged on SEV1 and on any unacked page after 15 minutes |
| **Domain owner** | Business hours in their own region, best-effort out of hours | Named owner per component: gateway, controlplane, fleetview, guardrails, telemetry pipeline |
| **Incident commander pool** | On request | Anyone trained as IC; the primary can hand off command and stay hands-on |

Shift boundaries, in UTC. Both regions are named here so nobody has to do the arithmetic under
pressure:

| Shift | UTC | Region |
|---|---|---|
| A | 06:00 – 18:00 | Europe |
| B | 18:00 – 06:00 | Americas / APAC |

Handover happens at the boundary, in the overlap, live where possible and always in writing.

**Rotation length** is one week, primary and secondary rotating on different days so that both
never change at once. A single-person handover has one unfamiliar person on the incident; a
simultaneous handover has two.

**Compensation for out-of-hours work is real and taken.** An engineer paged more than twice
overnight takes the following day. This is not generosity — it is how you avoid a tired engineer
making the second incident worse than the first.

---

## 2. What the on-call owns

**Owns:**
- Every page routed to `PD-AGENTGATE-PRIMARY`, triage to mitigation.
- The decision to declare an incident and its severity.
- Keeping the incident record current — timeline, actions, decisions.
- Handover quality at the end of the shift.

**Does not own:**
- Root-cause fixes. Mitigate, record, hand to the domain owner.
- Consuming teams' agent code. Contain it, page their on-call, let them fix it.
- Anything requiring an approval they do not hold: guardrail relaxation, quota beyond a team
  envelope, promotion exceptions, break-glass credentials.
- Project work. If the shift is quiet, do game-day preparation, close postmortem actions, or improve
  a runbook. Do not start something that cannot be dropped in thirty seconds.

---

## 3. Shift start checklist

Fifteen minutes, every shift, before you do anything else. Most shifts this finds nothing, and the
one where it does is the one that pays for all the others.

```bash
export CTX=prod-eastus NS=agentgate PROM=https://prometheus.internal
```

**1. Read the outgoing handover.** Written form below. If it is missing, ask for it before the
outgoing person logs off. Do not accept "nothing happened" as a handover.

**2. Confirm you can actually respond.**

```bash
kubectl --context "$CTX" -n "$NS" get pods -l app.kubernetes.io/part-of=agentgate >/dev/null && echo "kubectl ok"
psql "$AGENTGATE_PG_URL" -tAc "select 1;" >/dev/null && echo "psql ok"
redis-cli -u "$REDIS_URL" --no-auth-warning PING
agentctl --context "$CTX" whoami
curl -sS -o /dev/null -w 'prom %{http_code}\n' "$PROM/-/healthy"
```

Broken access is discovered during an incident by everyone who does not run this check. Fix it now,
while it is a nuisance rather than a delay.

**3. Confirm the page reaches you.**

```bash
pd trigger --service PD-AGENTGATE-PRIMARY --title "shift start test $(date -u +%FT%TZ)" --urgency low
# Acknowledge and resolve it. Phone on loud, watch charged, laptop within reach.
```

**4. Error budget and SLO state.** This sets your threshold for the whole shift.

```bash
promtool query instant "$PROM" '1 - (avg_over_time(agentgate:gateway_error:ratio_rate5m[28d]) / 0.001)'
promtool query instant "$PROM" 'agentgate:gateway_overhead:p95_1m{env="prod"}'
promtool query instant "$PROM" 'agentgate:gateway_ttft:p95_5m'
promtool query instant "$PROM" 'agentgate:controlplane_token_exchange:error_ratio_rate5m'
```

Below 25% budget remaining, the next incident is a SEV1 by rule. Know that before it happens.

**5. Open alerts, silences and anything time-boxed.** Expiring TTLs are a common source of
surprise incidents at shift boundaries.

```bash
curl -sS "$PROM/api/v1/alerts" | jq -r '.data.alerts[] | "\(.labels.alertname)\t\(.labels.severity)\t\(.state)"' | sort | uniq -c
curl -sS "http://alertmanager.internal/api/v2/silences" | jq -r '.[] | select(.status.state=="active") | "\(.id)\t\(.endsAt)\t\(.comment)"'
agentctl --context "$CTX" audit list --since 24h --kind mutation --with-ttl -o json \
  | jq -r '.[] | select(.expires_at != null) | "\(.expires_at)\t\(.command)\t\(.reason)"' | sort
```

**6. Platform state.**

```bash
promtool query instant "$PROM" 'agentgate_breaker_state{state!="closed"} == 1'
agentctl --context "$CTX" backend list --drained
promtool query instant "$PROM" 'count(agentgate_telemetry_completeness{env="prod"} < 0.98)'
promtool query instant "$PROM" 'sort(agentgate_certificate_expiry_seconds / 86400) < 30'
promtool query instant "$PROM" 'agentgate_canary_weight'
```

**7. Change calendar.** What is planned during your shift, by whom, and is there a freeze?

```bash
agentctl --context "$CTX" changes upcoming --window 12h
```

**8. Open dashboards and leave them open**: `agentgate-gateway-slo`, `agentgate-pools`,
`agentgate-infra`.

---

## 4. The written handover

Written every shift, without exception, even on a quiet one. With three hours of overlap and often
none at all, this document is the continuity of the whole rotation. Post it in `#agentgate-oncall`
and link it from the rotation record.

```markdown
## AgentGate on-call handover
Shift:      B (18:00-06:00 UTC), 2026-08-26
Outgoing:   <name>            Incoming: <name>
Live handover: yes / no       If no, reply in thread to confirm you have read this.

### 1. State of the world
Error budget remaining (28d):  62%   (was 68% at shift start)
Gateway p95 overhead:          41ms
TTFT p95:                      840ms
Control plane exchange errors: 0.01%
Agents below 0.98 completeness: 2 (dispute-triage, kyc-summariser - see item 3)

### 2. Open incidents
INC-1234  SEV2  Provider degradation, azure-openai eastus
  Status:      mitigated, not resolved
  Mitigation:  weight shifted to bedrock 90/10, retry budget reduced to 0.05
  Expires:     retry budget TTL 06:30Z - IT WILL REVERT ITSELF, watch for it
  Owner:       vendor ticket VEN-88213, provider ETA 08:00Z
  Next action: at 08:00Z re-check provider p95; if recovered, ramp weight 10 -> 30 -> 60
  Do not:      restore weights in one step; it re-degraded when we tried at 02:10Z

### 3. Things that are not incidents but will page you
- kyc-summariser telemetry completeness 0.94 since their 14:00Z deploy. Their on-call
  (PD-KYC) is aware, fix expected 08:00Z. TelemetryDegraded will keep firing. Do not
  silence it, it is correct.
- CertificateExpiringSoon for onprem-inference, 18 days. Ticket AGP-901, PKI request
  submitted 2026-08-25. Nothing to do overnight.

### 4. Time-boxed changes in force  (the list that bites people)
| Change | Set by | Reason | Expires | On expiry |
|---|---|---|---|---|
| retry budget 0.05 | <name> | INC-1234 | 06:30Z | Reverts to 0.25 automatically. Watch retry ratio |
| pool general-chat weights 10/90 | <name> | INC-1234 | none | Manual restore needed |
| guardrail window 128 tokens, general-chat | <name> | INC-1198 | 09:00Z | Reverts; TTFT will rise slightly |

### 5. Silences
| Alert | Until | Why | Who set it |
|---|---|---|---|
| ProviderDegradation{backend="azure-openai/gpt-4o-mini"} | 08:00Z | INC-1234, known | <name> |

### 6. Planned changes during your shift
02:00Z  fleetview 1.9.2 rollout (low risk, owner <name>, rollback: kubectl rollout undo)
None else. Change freeze from 2026-08-29 for month-end.

### 7. What I would watch if I were you
The bedrock backend is carrying 90% of general-chat and its inflight is around 60% of
its concurrency cap. If volume rises above roughly 1.4x current, it will saturate before
the breaker notices. If that happens, promote onprem-vllm to priority 1 rather than
sending traffic back to azure.

### 8. Anything you should not do
Do not undrain azure-openai/gpt-4o-mini before the vendor confirms. It looked healthy at
01:30Z, we undrained, and it re-tripped within four minutes.
```

Sections 4, 7 and 8 are the ones that matter and the ones people skip. Section 4 exists because a
TTL expiring at 06:30 while the incoming engineer has no idea it was set is how a resolved incident
comes back. Section 8 exists because the outgoing engineer already tried the obvious thing.

**Handover rules:**

1. Written before the boundary, not at it. Draft it through the shift.
2. If there is a live incident, the handover is verbal *and* written, and the outgoing engineer
   stays until the incoming one confirms they hold the thread. An IC handover is a formal
   statement: "you are IC as of 18:07Z", acknowledged in the channel.
3. The incoming engineer replies in the thread to confirm they have read it. No reply, no handover.
4. Never hand over a mitigation whose expiry you have not written down.
5. "Quiet shift" still gets a handover, with sections 1, 4 and 6. Time-boxed changes and planned
   work outlive quiet shifts.

---

## 5. Working with limited overlap

The three-hour overlap is the team's only synchronous time. Protect it.

- **Do not use overlap for status.** Status is written. Use overlap for the things that genuinely
  need two people: an incident in progress, a decision with a real trade-off, a runbook that turned
  out to be wrong.
- **Long-running incidents get an explicit IC handover**, never an implicit one. Two people who each
  think the other is running the incident is worse than nobody running it.
- **Write for someone who cannot ask you a question.** The person reading your note may be nine
  hours away from you and awake at 04:00. Ambiguity costs them an hour.
- **Escalating across the boundary is allowed and expected.** If you need the domain owner and they
  are asleep, the rules in section 6 tell you whether to wake them. When in doubt, wake them — an
  unnecessary wake-up costs one person's sleep, and a wrong decision at 03:00 costs the client.
- **Never hold a problem for the overlap window if it is degrading production now.** "I'll ask when
  they're up" is the right instinct for a question and the wrong one for an incident.

---

## 6. You are allowed to wake someone when…

This section exists because the most common on-call failure at this team is not waking someone.
Engineers under-escalate because they do not want to be the person who woke a colleague for
nothing. Read this as explicit permission.

### Wake the secondary — always, no judgement call required

- Any SEV1.
- Any page you have not made progress on in 20 minutes.
- Any mitigation whose blast radius is larger than one backend or one agent.
- Any time you are about to do something you have not done before in production.
- Any time you are not sure whether it is a SEV1. Two people deciding it is a SEV2 is a good
  outcome; one person deciding wrongly alone is not.
- You have been working an incident for 90 minutes. Fatigue is a failure mode with a known onset.

### Wake the domain owner

- The mitigation in the runbook did not work and you need someone who knows the code path.
- The runbook is wrong, missing, or its commands do not do what it says.
- You believe there is a data-correctness problem: usage records, chargeback figures, cache
  contents, telemetry attribution.
- A control behaved incorrectly: a rate limit not enforcing, an entitlement not applied, a gate that
  passed something it should have blocked.

### Wake the security duty officer — do not wait until morning

- Any suspected cross-tenant data exposure. See
  [cache-poisoning-suspected.md](cache-poisoning-suspected.md).
- Any JWKS or signing-key incident, regardless of impact.
- Any suspected credential compromise, including an unexplained cost anomaly on an idle agent.
- Any period during which guardrails were fail-open on a `restricted` pool.
- Any period during which authentication or authorization was degraded or bypassed.
- Any request to weaken a security control in order to restore service. You do not decide that
  alone, and you should not be asked to.

### Wake risk and compliance

- Before relaxing any guardrail: failure mode, threshold, action, or an agent exemption.
- Any guardrail fail-open window on a regulated pool, at the time it happens rather than in the
  morning summary.

### Wake the platform lead (L4)

- SEV1 unmitigated at 45 minutes.
- Any decision that commits spend: emergency provider capacity, a quota beyond a team envelope.
- Any decision to deliberately deny service to a production agent — suspension, version block, or
  fail-closed admission during a Redis outage.
- Any conflict between restoring service and maintaining a control. That is a management decision
  and it is not yours to carry alone at 03:00.

### Wake the consuming team's on-call

Read from `owner.oncall` in the registry, not from memory:

```bash
psql "$AGENTGATE_PG_URL" -tAc \
  "select owner->>'team', owner->>'oncall', owner->>'email' from agents where identity = '<identity>';"
```

- Their agent is causing a platform problem: runaway loop, retry storm, telemetry flood.
- You are about to throttle, block, or suspend their agent. Call **before or at the same time**,
  never after.
- Their agent is failing because of something on our side that will not be fixed within their SLA.

### Do not wake anyone for

- A single agent hitting its own quota with isolation holding.
- A SEV3 or SEV4 ticket: certificate at 21 days, secret rotation overdue, a blocked promotion.
- Telemetry completeness degradation on one agent.
- A cost anomaly that is not a ceiling breach.
- Curiosity. Write it in the handover and ask in the overlap.

---

## 7. During an incident: the first three things

Regardless of which runbook you are in:

1. **Write down when it started.** UTC, in the incident channel, before you diagnose anything.
   Reconstructing the start time later is always harder than you expect and it drives every
   compliance timeline.
2. **Say what you are about to do, before you do it.** One line in the channel. It gives the
   secondary something to catch, and it produces a timeline for free.
3. **Look at what changed.** Deploys, config, weights, quotas, certificates, provider status.

```bash
agentctl --context "$CTX" audit list --since 6h --kind mutation
kubectl --context "$CTX" -n "$NS" rollout history deploy/gateway | tail -5
agentctl --context "$CTX" changes recent --window 6h
```

Then open the runbook named in the alert annotation and work it in order. The runbooks are ordered
by blast radius deliberately; skipping ahead is how a small incident becomes a large one.

---

## 8. Related

- [incident-response.md](incident-response.md) — declaring, roles, comms
- [postmortem-template.md](postmortem-template.md)
- [README.md](README.md) — severity definitions and the escalation ladder
- [day-2-operations.md](day-2-operations.md) — the routine changes you may be asked to make
- [operational-readiness-review.md](operational-readiness-review.md)
