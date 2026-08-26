# ADR 0003 — One plane with three faces, not three independent services

**Status:** Accepted
**Date:** 2026-06-05
**Deciders:** Platform lead, client architecture review board
**Consulted:** Client platform engineering
**Affects:** Repository layout, release process, `01-architecture.md`, team structure

---

## Context

AgentGate has three responsibilities: model traffic (`gateway`), agent identity and promotion
(`controlplane`), and fleet visibility, SLO and cost (`fleetview`).

A conventional microservice decomposition would make these three independently owned, independently
versioned services with their own APIs and release trains. That is the default answer in most
organisations, and it is worth stating clearly why it is the wrong one here.

The forcing question is where identity is enforced and where telemetry originates. Both answers are
"at the gateway". A decomposition that puts the identity decision in a remote service on the hot
path, and asks a remote telemetry service to reconstruct what the gateway already knows, creates
exactly the seam this platform exists to remove.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Identity is enforced *at* the gateway and telemetry originates *at* the gateway | Decisive |
| 2 | A control-plane outage must not stop model traffic | Very high |
| 3 | Small senior team; three integration surfaces is a cost with no return at this size | Very high |
| 4 | Blast radius of a bad release must stay bounded | High |
| 5 | The client's run function will eventually operate this | Medium |
| 6 | Independent scaling of the three concerns | Medium |
| 7 | Independent release cadence | Low today; may rise |

## Options Considered

### Option A — One repository, one release train, three binaries sharing `internal/`

| Pros | Cons |
|---|---|
| Shared types for claims, registration and telemetry are compile-time consistent | A bad shared-package change can affect all three |
| Identity and telemetry semantics cannot drift between planes | One release train can become a queue as the team grows |
| One integration surface for a five-person team | Requires discipline to keep module boundaries clean without process to enforce them |
| Separate deployables mean independent rollout and scaling anyway | |
| A change spanning all three is one pull request and one review | |

### Option B — Three independently versioned services with their own APIs

| Pros | Cons |
|---|---|
| Textbook separation of concerns | The gateway calls a remote identity service on the hot path, or duplicates its logic |
| Independent release cadence | Telemetry semantics must be re-agreed across a service boundary and will drift |
| Independent team ownership at scale | Three APIs to version, document, test and keep compatible |
| | Three deployment pipelines, three sets of runbooks, three on-call surfaces, for five engineers |
| | A cross-cutting change becomes a coordinated multi-repository release |

Rejected on drivers 1 and 3. This would be the right answer at four times the team size.

### Option C — One binary containing all three

| Pros | Cons |
|---|---|
| Simplest possible deployment | Control-plane load and traffic load scale together, which is wrong |
| No inter-component network calls at all | A control-plane bug takes down model traffic — directly violates driver 2 |
| | Wildly different resource profiles in one pod |
| | Blast radius maximised |

Rejected on driver 2.

### Option D — Gateway separate; control plane and fleetview combined

| Pros | Cons |
|---|---|
| Two deployables instead of three | fleetview's background rollup work and the control plane's audit-grade write path have different failure tolerances |
| Recognises that the gateway is the special one | A fleetview rollup consuming memory should not risk the promotion audit trail |

Considered seriously. Rejected because the control plane holds audit evidence and must be the most
boring, most stable component in the system; sharing a process with a batch workload compromises
that.

## Decision

**One repository, one release train, three separately deployed binaries sharing `internal/` packages.
The gateway degrades gracefully when the other two are unavailable.**

Concretely:

1. `cmd/gateway`, `cmd/controlplane`, `cmd/fleetview`, `cmd/guardrails`, plus the tools, all built
   from one module.
2. Shared domain types — claims, registration record, usage record, telemetry attribute keys — live
   in `internal/` and are used by all three, so they cannot drift.
3. The three are **deployed and scaled independently**. One release train does not mean one rollout.
4. **The gateway never depends on the control plane per request.** JWKS is cached with a 10-minute
   TTL and served stale for up to 24 hours during an outage. Registry entries are cached for 30
   seconds with negative caching.
5. **The gateway never depends on fleetview at all.** Telemetry is fire-and-forget through the
   collector.
6. Module boundaries are enforced in CI: `internal/gateway/policy` may not import `internal/registry`
   storage internals, only its interface; no cloud SDK types cross into policy code.

## Consequences

### Positive

- Claims, registration records and telemetry attributes are compile-time consistent across all three
  planes. The most common failure mode in the decomposed alternative — a claim name that means one
  thing to the issuer and another to the verifier — cannot occur.
- A cross-cutting change is one pull request with one review.
- Five engineers maintain one integration surface, one CI pipeline and one dependency tree.
- Model traffic survives a total control-plane outage for up to 24 hours, and a total telemetry
  outage indefinitely.

### Negative

- A defect in a shared `internal/` package can affect all three binaries. Mitigated by independent
  rollout — the gateway is canaried first, and the control plane follows only after.
- One release train can become a queue. It has not yet; it would be the primary signal to revisit.
- Team ownership boundaries are conventional rather than structural. With five senior engineers this
  is manageable; with twenty it would not be.
- A reviewer expecting microservices will ask about this. The answer is drivers 1 and 3, and it needs
  to be given confidently.

### Neutral

- Independent scaling is preserved despite the shared repository, because the deployables are
  separate. The usual argument for decomposition — independent scaling — does not actually require
  independent repositories.

### What this forecloses

Independent team ownership with independent release cadence. Recovering it means splitting the
repository, which is mechanical for the binaries and genuinely difficult for the shared `internal/`
types — those would become a published library with its own versioning and compatibility problem.
Estimated cost: a quarter of engineering time, most of it spent on the shared types.

## Revisit when

Any one of:

- The release train is measurably a queue — more than one week's median wait between "ready" and
  "shipped" for changes to one plane, caused by another plane.
- Any plane requires a materially different release cadence, for example a regulatory requirement
  that the control plane changes only monthly.
- The team exceeds roughly twelve engineers, at which point conventional ownership boundaries stop
  holding on their own.
