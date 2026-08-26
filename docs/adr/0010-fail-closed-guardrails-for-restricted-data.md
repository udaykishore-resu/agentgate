# ADR 0010 — Guardrail failure mode is per pool; fail-closed by default for restricted data

**Status:** Accepted
**Date:** 2026-07-02
**Deciders:** Platform lead, client risk function, client security
**Consulted:** Consuming teams, client architecture review board
**Affects:** Stages 7 and 13, pool policy, `guardrails` service availability requirements, `07-slo-alerting.md`

---

## Context

Stages 7 `guardrail.input` and 13 `guardrail.output` call a content-safety provider — `noop`,
`builtin`, or `callout` to a managed content-safety service or the client's own filtering service.

The `callout` provider is a network dependency and will sometimes be unavailable, slow, or returning
errors. The gateway must then choose between serving the request without a verdict, or refusing it.

This is not a technical question with a technical answer. It is a risk question, and the correct
answer differs by the data involved. A decision must be recorded because whichever behaviour is
implemented silently becomes the client's de facto policy.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | For restricted data, an unscanned response is worse than no response | Decisive for that class |
| 2 | For low-classification internal tooling, availability is worth more than a missed scan | Decisive for that class |
| 3 | The choice must be explicit and reviewable, never inferred | Very high |
| 4 | Bypasses must be countable and alertable | Very high |
| 5 | The guardrail service must not become an availability dependency for traffic that does not need it | High |

## Options Considered

### Option A — Global fail-open

Proceed without a verdict when the guardrail is unavailable.

| Pros | Cons |
|---|---|
| Maximum availability | Restricted content can reach a caller unscanned |
| The guardrail is never an availability dependency | The failure is silent unless specifically instrumented |
| | Not defensible to a regulated client's risk function |

Rejected on driver 1.

### Option B — Global fail-closed

Refuse the request when the guardrail is unavailable.

| Pros | Cons |
|---|---|
| No content ever reaches a caller unscanned | The guardrail service becomes a hard availability dependency for **all** model traffic |
| Simple, single behaviour | An internal documentation assistant is taken down to protect data it never touches |
| | Concentrates availability risk in a component that is often a third-party service |

Rejected on drivers 2 and 5.

### Option C — Per-pool failure mode, defaulting by data classification

| Pros | Cons |
|---|---|
| The right answer for each data class | Two behaviours to implement, test, document and run books for |
| The choice is explicit in the pool's configuration and reviewed at promotion | An operator can misconfigure a restricted pool as `fail_open` |
| The guardrail is only an availability dependency where the risk requires it | Requires the classification metadata to be correct |

### Option D — Fail-open with a degraded response — serve, but mark the response as unscanned

| Pros | Cons |
|---|---|
| Availability preserved, caller informed | The caller has already received the content by the time they read the marker |
| | For restricted data the harm is in the delivery, not in the labelling |
| | Places the risk decision on the consuming application, at request time, which is the worst place for it |

Rejected on driver 1. Genuinely tempting, and worth stating why it fails: a marker on content that has
already been delivered does not undo the delivery.

## Decision

**Guardrail failure mode is declared per pool. `fail_closed` is the default for
`data_classification=restricted`. `fail_open` is available for availability-first pools.**

| Aspect | Decision |
|---|---|
| Declaration | Per pool policy, alongside categories, thresholds and action |
| Default for `restricted` | **`fail_closed`** |
| Default for other classifications | `fail_open`, but the pool must still declare it explicitly — the default is a starting point in the template, not an implicit behaviour |
| `fail_closed` behaviour | Guardrail unavailable is treated as a block. `403 guardrail_blocked` on input; SSE `error` frame on a stream already started |
| `fail_open` behaviour | The scan is skipped, the request proceeds, and `agentgate.guardrail.bypassed=true` is set on the span |
| Observability | Every bypass increments `agentgate.guardrail.decisions` with `action="bypassed"` |
| Alerting | `AgentGateGuardrailBypassed` is a **P1 page with a zero threshold**. Any bypass is a compliance event |
| Review | The failure mode is reviewed at promotion, per pool, with the owning team |
| Capacity consequence | For `fail_closed` pools the guardrail service inherits the pool's availability requirement and must be engineered and capacity-planned accordingly |

The zero-threshold alert is deliberate and is the only alert in the catalogue with no tolerance band.
A `fail_open` pool serving unscanned content is *permitted*, but it must never be *unnoticed*.

### Output scanning is windowed regardless of failure mode

For streams, output content is buffered in approximately 256-token windows, scanned, and then
released. A violation is therefore caught before the caller sees it, while keeping time-to-first-token
acceptable. This is what makes `fail_closed` meaningful on a streaming response: without windowing,
the content would already be delivered before any verdict existed.

## Consequences

### Positive

- Restricted-data pools cannot serve unscanned content, which is the property the client's risk
  function requires.
- Low-classification tooling is not taken offline by a content-safety service outage it has no need
  for.
- The choice is explicit configuration, reviewed at promotion, so nobody inherits a risk posture by
  accident.
- Bypasses are counted and paged, so the `fail_open` risk is bounded by observation.

### Negative

- **The guardrail service becomes a hard availability dependency for restricted pools.** Its
  availability must meet or exceed the gateway's, which means real engineering and real capacity
  planning for a component that might otherwise have been treated as ancillary.
- Two behaviours means two sets of tests, two runbook paths, and a more complex explanation to
  consumers.
- A misconfigured restricted pool set to `fail_open` would be a silent risk. Mitigated by the
  classification-derived default, by promotion review, and by the bypass alert making it visible the
  first time it matters.
- `fail_closed` pools experience guardrail latency on the critical path for both input and every
  output window, which is a material contributor to TTFT.

### Neutral

- The `builtin` provider — regex and entropy PII plus a denylist — is in-process and has no network
  failure mode. Pools using it are unaffected by this decision in practice, and it is a reasonable
  fallback posture for a pool that cannot tolerate the callout dependency.

### What this forecloses

A single global behaviour, and the simplicity that would come with it. Recovering it would mean
forcing one of the two data classes into the wrong posture.

## Revisit when

Never as a principle. The per-pool settings themselves are reviewed continuously — at every promotion
of an agent using that pool, and whenever a pool's data classification changes.

The one thing that would reopen the ADR: if `fail_closed` pool availability became the dominant
contributor to the gateway's availability error budget, the response would be to improve the guardrail
service or move those pools to `builtin`, not to change the failure mode.
