# ADR 0006 — Circuit breaker shape and parameters

**Status:** Accepted
**Date:** 2026-06-18
**Deciders:** Platform lead, gateway engineers
**Consulted:** Client platform engineering
**Affects:** Stage 11 `invoke`, `internal/resilience`, `agentgate.breaker.state`, the `no_healthy_backend` error

---

## Context

Model providers fail in characteristic ways: regional degradation, quota exhaustion producing
sustained 429s, and slow failure where requests time out rather than erroring. Without a breaker,
every request continues to pay the full timeout against a backend that is known to be failing,
consuming the gateway's concurrency and the caller's deadline.

Breaker parameters are the difference between a breaker that protects the system and one that
amplifies a transient blip into an outage. They must be chosen deliberately and written down, because
they are otherwise tuned by whoever is on call during the incident.

`SPEC.md` §3.3 fixes the parameters. This ADR records why those values, what they trade against, and
what would change them.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Stop spending caller deadline on a known-failing backend | Decisive |
| 2 | Do not trip on a transient blip and cause a self-inflicted outage | Very high |
| 3 | Recover automatically and quickly when the backend returns | Very high |
| 4 | Breaker state must be observable and must drive `no_healthy_backend` | High |
| 5 | Parameters must be explicable to a client review board | High |
| 6 | Behaviour must be identical across replicas in the operationally meaningful sense | Medium |

## Options Considered

### Option A — No breaker; rely on retries and timeouts

| Pros | Cons |
|---|---|
| Simplest | Every request pays the full timeout against a dead backend |
| No tuning | Concurrency is consumed by requests that will fail |
| | The failover tier is reached only after exhausting the deadline, so failover rarely helps |

Rejected on driver 1.

### Option B — Two-state breaker, closed and open, with a fixed cooldown

| Pros | Cons |
|---|---|
| Simple to reason about | Recovery is a cliff: at cooldown expiry, full traffic returns at once |
| | A backend that is still degraded is immediately re-saturated, and the breaker flaps |

Rejected on driver 3.

### Option C — Three-state breaker: closed, open, half-open

| Pros | Cons |
|---|---|
| Half-open probes test recovery with a bounded number of requests | More states to reason about and to expose as a metric |
| Recovery is gradual, so a still-degraded backend is not re-saturated | Probe count is another parameter to choose |
| Standard, well understood, explicable in review | |

### Option D — Adaptive breaker with a dynamically computed threshold

| Pros | Cons |
|---|---|
| Adapts to each backend's baseline error rate | Behaviour is hard to explain during an incident |
| Fewer parameters to choose | Hard to test; hard to reason about at 3 a.m. |
| | Fails in unexpected ways precisely when the environment is unusual |

Rejected on driver 5. During an incident, "the breaker opened because 10 consecutive requests failed"
is actionable. "The breaker opened because the adaptive threshold crossed" is not.

## Decision

**Three-state breaker per backend, with the `SPEC.md` §3.3 parameters.**

| Parameter | Value | Reasoning |
|---|---|---|
| Scope | **Per backend**, shared across the pod's traffic | Tripping protects every request, not only the one that discovered the problem. Per-request breakers would be useless |
| States | closed / open / half-open | Option C |
| Sliding window | **50 requests** | Large enough that a two- or three-request blip cannot trip it; small enough to react within seconds at production rates. At 100 req/s per pod that is a half-second of history |
| Failure-rate trip | **50% over the window** | The point at which a backend is doing more harm than good. A lower threshold trips on normal transient error rates; a higher one keeps a mostly-dead backend in rotation |
| Consecutive-failure trip | **10 consecutive failures** | Catches a hard-down backend fast, before the 50-request window fills. This is the branch that fires in a total outage |
| Open duration | **30 seconds** | Long enough that a restarting backend has a chance; short enough that a false positive costs one failover cycle, not minutes |
| Half-open probes | **5** | Enough signal to distinguish recovery from luck; small enough that a still-broken backend absorbs only five failures per cycle |
| Half-open success rule | **All 5 must succeed to close.** Any probe failure reopens | Conservative on purpose. Reopening is cheap; a premature close re-saturates a degraded backend and causes flapping |
| State scope | **Per pod, not shared** | See below |

### What counts as a failure

| Counts | Does not count |
|---|---|
| 5xx from the provider | 4xx other than 429 — a caller error is not a backend failure |
| Connection errors | `client_closed_request` — the caller left |
| Timeouts before first byte | Guardrail blocks |
| 429 **without** `Retry-After` | 429 **with** `Retry-After` — the provider is signalling correctly and we should honour it, not punish it |

The 429 distinction matters. A provider returning `429` with `Retry-After` is behaving well under
load; treating that as a failure would open breakers during exactly the periods when careful backoff
is what is needed.

### Breaker state is per pod

**[Decision]** Breaker state is **not** shared across pods.

| Argument for sharing | Why we did not |
|---|---|
| Faster fleet-wide reaction | Every pod sees the same backend and reaches the same conclusion within its own 50-request window — at production rates, within a second or two |
| One pod's discovery protects all | A shared breaker is a shared failure domain: a bug or a Redis blip could open every backend fleet-wide simultaneously |
| | Adds a Redis round trip to the hot path for a benefit measured in seconds |
| | A per-pod breaker degrades gracefully; a shared one has its own availability requirement |

The cost is that during the first second or two of a backend failure, different pods are in different
states. That is acceptable and self-correcting. `agentgate.breaker.state` is exported per pod, and
the alert aggregates with `max by (backend)`, so a partially-open fleet is still visible.

## Consequences

### Positive

- A hard-down backend is removed from selection within roughly 10 requests, so caller deadline is
  spent on the failover tier rather than on timeouts.
- Recovery is automatic and gradual; no human action is needed for a transient provider incident.
- `agentgate.breaker.state` gives a direct operational view, and `AgentGatePoolAllBackendsOpen` maps
  breaker state to the `no_healthy_backend` condition consumers see.
- Every parameter has a one-line justification that survives being asked about during an incident.

### Negative

- Six parameters is six things that can be wrong. They are configurable per backend, which is both a
  mitigation and a risk — divergent per-backend tuning is how a system becomes unexplainable.
- A backend with a genuinely high baseline error rate — some on-prem deployments — may trip more often
  than intended. Mitigated by per-backend overrides, used sparingly and reviewed.
- Per-pod state means a brief window of inconsistency across the fleet.
- The all-probes-must-succeed rule makes recovery slightly slower than a majority rule would. That is
  the intended conservatism.

### Neutral

- Breaker behaviour interacts with the retry budget and the failover tiers. The composite behaviour is
  documented in `02-flows.md` §5 and `03-sequences.md` §5, because no one of the three is
  understandable alone.

### What this forecloses

Adaptive or learned breaker thresholds. Recovering them is straightforward — the interface supports
it — but would cost the explicability that is driver 5.

## Revisit when

- A specific backend's measured baseline error rate approaches 50%, making the failure-rate trip
  meaningless for it.
- Observed flapping — more than three open/close cycles per hour for a backend that is not actually
  degraded — which would point at the half-open probe count.
- Provider p99 latency shifts such that 30 seconds of open is materially wrong in either direction.
