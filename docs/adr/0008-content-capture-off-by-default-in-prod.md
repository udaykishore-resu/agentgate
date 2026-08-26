# ADR 0008 — Content capture off by default in production

**Status:** Accepted
**Date:** 2026-06-25
**Deciders:** Platform lead, client security, client data protection
**Consulted:** Consuming teams, client observability
**Affects:** `AGENTGATE_CAPTURE_CONTENT`, the content pipeline, `06-telemetry-schema.md` §7, `10-network-security.md` §5

---

## Context

Debugging an agent frequently requires seeing what was actually sent to the model and what came back.
LLM observability tooling is built around this, and an agent engineer's most common question —
"why did the model do that" — is often unanswerable without the content.

In a financial-services context the prompt is the data. A dispute-triage agent's prompt contains
account numbers, transaction narratives, customer names and, in some cases, information the client is
obliged to handle under specific controls. Capturing prompts turns the observability plane into a
system that processes restricted data, with all the review, retention, access-control and residency
obligations that follow.

A decision is forced now because a default that has ever been `on` in production cannot be
retroactively un-collected.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Data collected in error cannot be uncollected | Decisive |
| 2 | Content is the client's restricted data, not telemetry about it | Very high |
| 3 | Agent engineers genuinely need content to debug | High |
| 4 | Configuration mistakes must not be the only thing preventing a breach | High |
| 5 | The observability plane's review burden should be proportionate to what it holds | Medium |

## Options Considered

### Option A — Content on by default, redacted

| Pros | Cons |
|---|---|
| Best debugging experience | Redaction is best-effort; a novel PII format is not caught |
| Content is there when you need it | The trace store becomes a restricted-data system, changing its entire review posture |
| | Retention, access control and residency all become content-grade for the whole store |
| | The failure mode is silent: nobody notices content is being collected until an audit |

Rejected on drivers 1 and 2.

### Option B — Content off by default in production, redacted in non-production, on a separate pipeline

| Pros | Cons |
|---|---|
| Production restricted data is not collected at all | Production debugging is harder; the engineer works from metadata |
| Non-production keeps the debugging experience where the data is synthetic or lower-classification | Non-production data is not always as synthetic as teams believe |
| The observability store's review posture stays metadata-grade | Two configurations to keep straight |
| Separate pipeline means retention and access control are separable | |

### Option C — Content off everywhere

| Pros | Cons |
|---|---|
| Simplest possible posture | Removes the debugging capability entirely, including where the data is genuinely synthetic |
| | Teams will work around it by logging prompts themselves, in places with no controls at all |

Rejected on driver 3. The last row is the real argument: a control that makes legitimate work
impossible gets routed around, and the workaround has no controls.

### Option D — Content on, per-agent opt-in by the owning team

| Pros | Cons |
|---|---|
| Teams choose for their own data | A team is not the right authority to decide that the client's restricted data may be retained |
| | The decision would be made under debugging pressure, which is the worst moment to make it |
| | Inconsistent posture across the estate is impossible to attest to in an audit |

Rejected on driver 2.

## Decision

**`AGENTGATE_CAPTURE_CONTENT=off` in production. `redacted` in non-production. `full` requires a named
approval and is never used in production.**

| Mode | Behaviour | Default |
|---|---|---|
| `off` | No content emitted anywhere | **Production** |
| `redacted` | Content emitted as span events after guardrail redaction | **Non-production** |
| `full` | Content emitted verbatim | Never a default. Named approval required, never in prod |

Supporting decisions:

1. **Content is emitted as span events** — `gen_ai.content.prompt`, `gen_ai.content.completion` — never
   as span attributes, so it is separable at the pipeline level.
2. **A physically separate content pipeline**: not sampled, own retention of 7 days, own access
   control with every read audited, and it never leaves the self-hosted store.
3. **Two-stage redaction.** The gateway redacts through the guardrail engine, which has the policy
   context. The collector redacts again, because a gateway misconfiguration must not be the only thing
   standing between restricted content and a store not provisioned for it.
4. **Three independent enforcement controls in production**, any one of which is sufficient:
   - the production deployment pipeline fails if the variable is anything other than `off`;
   - `agentgate.content_capture.mode` is exported as a gauge and a non-zero value in prod is a **P1
     page**;
   - the content pipeline's exporter is **not configured at all** in the production collector, so a
     gateway misconfiguration has nowhere to send content.

Control 4 is the heart of this ADR. Configuration alone is not a control, because configuration
drifts. Three independent mechanisms, each of which alone prevents the outcome, is a control.

### What production debugging looks like without content

| Question | Answered by |
|---|---|
| Why was this slow | Per-stage duration attributes on `gateway.request` |
| Why did this fail | `agentgate.error.code`, `agentgate.attempt`, `agentgate.retry.reason`, breaker state |
| Why was this expensive | Token counts, `agentgate.backend`, failover attributes, cache outcome |
| Why was this blocked | `agentgate.guardrail.category` and `.action` — the category, never the content |
| What did the model actually say | **Not answerable in production.** Reproduce in non-production with synthetic input |

The last row is the cost, stated plainly. It is real, and consuming teams are told about it during
onboarding rather than discovering it during an incident.

## Consequences

### Positive

- The client's restricted data does not enter the observability plane in production.
- The trace store's review posture stays metadata-grade, which materially shortens its data-flow
  review.
- Non-production keeps the debugging experience for teams working with synthetic or lower-
  classification data.
- Three independent controls mean a single mistake — a bad environment variable, a wrong Helm value —
  does not produce a breach.

### Negative

- **Production debugging is genuinely harder.** An agent misbehaving only in production, only on
  specific inputs, is a hard problem without the input. This is the real cost and it is paid by the
  people this platform exists to serve.
- Teams may be tempted to log prompts in their own application logs, where AgentGate's controls do not
  apply. This is called out explicitly in onboarding and in the security review of each agent, and it
  is the main residual risk of this decision.
- Non-production is not always as synthetic as teams assume. `redacted` mode plus the separate
  pipeline is the mitigation, not a guarantee.

### Neutral

- The content-capture machinery is built and tested regardless of the production default, so enabling
  it in an approved context is a configuration change, not a development effort.

### What this forecloses

Production prompt-level debugging as a routine capability. Recovering it for a specific investigation
means a named approval, a time-boxed enablement, and the data-flow review that follows — which is the
correct amount of friction for what it is.

## Revisit when

The client's data-protection function approves a production content-capture posture with defined
retention, access control and residency, **and** a specific debugging need justifies it. Both
conditions, and never as a default.
