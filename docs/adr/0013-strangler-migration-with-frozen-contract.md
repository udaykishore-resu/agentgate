# ADR 0013 — Strangler migration with the contract held fixed

**Status:** Accepted
**Date:** 2026-07-20
**Deciders:** Platform lead, client change management, client architecture review board
**Consulted:** Every consuming team, client security, FinOps
**Affects:** `09-migration.md`, `11-delivery-plan.md`, the compatibility corpus, routing configuration

---

## Context

A first-generation gateway is in production and consuming teams integrate against it. AgentGate
replaces it. The consuming teams are not resourced to change code, the delivery is time-critical, and
in this environment every consumer-side change is a change record with its own review and approval.

ADR 0001 established that AgentGate v1 *is* the legacy contract. This ADR establishes how traffic
actually moves from one implementation to the other.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | No consumer changes code | Decisive |
| 2 | Rollback must be fast enough to be used without hesitation | Decisive |
| 3 | A consumer must never see two behaviours at once | Very high |
| 4 | Differences must be found before a consumer experiences them | Very high |
| 5 | The client's change process must be able to absorb the migration | High |
| 6 | Calendar predictability | Medium |

## Options Considered

### Option A — Big-bang cutover

Switch all traffic at a scheduled window.

| Pros | Cons |
|---|---|
| Shortest calendar | **Every unknown difference is discovered simultaneously, in production** |
| One change record | Rollback is a deploy and a scheduled window |
| No dual-running cost | Blast radius is the entire estate |
| | Requires a level of confidence no compatibility test can justify |

Rejected on drivers 2 and 4.

### Option B — Per-endpoint migration

Move `/v1/embeddings` first, then `/v1/chat/completions`.

| Pros | Cons |
|---|---|
| Smaller units | **A consumer sees two implementations simultaneously** |
| Endpoint-level isolation | When something breaks, the consumer cannot tell which implementation caused it |
| | Cross-endpoint state — quota, identity, cost attribution — is split across two systems |

Rejected on driver 3.

### Option C — Strangler: shadow, then per-consumer canary, then cutover, with the contract frozen

| Pros | Cons |
|---|---|
| Differences found in shadow, with no consumer impact | Longest calendar |
| Rollback is a weight change at the routing layer — seconds, not a deploy | Dual-running cost, including doubled model spend during shadow |
| Blast radius bounded by canary weight and by one consumer | Requires a diff harness and a compatibility corpus |
| A consumer always sees exactly one implementation | Requires every consumer to be identified |

### Option D — Strangler by feature flag inside the legacy gateway

Route internally rather than at the network layer.

| Pros | Cons |
|---|---|
| Fine-grained control | Rollback depends on the legacy gateway being healthy — the thing being replaced |
| No routing-layer change | Requires modifying the legacy service, which nobody wants to touch |
| | Couples the migration's safety to the system being decommissioned |

Rejected on driver 2.

## Decision

**Strangler migration: freeze and document, compatibility suite, shadow, per-consumer canary,
per-consumer cutover, staged decommission. Rollback at every stage is a weight change at the routing
layer, never a deploy.**

| Phase | Consumer impact | Rollback |
|---|---|---|
| 1 Freeze and document | None | n/a |
| 2 Compatibility suite | None | n/a |
| 3 Shadow — mirrored traffic, responses discarded | None | Turn off the mirror |
| 4 Canary — 1% → 5% → 25% → 50% → 100% **per consumer** | Bounded by weight | Weight change, seconds |
| 5 Cutover — recorded per consumer | Full for that consumer | Weight change, seconds |
| 6 Decommission — read-only, stopped, deleted | None | Restore from stopped state, 44 days |

Non-negotiable rules:

1. **Cutover is per consumer, never per endpoint.**
2. **Rollback is a weight change**, at the DNS or front-door layer, outside both implementations.
3. **Weight advancement is a human decision** after reviewing the criteria; **rollback is automatic.**
4. **After any automatic rollback, advancement is blocked pending human review.** An automatic system
   that rolls back and then automatically retries is a system that flaps.
5. **Unattributed traffic stays on legacy and blocks decommission** until identified. Migrating
   traffic whose owner is unknown means having nobody to call when it breaks.
6. **Zero critical or high contract diffs** is the bar at every gate, not a target.
7. **Decommission is staged** — read-only, then stopped, then deleted — keeping the decision reversible
   for 44 days after the last cutover.

## Consequences

### Positive

- Differences are found in shadow, at production volume, with real providers, before any consumer
  experiences them.
- Rollback in seconds means the canary steps are safe enough for automatic rollback triggers, which in
  turn means the response to a problem does not depend on a human being awake.
- Blast radius at every moment is bounded by the current canary weight and by a single consumer.
- Each consumer's cutover is an independently reviewable change, which fits the client's change
  process rather than fighting it.
- The staged decommission means the irreversible step is separated from the migration by six weeks and
  its own sign-off.

### Negative

- **Longest calendar of any option**, dominated by soak times and the client's change process rather
  than by engineering. Phase 6 is estimated at 10–16 weeks.
- **Shadow costs real money.** Mirroring 100% of traffic doubles model spend for the shadow period.
  This must be funded in Phase 0; the sampled-shadow alternative trades calendar for cost.
- Requires building and maintaining a compatibility corpus and a diff harness — real engineering that
  produces nothing shipped to a consumer.
- Every consumer must be identified. Where identification is hard, the migration stalls, which is the
  intended behaviour but is still a stall.
- Dual-running means two systems to operate and monitor for months.

### Neutral

- Shadow requests are metered with `shadow: true` and excluded from every cost sum, so shadow spend is
  visible and separate rather than appearing as consumption growth.
- Shadowing is safe here only because model inference has no side effects beyond cost and quota. Any
  future endpoint with side effects cannot be shadowed this way, and that constraint is recorded
  against any such proposal.

### What this forecloses

A fast migration. Recovering speed means accepting either a big-bang cutover or a shorter shadow, both
of which trade directly against driver 4.

## Revisit when

Nothing reopens the strategy. The parameters within it — shadow duration, mirror rate, canary weights,
soak times — are tuned continuously against observed diff rates, and a consumer with a very simple,
well-understood traffic profile may be granted a compressed weight schedule on a case-by-case basis
with the platform lead's approval.
