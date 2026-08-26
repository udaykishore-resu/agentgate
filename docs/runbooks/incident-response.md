# AgentGate Incident Response

How to declare an incident, who does what, how often you communicate and to whom, and the timeline
discipline that makes the record defensible afterwards.

This is a regulated financial-services environment. The record you produce during an incident is
read later by people who were not there — auditors, the client's risk function, sometimes a
regulator. Write it accordingly, while it is happening.

---

## 1. Declaring an incident

Declare when **any** of these is true. Do not deliberate.

- A page has fired and you cannot mitigate it in the first five minutes.
- Consumer-visible impact exists, whatever the alerts say. A consuming team reporting a problem is
  sufficient grounds on its own.
- More than one alert is firing and you suspect they share a cause.
- Any suspected security, data-disclosure, or control failure — regardless of availability impact.
- You are about to take an action whose blast radius exceeds one backend or one agent.
- You are not sure. Declaring costs a channel and five minutes. Not declaring costs the timeline.

**Declaring is cheap. Un-declaring is trivial. Reconstructing an undeclared incident is not.**

```bash
agentctl incident declare --severity sev2 \
  --title "Elevated 5xx on general-chat pool, azure-openai eastus" \
  --detected-at "$(date -u +%FT%TZ)" --detected-by alert \
  --alert GatewayAvailabilityBurnFast
# Creates INC-1234, opens #inc-1234, creates the timeline record, notifies the on-call channel.
```

If tooling is unavailable, declare manually and keep the same fields — an incident with a
hand-written record is fine, an incident with no record is not:

```
INC-<n> declared <UTC timestamp>
Severity: <sev>
Title: <one line>
Detected by: <alert name | consuming team | internal observation>
Detected at: <UTC - when impact began, not when you noticed>
IC: <name>
```

