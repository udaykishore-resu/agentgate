# ADR 0004 — Token-aware quota with reserve and settle

**Status:** Accepted
**Date:** 2026-06-11
**Deciders:** Platform lead, FinOps
**Consulted:** Three consuming teams, client architecture review board
**Affects:** Stage 6 `quota.tokens`, stage 15 `meter`, `x-agentgate-ratelimit-*` headers, `08-chargeback.md`

---

## Context

Model consumption is measured in tokens, and token cost per request varies by three orders of
magnitude. A request-per-minute limit does not bound spend: 600 requests generating 50 tokens each
and 600 requests generating 4,000 tokens each are the same number under an RPM limit and an
eighty-fold difference on the invoice.

The client requires per-agent spend control with a monthly budget. The question is *when* the limit
is applied relative to the work being done.

This decision is forced before the contract freezes, because the `x-agentgate-ratelimit-limit-tokens`
/ `-remaining-tokens` / `-reset` headers and the `quota_exceeded` error are part of the frozen
contract and their semantics follow from it.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | A single request must not be able to blow a budget | Decisive |
| 2 | Enforcement must be correct across replicas, scale events and rolling deploys | Very high |
| 3 | Callers must be able to pace themselves from the response headers | High |
| 4 | The overhead cost must fit inside a 60 ms budget | High |
| 5 | A crashed pod must not strand quota | High |
| 6 | Estimation error must be bounded and transient | Medium |

## Options Considered

### Option A — Request-rate limiting only

| Pros | Cons |
|---|---|
| Trivial; one counter | Does not bound spend at all, which is the entire requirement |
| No estimation error | A single long-context request can consume a day's budget |

Rejected on driver 1.

### Option B — Post-hoc metering with next-request enforcement

Meter actual usage after the fact; refuse the *next* request if the budget is exceeded.

| Pros | Cons |
|---|---|
| No estimation needed; always exact | **Cannot stop the request that blows the budget** |
| Simplest correct accounting | With streaming and long generations, overshoot is unbounded within a single request |
| No settle path | Under concurrency, many requests are in flight when the budget is crossed, so the overshoot is unbounded in a second dimension too |

Rejected on driver 1. This is the option most systems ship, and it is why most systems' budgets are
advisory.

### Option C — Estimate, reserve, settle

Estimate input tokens plus `max_tokens`, reserve that total, release the difference on completion.

| Pros | Cons |
|---|---|
| **Overshoot is bounded by one request's `max_tokens`** | Requires an estimation step |
| Callers see true remaining budget after settle | Over-reservation temporarily under-serves the agent |
| Works correctly under concurrency — the reservation is the serialisation point | Every terminal path must settle, including errors and disconnects |
| A crashed pod's reservation expires on TTL | A large `max_tokens` reserves heavily, which consumers must understand |

### Option D — Reserve on the estimate only, ignore `max_tokens`

| Pros | Cons |
|---|---|
| Smaller reservations; less under-serving | Output is the expensive direction and is exactly what is unbounded |
| | Reintroduces unbounded overshoot for the output half |

Rejected. It solves the cheap half of the problem.

### Option E — Probabilistic admission based on historical token distribution

Admit based on the agent's observed distribution rather than a reservation.

| Pros | Cons |
|---|---|
| No over-reservation | Unexplainable to a consuming team: "why was I throttled" has a statistical answer |
| Higher effective utilisation | Wrong precisely when behaviour changes, which is when a budget matters |
| | Not defensible in a chargeback dispute |

Rejected on explicability. In a financial-services context, a control whose behaviour cannot be
explained to the team it constrains is not a usable control.

## Decision

**Estimate, reserve, settle, on a distributed token bucket keyed `tenant:team:agent:env`.**

1. **Estimate.** Input tokens from the tokenizer where available, otherwise a four-characters-per-token
   heuristic.
2. **Reserve.** `reserve_amount = estimated_input + max_tokens`. One atomic Lua script checks and
   decrements both the TPM bucket and the monthly budget, and writes a reservation record. Refusal is
   `429 quota_exceeded` with `retry_after_seconds`.
3. **Settle.** On every terminal path, release `reserve_amount - actual` back to the bucket and record
   `actual` against the monthly budget. Settle is idempotent on `reservation_id`.
4. **Reservation TTL** = request deadline + 30 s, so a crashed pod cannot strand capacity longer than
   that.
5. **`max_tokens` omitted** uses the logical model's configured default, reported by
   `GET /v1/models/{id}`.
6. **Response headers reflect post-settle state**, so a caller pacing on them sees true remaining
   budget rather than the reserved-but-unused figure.
7. **A second bucket keyed `pool:backend`** protects shared capacity from a single agent, independent
   of that agent's own quota.

Settle sources for `actual`, in order of preference: the provider's reported usage; the
`agentgate.usage` frame for streams; locally counted tokens from delivered chunks, marked
`agentgate.usage.estimated=true`.

## Consequences

### Positive

- Overshoot is bounded by one request's `max_tokens`, which is the strongest guarantee available
  without pre-generation knowledge.
- The reservation is the serialisation point, so correctness holds under concurrency without a lock.
- Callers get a genuinely useful `x-agentgate-ratelimit-remaining-tokens` for self-pacing.
- Crash recovery is automatic via TTL; the monthly budget reconciles independently from usage records
  on a separate path.

### Negative

- **A large `max_tokens` materially reduces effective throughput.** `max_tokens: 8192` for responses
  typically 200 tokens long cuts effective request rate by roughly forty. This is documented
  prominently in `04-gateway-contract.md` §9, and it is the single most common consumer surprise.
- Every terminal path must settle. Missing one is a slow quota leak. Mitigated by TTL and by settle
  being exercised in tests for every error path.
- Two Redis operations per request on the hot path, in one script. Roughly 0.5–1.5 ms, inside budget
  but not free.
- Estimation error means an agent can be briefly under-served relative to its true entitlement.

### Neutral

- The monthly budget is checked in the same script as the TPM bucket, so a monthly breach and a
  per-minute breach are both `quota_exceeded`, distinguished by `detail` and by `retry_after_seconds`
  reflecting the month boundary rather than the bucket refill.

### What this forecloses

Post-hoc-only metering, and any design where a request's cost is unknown until after it completes.
Recovering it would be trivial technically and would give up the only bound on overshoot.

## Revisit when

Measured estimation error exceeds roughly 30% on a material share of traffic — the fix is a better
tokenizer, not a different design — or the Redis round trip becomes a measurable share of the
overhead budget, at which point local pre-reservation with periodic reconciliation is the next step
and is compatible with everything above.
