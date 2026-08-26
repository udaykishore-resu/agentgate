# ADR 0005 — Redis for distributed rate limits, quota and cache

**Status:** Accepted
**Date:** 2026-06-12
**Deciders:** Platform lead
**Consulted:** Client platform engineering, client security
**Affects:** Stages 5, 6, 8, 14, and the `jti` replay set at stage 2; pod sizing; `01-architecture.md` §9

---

## Context

ADR 0004 requires a shared ledger for token reservations. The gateway runs as a horizontally scaled
stateless pool, so the ledger must be external. The same store is a natural home for the exact
response cache and the `jti` replay set.

The store sits on the hot path of every request, so its latency is charged against the 60 ms overhead
budget, and its availability must have a defined degradation.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Atomic read-modify-write across replicas for reserve and settle | Decisive |
| 2 | Sub-millisecond typical latency on the hot path | Very high |
| 3 | A defined, safe behaviour when the store is unavailable | Very high |
| 4 | Available as a managed service in both clouds | High |
| 5 | Identical semantics for local and dev without the dependency | High |
| 6 | Operationally familiar to the client | Medium |

## Options Considered

### Option A — In-memory per-pod buckets with the limit divided by replica count

| Pros | Cons |
|---|---|
| Zero latency, zero dependency | **Wrong under uneven load balancing** — one pod exhausts while others idle |
| Trivially available | **Wrong during scale events** — the divisor changes underneath the limit |
| | **Wrong during rolling deploys** — old and new pods both hold a share |
| | Reserve/settle is meaningless without a shared ledger |
| | Wrong precisely when a quota matters most: during a surge |

Rejected on driver 1. Retained as the **local and dev** implementation, behind the same interface with
identical semantics.

### Option B — Redis with an atomic Lua script

| Pros | Cons |
|---|---|
| Lua gives true atomicity for the compound reserve operation | A hot-path dependency needing a degradation story |
| Sub-millisecond p50 in-region | Single-primary write path; failover is seconds of unavailability |
| Managed in both clouds — Azure Cache for Redis, ElastiCache | Cluster mode requires care with multi-key scripts |
| Natural fit for the cache and the replay set as well | Persistence semantics need to be chosen deliberately |
| Universally operationally familiar | |

### Option C — PostgreSQL with row-level locking

| Pros | Cons |
|---|---|
| Already present for the control plane | Millisecond-scale latency at best; worse under contention |
| Strong durability | Row locks on a hot key serialise the entire fleet's traffic for that agent |
| One less component | Wrong tool: this is an ephemeral counter, not a durable record |

Rejected on driver 2.

### Option D — A cloud-native distributed counter service

| Pros | Cons |
|---|---|
| Fully managed | Not available equivalently in both clouds; contradicts cloud neutrality |
| | Compound atomic operations are awkward or unavailable |
| | Latency generally worse than Redis |

Rejected on driver 4.

### Option E — Local buckets with periodic reconciliation against a shared store

Each pod holds a local allocation and reconciles asynchronously.

| Pros | Cons |
|---|---|
| Removes the store from the hot path | Overshoot between reconciliation intervals |
| Survives store outages naturally | Materially more complex: allocation, reclamation, rebalancing |
| | Premature. The Redis round trip is not yet a measured problem |

**Rejected for now, retained as the documented next step** if Redis latency becomes material.

## Decision

**Redis, managed, HA, with a single atomic Lua script per request covering both the RPM and TPM
checks. In-memory buckets with identical semantics for local and dev.**

| Aspect | Decision |
|---|---|
| Deployment | Managed HA with automatic failover, TLS in transit, AOF persistence |
| Atomicity | One Lua script per request performing the RPM check, the TPM check, the monthly budget check, and the reservation write |
| Key scheme | `rl:{tenant:team:agent:env}` for buckets, `resv:{reservation_id}`, `cache:{tenant}:{sha256}`, `jti:{jti}`. Hash tags keep a request's keys on one slot under cluster mode |
| Connection pool | 64 per pod, tuned from the Phase 1 load baseline |
| Persistence | AOF with `everysec`. Losing one second of quota state is acceptable; losing all of it is not |
| Cache TTL | Per pool policy, default 300 s |
| Replay set TTL | The token's remaining lifetime |

### Degradation when Redis is unavailable

| Function | Behaviour | Reasoning |
|---|---|---|
| Rate limiting and quota | **Fail open**, with a hard per-pod local ceiling as a backstop, the decision recorded on `agentgate.ratelimit.decisions`, and a P1 alert | A quota is a **cost control**, not a safety control. A safety control fails closed; a cost control fails open with an alarm. Failing all model traffic closed to protect a budget is the wrong trade, and the monthly budget reconciles from usage records on a separate path |
| Exact cache | **Degrades to miss** | Always safe. Costs money, never correctness |
| `jti` replay set | **Degrades to per-pod in-memory**, with an alert | Signature, issuer, audience and expiry are still fully verified. Replay is the only weakened property, bounded by the 15-minute TTL. Discussed honestly in `05-identity.md` §5 |

**This is the most consequential paragraph in this ADR** and it is the one a security reviewer should
read first. The fail-open decision for quota is deliberate and is defended on the grounds that the
alternative — a Redis blip becoming a fleet-wide model-traffic outage — is a worse outcome for the
client than a bounded period of unenforced cost limits with an alarm raised.

## Consequences

### Positive

- Reserve/settle is correct across replicas, scale events and rolling deploys.
- One script per request keeps hot-path cost to a single round trip, roughly 0.5–1.5 ms in region.
- The same store serves the cache and the replay set, so there is one dependency rather than three.
- The in-memory implementation makes local development and integration tests dependency-free while
  exercising identical semantics.

### Negative

- A hot-path dependency whose failure has to be reasoned about. Every degradation above is a
  compromise that must be defended in review.
- Redis failover is seconds of write unavailability, during which quota is unenforced.
- Redis becomes a scaling axis of its own; the bucket key shards naturally, but sharding is work.
- AOF persistence has a cost, and a Redis restart replays a log.

### Neutral

- Cluster-mode hash tags are required so a request's keys land on one slot. This constrains key
  naming but is otherwise invisible.

### What this forecloses

Nothing material. The `ratelimit.Store` interface abstracts the backend; the in-memory implementation
already proves the abstraction holds, and Option E remains reachable behind the same interface.

## Revisit when

Any of:

- p99 Redis latency exceeds 5 ms sustained, making it a material share of the 60 ms overhead budget.
- A single Redis primary approaches its throughput ceiling — expect this above roughly 20k req/s.
- Redis availability becomes the dominant contributor to the availability error budget, at which
  point Option E's local pre-reservation moves from "next step" to "now".
