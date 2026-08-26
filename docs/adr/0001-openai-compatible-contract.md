# ADR 0001 — OpenAI-compatible wire contract, frozen and additive-only

**Status:** Accepted
**Date:** 2026-06-02
**Deciders:** Platform lead, client architecture review board
**Consulted:** External engineering team, three consuming teams, client security
**Affects:** `api/openapi/gateway.v1.yaml`, `04-gateway-contract.md`, `09-migration.md`, every consumer

---

## Context

The client already runs a first-generation gateway. Consuming teams have integrated against it,
mostly using the OpenAI SDK or agent frameworks that assume the OpenAI Chat Completions shape.
AgentGate replaces that service.

The delivery is time-critical, and the consuming teams are not resourced to rewrite their
integrations. Whatever interface AgentGate publishes will be depended on immediately, and in a
regulated environment a change to a published interface is a change programme across many teams, not
a version bump.

A decision is forced now because the migration strategy, the compatibility test corpus and the
external engineering team's plans all depend on it, and because Phase 0 of the delivery plan cannot
start without it.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Consumers must not change code to migrate | Decisive on its own |
| 2 | Existing SDKs and agent frameworks must work unmodified | Very high |
| 3 | The migration must be contract-preserving, so the contract must be one that already exists | Very high |
| 4 | The interface must be stable for years in a regulated change environment | High |
| 5 | AgentGate concepts — policy, cost, guardrails, failover — need somewhere to live | Medium |
| 6 | Elegance of the resulting API | Low. Explicitly deprioritised |

## Options Considered

### Option A — Wire-compatible with the OpenAI Chat Completions shape, frozen, additive-only

Adopt the existing contract as `gateway.v1.yaml`. Extend only through `x-agentgate-*` headers, an
`agentgate` sub-object on discovery responses, and ignorable SSE `event:` frames.

| Pros | Cons |
|---|---|
| Zero consumer change to migrate | Inherits an API shape with no first-class place for policy metadata |
| Existing SDKs and frameworks work as-is | Extensions live in headers and side-channel frames, which is less discoverable |
| The migration becomes a routing exercise, not a coordination programme | We are bound to another organisation's API evolution decisions for interoperability |
| The external engineering team has nothing to learn | Some AgentGate concepts have no natural home in the body |
| A very large ecosystem of tooling assumes this shape | |

### Option B — Bespoke agent-native API

Design an API that expresses agent runs, tool loops, policy decisions, cost and provenance as
first-class concepts.

| Pros | Cons |
|---|---|
| Better shaped for what the platform actually does | Every consuming team rewrites, during a time-critical delivery |
| Policy metadata has a natural home | The migration becomes a multi-team coordination programme with no contract-preserving path |
| Not bound to another organisation's decisions | No SDK ecosystem; we would ship and maintain client libraries in several languages |
| | The legacy contract would still need supporting during transition, so we would run two |

Rejected on driver 1 alone.

### Option C — OpenAI-compatible plus a parallel native API

Serve both from day one.

| Pros | Cons |
|---|---|
| Migration path plus a better future shape | Two contracts to keep consistent, test and freeze |
| Consumers can adopt the native API when ready | The compatibility corpus and diff harness double |
| | A small senior team maintaining two interfaces during a migration |
| | Consumers would reasonably ask which one is strategic, and any answer is bad |

Rejected as premature. If a native API is ever right, it is `/v2`, informed by operating `/v1`.

### Option D — OpenAI-compatible but not frozen; evolve as needed

| Pros | Cons |
|---|---|
| Room to correct mistakes | Removes the property that makes the migration safe |
| | Consumers cannot rely on it, so they defensively pin, and pinning is worse than freezing |
| | In a regulated change environment, an evolving interface means an unbounded stream of change records for every consumer |

Rejected. The freeze *is* the product feature.

## Decision

**AgentGate v1 is wire-compatible with the OpenAI Chat Completions shape. The contract published as
`api/openapi/gateway.v1.yaml` is FROZEN and additive-only.**

Specifically:

1. Endpoint paths and methods, request and response field names and types, the set of error `code`
   values, and the HTTP status paired with each `code` do not change within `/v1`.
2. Fields may be **added** to responses. Nothing is removed, retyped, or repurposed.
3. Extensions use three mechanisms only: `x-agentgate-*` request and response headers, an `agentgate`
   sub-object on discovery responses, and SSE `event:` frames that a strict OpenAI client ignores.
4. Errors are RFC 9457 `application/problem+json` with a stable `code`, `request_id` and `trace_id`.
5. `model` always names a **logical** model, never a provider deployment name.
6. A breaking change means `/v2`, published at a separate base path, run side by side, with a
   published deprecation window. `/v1` is never modified to accommodate `/v2`.

## Consequences

### Positive

- Migration is contract-preserving: no consumer changes code, so no consumer must be scheduled.
- Consumers can use the OpenAI SDK, LangChain, Semantic Kernel or anything else that speaks the
  shape, without an AgentGate-specific client.
- The compatibility test corpus has an unambiguous pass criterion.
- Internal implementation freedom is bought with external rigidity: we can change providers, routing,
  caching and resilience without touching a consumer.

### Negative

- We inherit an API shape that has no first-class place for policy metadata, so AgentGate's most
  interesting information travels in headers and side-channel frames.
- We are bound to another organisation's API evolution for interoperability; if they change the
  streaming shape, we must decide between compatibility and correctness.
- Some concepts — a policy decision, a failover, a guardrail redaction — are second-class citizens on
  the wire.
- Mistakes in the contract are permanent within `/v1`, which raises the cost of Phase 0 being wrong.

### Neutral

- Consumers must ignore unknown fields, headers and SSE events. This is stated as a client
  requirement in `04-gateway-contract.md` §1.4 and is normal practice.
- `x-agentgate-provider` and `x-agentgate-model` are observability, not contract. Consumers are told
  explicitly not to depend on them for correctness.

### What this forecloses

An agent-native API in v1. Recovering it costs a `/v2` programme with a deprecation window and
per-consumer migration — precisely the programme this decision avoids. That is the right price for
the property we are buying.

## Revisit when

Only at `/v2` scoping, and only if two conditions both hold: a concrete capability we need is
genuinely inexpressible in the current shape (the leading candidate is delegation / on-behalf-of, see
`05-identity.md` §10.3), **and** the consuming teams have capacity for a migration. Neither alone is
sufficient.
