# ADR 0012 — Run both managed and self-hosted observability, split by question

**Status:** Accepted
**Date:** 2026-07-15
**Deciders:** Platform lead, telemetry owner, client observability team
**Consulted:** Consuming teams, client security, FinOps
**Affects:** Collector routing configuration, `06-telemetry-schema.md` §11, on-call tooling, operating cost

---

## Context

Two audiences ask different questions of the same telemetry.

An **agent engineer** asks: why did this run behave this way, what did the model actually receive and
produce, why did this cost forty cents. These questions require an LLM-native view — prompt,
completion, token cost, model version, cache outcome.

A **platform engineer on call** asks: is the service healthy, did this latency spike coincide with
node memory pressure, which alert fired and what does its runbook say. These questions require
infrastructure correlation and mature alerting wired into the client's existing paging.

No single product serves both well. A generic APM has no concept of a prompt or a token cost. An
LLM-observability tool is not an alerting product and has no view of the infrastructure.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Alerting must live where the client's paging and escalation already are | Decisive |
| 2 | Prompt and completion content must never reach a managed backend | Decisive |
| 3 | Agent engineers need an LLM-native trace view or they cannot do their jobs | Very high |
| 4 | Operating burden on a five-person team | High |
| 5 | Retention cost must not grow punitively with adoption | High |
| 6 | Avoid duplicated on-call surface and dashboard drift | Medium |

## Options Considered

### Option A — Managed only

Azure Monitor and App Insights, or CloudWatch and X-Ray.

| Pros | Cons |
|---|---|
| Nothing to operate | **No LLM-native view.** Sees spans, not model calls |
| Already reviewed and present in the client's environment | **Content in a managed backend is a data-flow the client will not approve** |
| Mature alerting, already wired to paging | Expensive per GB; cost grows with adoption |
| Strong infrastructure correlation | The platform's primary users cannot use its observability |

Rejected on driver 3.

### Option B — Self-hosted only

Langfuse or an equivalent self-hosted OTLP store.

| Pros | Cons |
|---|---|
| Native `gen_ai.*` understanding; built for this | **Not an alerting product** |
| Content stays inside the client's network | **No infrastructure correlation** |
| Cheap retention; storage is yours | Wiring a new alerting path into a regulated client's paging is a months-long review |
| Low lock-in; OTLP in, OTLP out | An operational burden on a five-person team |

Rejected on driver 1. The team would be unable to run the platform.

### Option C — Both, split by question

| Pros | Cons |
|---|---|
| Each audience gets the tool that answers their question | Two backends to operate |
| Content stays self-hosted; metrics and alerts stay managed | Duplicated trace export volume |
| Alerting reuses the client's existing, already-reviewed paging | Two places to look, which is a genuine cognitive cost |
| The split is a collector configuration, changeable later | Dashboard drift risk |

### Option D — Both, but self-hosted as a full mirror of everything

| Pros | Cons |
|---|---|
| Complete independence from the managed backend | Doubles storage cost with no additional answered question |
| | Doubles the operating burden |
| | Encourages two parallel alerting paths, which is the failure mode driver 6 warns about |

Rejected. "Run both" must mean a deliberate split, not duplication.

## Decision

**Run both, with a division of responsibility by question, not by preference.**

| Signal | Destination | Reasoning |
|---|---|---|
| **Metrics** | Prometheus or managed Prometheus, **primary**; mirrored to the managed APM where the client's dashboards live | Cheap, low cardinality after the §5.1 controls, and drives alerting |
| **Alerting** | **Managed platform, exclusively** | The paging, escalation and incident tooling already exist and are already reviewed. A second alerting path is a second on-call surface |
| **Traces, sampled** | **Both.** Langfuse primary for engineers; managed APM for platform and infrastructure correlation | Two audiences, two questions |
| **Traces, content events** | **Langfuse only. Never the managed backend** | Driver 2. Enforced by not configuring the content exporter in the production collector at all |
| **Logs** | Managed log store, **primary** | Correlation with infrastructure logs is the main use, and that is where they already are |
| **Usage records and cost** | **Warehouse, exclusively.** Neither observability backend | Billing data is not telemetry: unsampled, immutable, needs SQL and joins, retained for years |

Supporting rules:

1. **Routing is a collector concern.** Agents and the gateway emit OTLP once; the collector fans out.
   Changing the split later is a configuration change, not re-instrumentation.
2. **Only the managed side has alert-backed dashboards.** Langfuse dashboards are exploratory and are
   explicitly not part of the on-call path. This is what prevents dashboard drift from becoming an
   incident-response problem.
3. **Runbooks name which backend to open.** "Two places to look" is only a cost if the reader has to
   choose; the runbooks choose for them.

## Consequences

### Positive

- Each audience gets a tool that answers its question, rather than both getting a compromise.
- Alerting reuses an already-reviewed, already-integrated paging path — the single biggest saving in
  this decision, and one that would have taken months to replicate.
- Content never leaves the self-hosted store, so the managed backend's data-flow review stays
  metadata-grade.
- Retention cost is bounded: the expensive managed store holds sampled traces and metrics; the cheap
  self-hosted store holds the high-volume LLM detail.
- Low lock-in. Both sides receive OTLP, and the split is reversible by configuration.

### Negative

- **Two backends to operate.** Langfuse is a real deployment with a database, an upgrade path and
  backups. It is one deployment with one job, but it is not free.
- Sampled traces are exported twice. At a 5% baseline the duplicated volume is small, but it is real.
- Two places to look is a genuine cognitive cost, mitigated but not eliminated by the split being by
  question and by runbooks naming the destination.
- Dashboard drift risk between the two, mitigated by only one side having alert-backed dashboards.
- A new engineer must learn two tools.

### Neutral

- The managed side may be Azure Monitor or CloudWatch and X-Ray depending on cloud. The collector
  routing differs; nothing above changes.

### What this forecloses

A single-pane-of-glass observability story. Recovering it would mean either accepting a generic APM
for LLM debugging, or building alerting on the self-hosted side — both of which were rejected as
options A and B.

## Revisit when

Any of:

- The managed APM gains genuine `gen_ai.*` semantic support with an acceptable content-handling
  posture, which would make Option A viable.
- The self-hosted store's operating burden exceeds roughly one engineer-day per month sustained,
  which would mean it is not one deployment with one job any more.
- The client's paging platform gains native OTLP alerting that would let the self-hosted side carry
  alerts without a new review.
