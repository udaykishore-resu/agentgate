# ADR 0009 — Semantic cache off by default

**Status:** Accepted
**Date:** 2026-07-01
**Deciders:** Platform lead, client risk function
**Consulted:** FinOps, two consuming teams, client architecture review board
**Affects:** Stages 8 and 14, per-pool cache policy, `02-flows.md`, `03-sequences.md` §7

---

## Context

An exact-match cache keyed on a hash of the normalised request catches genuinely identical requests.
Agentic workloads produce many of those — retries, re-plans, repeated classification of the same
input — so the exact cache earns its place immediately.

A semantic cache goes further: it embeds the final user turn and returns a cached response when the
cosine similarity to a previous request exceeds a threshold. The saving is much larger because near-
duplicate requests are far more common than exact duplicates.

It also changes what "the same question" means, and it does so probabilistically.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | An answer given to the wrong question must not be possible in a regulated context | Decisive |
| 2 | Model spend reduction is a real and funded objective | High |
| 3 | Every response must be explicable after the fact | High |
| 4 | Data classification governs what may be treated as equivalent | High |
| 5 | Cache hit rate | Medium |

## Options Considered

### Option A — Semantic cache on by default with a high threshold

| Pros | Cons |
|---|---|
| Best cost reduction, immediately | **Two prompts at 0.97 cosine similarity can differ by an account number, a date, or a negation** |
| Threshold is tunable per pool | Embedding models are not trained to treat those differences as significant — they are trained to treat them as noise |
| | A wrong answer in a dispute-triage or credit-decision context is a regulatory event, not a bug report |
| | Default-on means the failure occurs before anyone has decided to accept the risk |

Rejected on driver 1.

### Option B — Semantic cache off by default; per-pool enablement requiring a data-classification allowance

| Pros | Cons |
|---|---|
| The risk is accepted deliberately, per pool, by the people who own the data | Lower hit rate and higher spend than the theoretical maximum |
| The capability is built and available where it is genuinely safe | Two cache behaviours to document, test and explain |
| Classification is the gate, which is the right axis | Teams must be told the capability exists, or it will never be used |

### Option C — No semantic cache at all

| Pros | Cons |
|---|---|
| Simplest; no risk | Leaves a real saving on the table for workloads where near-duplicates are safe |
| One cache behaviour | Teams will implement their own caches, without the tenant isolation, similarity recording or attribution |

Rejected. The last row matters: a capability the platform refuses to provide gets reimplemented
worse, elsewhere.

### Option D — Semantic cache with a mandatory model-based verification of equivalence

Confirm semantic equivalence with a cheap model call before returning a cached response.

| Pros | Cons |
|---|---|
| Much lower false-hit rate | A model call to avoid a model call — the saving largely disappears |
| | Adds latency to the path that exists to reduce latency |
| | The verification is itself probabilistic, so it reduces the risk rather than removing it |

Rejected on cost/benefit, though it is the most interesting rejected option and may be worth
revisiting for high-value pools.

## Decision

**Semantic cache is off by default. It is enabled per pool, and only where the data classification
carries an explicit allowance.**

| Aspect | Decision |
|---|---|
| Default | **Off** |
| Enablement | Per pool, in configuration, as a reviewed change |
| Prerequisite | The pool's data classification must carry a semantic-cache allowance. `restricted` never does |
| Threshold | Cosine ≥ **0.97** default, configurable per pool but never below 0.95 |
| Scope | Embedded on the **final user turn**, matched against **the tenant's own** recent entries only. Never across tenants |
| Auditability | The similarity score and the matched `origin_trace_id` are recorded on the `gateway.cache` span, so any semantic hit can be reconstructed and reviewed |
| Interaction with exact cache | Exact is always tried first. Semantic is a fallback, never a replacement |
| Bypasses | Same as exact: `temperature > 0.2` without an explicit policy allowance, tool-calling requests, and `x-agentgate-cache: off` |

Enabling it for a pool requires evidence: a corpus of that pool's real traffic, reviewed by the owning
team, showing an acceptable false-hit rate at the chosen threshold. "We think 0.97 is high enough" is
not evidence.

## Consequences

### Positive

- The default posture is the safe one, so a pool that nobody has thought carefully about behaves
  conservatively.
- Where semantic caching is genuinely safe — internal documentation assistants, code helpers, low
  classification summarisation — the saving is available.
- Every semantic hit is auditable: the score, the matched origin trace, and the timestamp are on the
  span. A regulator asking "why did this customer receive this answer" has an answer.
- Tenant isolation is absolute; the embedding search never crosses a tenant boundary.

### Negative

- **Cache hit rate and cost saving are lower than the theoretical maximum.** FinOps will observe this
  and should be told why in advance rather than asked afterwards.
- Two cache behaviours mean two sets of documentation, tests and consumer explanations.
- Enabling a pool requires evidence-gathering work that nobody is funded for, so in practice semantic
  cache may remain unused. That is an acceptable outcome; an unused safe default is better than a
  used unsafe one.
- The embedding call itself has a cost and a latency, paid on every exact-cache miss in an enabled
  pool. For pools with a low semantic hit rate this can be net negative, which is another reason for
  evidence before enablement.

### Neutral

- The 0.97 default is conservative relative to common practice. It is chosen for a financial-services
  context and should not be read as a general recommendation.

### What this forecloses

Nothing permanently. The capability is built; only the default is conservative. Enabling it for a pool
is a configuration change plus evidence.

## Revisit when

A pool's owning team produces a reviewed corpus showing an acceptable false-hit rate at the proposed
threshold, and the data classification permits it. That is a per-pool decision recorded against the
pool's configuration, not a change to this ADR.

This ADR would itself be superseded only if the industry produces embedding or verification techniques
that make semantic equivalence deterministic rather than probabilistic — at which point the whole
argument changes.
