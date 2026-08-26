# ADR 0007 — Tail sampling at the collector gateway tier

**Status:** Accepted
**Date:** 2026-06-24
**Deciders:** Platform lead, telemetry owner
**Consulted:** Client observability team, three consuming teams
**Affects:** Collector configuration, `06-telemetry-schema.md` §8, storage cost, promotion gate inputs

---

## Context

Every gateway request produces a `gateway.request` span, sixteen `gateway.policy.<stage>` spans, one
or more `gen_ai.chat` spans, and guardrail and cache spans. An agent run adds `agent.invoke`,
`agent.step` and `agent.tool` spans. A single agent run of three model calls is comfortably
sixty spans.

At a fleet of a few hundred agents, retaining every span is a storage and cost problem that grows
linearly with adoption — which is the wrong incentive for a platform trying to attract consumers onto
itself. Sampling is required. The question is which sampling, and where.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Every trace that is interesting must be retained | Decisive |
| 2 | Storage cost must not grow linearly with adoption | Very high |
| 3 | Billing and aggregate metrics must be exact regardless of sampling | Very high |
| 4 | Telemetry completeness is a promotion gate, so the sampling must not corrupt the completeness measure | High |
| 5 | Operationally simple enough for a small team | Medium |

## Options Considered

### Option A — No sampling; retain everything

| Pros | Cons |
|---|---|
| Any request is retrievable | Cost grows linearly with adoption, penalising the platform's own success |
| No sampling logic at all | At fleet scale the storage bill becomes a reason not to onboard agents |
| | Query performance degrades on a store dominated by uninteresting traces |

Rejected on driver 2.

### Option B — Head sampling at the SDK

Decide at span creation, typically a fixed probability.

| Pros | Cons |
|---|---|
| Trivial; no buffering | **Decides before the trace is interesting.** The SDK does not know a failover will occur or that the request will take 12 seconds |
| Lowest possible pipeline cost | Discards exactly the traces you most want, at random |
| Consistent by trace id | Every agent SDK must be configured consistently, across five runtimes |

Rejected on driver 1. This is the option most teams ship and then regret during their first incident.

### Option C — Tail sampling at the collector gateway tier

Buffer the trace, decide once it is complete.

| Pros | Cons |
|---|---|
| **Decides with full knowledge of the trace** | Requires buffering, and therefore memory |
| 100% retention of errors, blocks, failovers and slow requests | Requires trace-affine routing, which is a hard operational requirement |
| Sampling policy is one configuration change, not a fleet-wide SDK rollout | Requires a decision wait time, so a very long trace may be decided incompletely |
| Baseline rate keeps aggregate shape visible | The collector tier becomes a stateful component with its own scaling behaviour |

### Option D — Tail sampling with a much higher baseline, e.g. 50%

| Pros | Cons |
|---|---|
| More uninteresting traces available for exploratory analysis | Ten times the storage for traces that, by construction, are unremarkable |
| | The interesting traces are already at 100%; the marginal value of the extra 45% is low |

Rejected on cost/benefit. 5% is enough to establish aggregate shape; anything specific and interesting
is already kept.

### Option E — Per-agent sampling rates set by the owning team

| Pros | Cons |
|---|---|
| Teams choose their own trade-off | Uncontrolled aggregate cost |
| | A team that sets 100% for itself imposes cost on the shared platform |
| | Completeness measurement becomes agent-dependent and hard to compare |

Rejected on driver 4.

## Decision

**Tail sampling at the collector gateway tier, with the `SPEC.md` §4.4 policy.**

| Policy | Rate |
|---|---|
| Trace contains an error | **100%** |
| Trace contains a guardrail block | **100%** |
| Trace contains a failover | **100%** |
| Trace latency above p99 | **100%** |
| Everything else | **5% baseline** |

Supporting decisions:

1. **Trace-affine routing is mandatory.** The agent tier uses a load-balancing exporter keyed on trace
   id so every span of a trace reaches one gateway-tier instance. Without it, the sampler sees
   fragments and makes inconsistent decisions. This is not an optimisation; it is a correctness
   requirement.
2. **Decision wait time: 30 seconds**, above the p99.9 request duration. A trace still open at 30 s is
   decided on what has arrived.
3. **Late spans**: exported if they arrive after a keep decision, dropped if after a drop decision.
   This produces occasional single-span traces in the backend, which is preferable to holding every
   trace in memory for minutes.
4. **Memory is bounded by `memory_limiter`**, so a trace-volume spike becomes back-pressure rather
   than an out-of-memory event.
5. **The following are never sampled**: metrics, `UsageRecord` on the usage stream, audit-class logs,
   and the content pipeline. Billing and aggregate correctness do not depend on sampling.
6. **Completeness measurement uses metrics, not traces.** `traces_expected` is the gateway request
   count from an unsampled counter, and `traces_received` is counted at the collector **before** the
   sampling decision. Measuring after sampling would make completeness a measure of the sampler.

Point 6 is the subtle one, and getting it wrong would make the promotion gate meaningless.

## Consequences

### Positive

- Every trace an engineer actually wants — errors, guardrail blocks, failovers, slow requests — is
  retained in full.
- Storage cost is roughly a twentieth of full retention for the uninteresting majority, and does not
  grow linearly with adoption.
- Sampling policy is one collector configuration change, not a coordinated SDK rollout across five
  runtimes.
- Billing, availability and cost figures are exact.

### Negative

- **A specific successful, fast, unremarkable request may not be retrievable as a trace.** This is the
  one that surprises consumers, and it is documented in `06-telemetry-schema.md` §8.3. The request is
  always retrievable as a usage record and is always counted in metrics.
- The collector gateway tier becomes stateful and memory-sensitive. A trace-volume spike is a memory
  event, bounded but real.
- Trace-affine routing is an operational requirement that is easy to misconfigure and whose failure
  mode — inconsistent sampling — is subtle. `orphan_span_ratio` is the detector.
- The 30-second decision window means a genuinely long-running trace can be decided on partial data.

### Neutral

- The p99 latency threshold is computed on a rolling basis, so "slow" is defined relative to current
  behaviour rather than a fixed number. During a general slowdown, the retained set stays
  proportionate rather than exploding.

### What this forecloses

Retrieval of an arbitrary specific request's trace. Recovering it would mean full retention, and the
cost model that follows. If a regulatory requirement ever demands per-request trace retention, the
correct answer is to extend the content-pipeline model — a separate, unsampled, access-controlled
path — rather than to remove sampling from the main pipeline.

## Revisit when

- A regulatory requirement mandates per-request trace retention.
- Collector memory becomes the binding constraint on telemetry throughput, which would point at the
  decision wait time rather than at the policy.
- The measured share of traces retained by the four 100% rules exceeds roughly 25%, which would mean
  the system is unhealthy enough that the sampling policy is no longer the interesting question.