Severity comes from [README §3](README.md#3-severity-definitions). Set it on observed impact, not
on the alert that fired, and raise it freely — raising severity is normal and carries no stigma.

---

## 2. Roles

For SEV3 and SEV4 one person holds all roles. For SEV1 and SEV2 they separate, and the separation is
the point: the person typing commands cannot also be the person writing updates and thinking about
what happens in twenty minutes.

### Incident Commander (IC)

Owns the incident. **Does not type commands.** The moment the IC starts debugging, the incident has
no commander.

- Sets and re-evaluates severity.
- Decides what is done and in what order; explicitly approves anything with wide blast radius.
- Assigns roles by name, out loud: "Priya is ops, Sam is comms."
- Keeps the timeline current.
- Calls the escalations.
- Declares mitigation and resolution, and says so explicitly so everyone stops.

The first responder is IC by default until they hand it over. Handing over is a formal statement:
"you are IC as of 03:14Z", acknowledged.

### Operations (ops)

Executes. One person types in production during an incident; others read and advise. Announces each
action **before** running it, and reports the result. Refuses any action they believe is unsafe and
says why — the IC decides, but the ops engineer's hands are their own.

### Communications (comms)

Owns everything leaving the incident channel: internal updates, consuming-team notifications,
client-facing updates on the agreed cadence. Protects the IC from stakeholder questions. In a
regulated environment this role is not optional at SEV1 — the comms cadence is a commitment, and
somebody must own it while everyone else is busy.

### Scribe

Only for SEV1, or SEV2 running past an hour. Records every action, decision, timestamp, and query
result in the channel. Frees the IC to think. If nobody is available, the IC scribes and accepts
that they will do it imperfectly.

### Subject-matter experts

Pulled in as needed, released when done. The IC says explicitly when someone is released — an SME
sitting silently in a bridge for two hours is a wasted person and a tired one.

---

## 3. Severity matrix

| | SEV1 | SEV2 | SEV3 | SEV4 |
|---|---|---|---|---|
| **Trigger** | Core capability lost across tenants or teams; suspected data disclosure; identity plane down | Partial loss — one pool, one tenant, one provider; SLO burning fast | Degraded, no immediate consumer impact; single agent affected | Hygiene and lifecycle |
| **Examples** | Fleet-wide 5xx, `NoHealthyBackend`, token exchange failing, Redis down, cache contamination | `CircuitBreakerOpen` with failover holding, TTFT regression, guardrail false positives | Telemetry completeness below objective, quota saturation, certificate at 21d | Secret rotation overdue, promotion blocked |
| **Ack** | 5 min | 15 min | 4 business hours | 5 business days |
| **IC** | Required, within 10 min | Required if unmitigated at 30 min | Not required | No |
| **Roles split** | IC + ops + comms + scribe | IC + ops + comms | Single responder | Single responder |
| **Internal updates** | Every 30 min | Every 60 min | On state change | On closure |
| **Client updates** | First within 30 min, then every 30 min | First within 60 min if consumer-visible | On request | No |
| **Escalation** | L2 immediate, L4 at 45 min, L5 at 60 min | L2 immediate, L4 at 90 min | Business hours | Business hours |
| **Postmortem** | Mandatory, within 5 business days | Mandatory if consumer-visible or budget-consuming | If it recurs | No |
| **Security notified** | Always | If any control was affected | If any control was affected | No |

**Automatic SEV1 conditions**, regardless of how it looks:

- Any suspected cross-tenant data exposure.
- Any period where authentication or authorization was bypassed or not enforced.
- Any incident consuming more than 25% of the remaining 28-day error budget.
- Any guardrail fail-open window on a `restricted`-classification pool.

---

## 4. Timeline discipline

The timeline is the deliverable. Everything else is recoverable; the timeline is not.

**Rules:**

1. **UTC, always.** A mixed-timezone timeline is worthless in a postmortem and dangerous in an audit.
2. **Impact start time is not detection time.** Record both, separately, and label them. The gap
   between them is the detection metric and the thing most worth improving.
3. **Actions are recorded before they are taken**, not after. "Draining azure-openai/gpt-4o-mini
   now" then "done, traffic moved within 40s".
4. **Record what you looked at, not only what you did.** "Checked breaker state, all closed" is a
   finding that saves the next person ten minutes and prevents a wrong conclusion in the postmortem.
5. **Record decisions with their reasoning**, especially decisions not to act. "Chose not to
   undrain; vendor has not confirmed recovery" is more valuable six weeks later than any command.
6. **Record who approved what.** Every control relaxation, every quota exception, every suspension
   carries an approver name in the timeline as well as in the audit log.
7. **Never edit the timeline retrospectively.** Append corrections: "03:41Z correction: the drain at
   03:22Z was of the bedrock backend, not azure."

Timeline entry shape:

```
03:12Z  [detect]   GatewayAvailabilityBurnFast fired. Error ratio 4.2%, was 0.02% at 03:00Z.
03:13Z  [assess]   Impact began ~03:08Z per agentgate_gateway_requests_total. 4 min detection lag.
03:14Z  [role]     <name> is IC. <name> is ops.
03:15Z  [observe]  Errors 96% provider_timeout, all on azure-openai/gpt-4o-mini. Other backends clean.
03:16Z  [decide]   Draining azure backend. Failover capacity confirmed: bedrock at 38% of cap.
03:17Z  [act]      agentctl backend drain --pool general-chat --backend azure-openai/gpt-4o-mini
03:19Z  [verify]   Backend at 0 req/s. Error ratio 4.2% -> 0.3% and falling.
03:22Z  [comms]    Client update 1 sent. Consuming teams notified in #agentgate-consumers.
03:25Z  [verify]   Error ratio 0.02%. MITIGATED. Not resolved - root cause unknown.
03:26Z  [decide]   Not undraining until vendor confirms. Vendor ticket VEN-88213 opened 03:20Z.
```

Bracketed tags make the timeline greppable and make the postmortem nearly write itself.

Export it when you close:

```bash
agentctl incident timeline --id INC-1234 -o markdown > /tmp/inc-1234-timeline.md
```

---

## 5. Communication cadence

### Internal — `#inc-1234`

Every 30 minutes at SEV1, 60 at SEV2, **even when nothing has changed**. "No change, still
investigating the provider path, next update 04:15Z" is a valid and necessary update. Silence is
read as either resolved or abandoned, and both readings cause problems.

```
[UPDATE 03:45Z] INC-1234 SEV2
Status:   Mitigated, monitoring
Impact:   general-chat pool, 4.2% error ratio 03:08Z-03:25Z. Now 0.02%.
Cause:    azure-openai eastus provider timeouts. Vendor engaged, VEN-88213.
Doing:    Holding on drained backend. Watching bedrock saturation.
Next:     04:15Z, or immediately on change.
IC:       <name>
```

### Consuming teams — `#agentgate-consumers`

Notify when their agents are affected, when you are about to affect them deliberately, and when it
is over. Plain language, no metric names, no PromQL. They do not have our dashboards and should not
need them.

```
[AgentGate] Elevated errors on the general-chat pool, 03:08Z-03:25Z.

If your agent uses general-chat you may have seen 502 or 504 responses during
this window. Requests are succeeding normally now.

Cause: one model provider region was timing out. We moved traffic to another
provider. You do not need to do anything. Responses may be marginally slower
than usual until we move back.

If you are still seeing errors, reply here with a request id from the
x-agentgate-request-id response header and we will trace it.

Next update if anything changes. INC-1234.
```

### Client-facing — regulated environment

Sent by comms, through the agreed channel, approved by the IC. At SEV1 the client incident manager
is on the distribution from the first update.

**Rules for external updates:**

- **State facts, not hypotheses.** "One model provider region is returning timeouts" is a fact.
  "We think their capacity is exhausted" is not — leave it out until it is confirmed.
- **Never speculate about cause in writing.** A speculative cause in an early update, later
  contradicted, becomes a credibility problem and sometimes a compliance one.
- **Always state the next update time**, and send at that time whether or not there is news.
- **Never minimise.** "Some customers may have experienced brief slowness" when the pool was
  returning 5xx for seventeen minutes is the sentence that ends up quoted back at you.
- **Data and controls are stated explicitly.** If there is no data impact, say so. If you do not yet
  know, say that you are establishing it and when you will report.
- **Times in UTC with the client's local time in brackets** if they have a stated preference.

**First update (within 30 min of a SEV1):**

```
AgentGate incident notification - INC-1234

Status:        Investigating
Severity:      SEV1
Started:       2026-08-26 03:08 UTC
Detected:      2026-08-26 03:12 UTC
Services:      AgentGate gateway, general-chat model pool

What is happening:
Requests to the general-chat model pool are returning errors. We estimate
approximately 4% of requests to that pool are affected. Other pools are
unaffected.

Who is affected:
Agents using the general-chat pool in production. Agents using long-context
or embeddings are not affected.

Data impact:
No data loss or data exposure has been identified. Telemetry and usage records
are being written normally. We will confirm this position in the next update.

What we are doing:
We have identified a model provider region returning timeouts and are moving
traffic to an alternative provider within the same pool.

Next update: 2026-08-26 03:45 UTC, or sooner if the position changes.
Incident Commander: <name>
```

**Mitigation update:**

```
AgentGate incident update - INC-1234

Status:        Mitigated, monitoring
Impact window: 2026-08-26 03:08 UTC to 03:25 UTC (17 minutes)
Peak impact:   4.2% of requests to the general-chat pool

What changed:
Traffic has been moved away from the affected provider region. Error rates
returned to normal at 03:25 UTC and have remained normal since.

Current position:
The platform is serving normally. The affected provider backend remains out
of service pending confirmation from the provider. Responses on this pool are
served by an alternative provider and may differ slightly in latency.

Data impact:
Confirmed: no data loss, no data exposure, no impact to usage or chargeback
records. Requests that failed returned an error to the caller and were not
partially processed.

Error budget:
This incident consumed approximately 11% of the monthly availability error
budget for the gateway service.

Next steps:
We will restore the original provider configuration once the provider confirms
recovery, in a staged manner. A blameless postmortem will be completed within
five business days and shared.

Next update: on resolution, or 2026-08-26 08:00 UTC, whichever is sooner.
```

**Resolution update** adds: the confirmed root cause, the permanent fix or the commitment to one
with a date, the total impact, and the postmortem date. Never send a resolution update while a
mitigation with a TTL is still holding the incident together — that is a mitigated state, not a
resolved one, and describing it as resolved is the mistake that damages trust.

### Security and compliance notification

Immediate, in parallel with everything else, for: suspected data exposure; any window where a
control was not enforced; any credential compromise; any guardrail fail-open on a regulated pool.
Notify at suspicion, not at confirmation. The reporting clock in a regulated environment starts at
discovery.

```
[SECURITY NOTIFICATION] INC-1234
Type:            Potential control gap - guardrail fail-open
Discovered:      2026-08-26 03:12 UTC
Window:          03:08 UTC to 03:25 UTC (17 min)
Scope:           Pool regulated-chat, data classification restricted
Requests in window: 412
Control affected: Content-safety scanning, input and output
Confirmed exposure: Not established. Investigation ongoing.
Preservation:    Cache exported, gateway logs retained, no purge performed.
Contact:         <IC name>, #inc-1234
```

---

## 6. Mitigated is not resolved

Three distinct states, and conflating them is the most common process failure in this team:

| State | Meaning | You may |
|---|---|---|
| **Active** | Impact ongoing | Nothing but work the incident |
| **Mitigated** | Impact stopped, cause not fixed, mitigations holding | Reduce update cadence; keep the incident open; hand over explicitly |
| **Resolved** | Cause fixed or permanently mitigated; no TTL-bound change is load-bearing | Close, schedule the postmortem |

You cannot resolve an incident that depends on a time-boxed change. If a retry budget override, a
guardrail relaxation, or an extended cache TTL is holding the platform up, the incident is
mitigated. Write the expiry into the handover ([oncall-guide.md §4](oncall-guide.md#4-the-written-handover)).

---

## 7. Vendor engagement

Open the vendor bridge **in parallel** with mitigation, never after. Provider incidents are common
and their clock is slower than ours.

```
Vendor escalation - AgentGate / FS Client
Provider:        Azure OpenAI
Region:          eastus
Deployment:      gpt-4o-mini (fsclient-eastus)
Symptom:         Request timeouts after 20s, no response body. Began 03:08 UTC.
Rate:            ~40% of requests to this deployment
Our request ids: 01J8Z9X2QK4M, 01J8Z9X2QK5N, 01J8Z9X2QK6P
Our measurement: p95 total latency 24s (baseline 1.8s), error ratio 40%
Comparison:      Same deployment in westus unaffected. Other providers unaffected.
Impact:          Production traffic for a regulated financial services client
Our mitigation:  Traffic moved away from this deployment at 03:17 UTC
Ask:             Confirm status of this deployment and expected recovery time
Contact:         <name>, bridge #vendor-escalation, INC-1234
```

Record the vendor's stated cause in the postmortem verbatim, and note whether it matched what we
observed. Over time that comparison tells you how much to trust a given provider's status page.

---

## 8. Closing

Before closing:

- [ ] Every time-boxed change has been reverted or converted into a recorded permanent change.
- [ ] Every silence is removed or has a documented expiry and owner.
- [ ] Live routing configuration reconciled against git: `agentctl pool diff --against deploy/k8s/overlays/prod/pools.yaml`
- [ ] Every control relaxation restored, with evidence.
- [ ] Timeline exported and attached.
- [ ] Evidence captured per the runbook's post-incident section — logs and metrics age out.
- [ ] Client and consuming teams have had a closing update.
- [ ] Postmortem scheduled with a named owner and a date.

```bash
agentctl incident resolve --id INC-1234 \
  --root-cause "Provider regional degradation, azure-openai eastus" \
  --impact-window "2026-08-26T03:08:00Z/2026-08-26T03:25:00Z" \
  --postmortem-owner <name> --postmortem-due 2026-09-02
```

---

## 9. Related

- [oncall-guide.md](oncall-guide.md) — shift model, handover, wake-up rules
- [postmortem-template.md](postmortem-template.md) — with a worked example
- [README.md](README.md#3-severity-definitions) — severity definitions and escalation ladder
- Runbooks with mandatory security involvement:
  [cache-poisoning-suspected.md](cache-poisoning-suspected.md),
  [jwks-rotation-failure.md](jwks-rotation-failure.md),
  [guardrail-service-down.md](guardrail-service-down.md)
