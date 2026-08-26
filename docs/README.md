# AgentGate — Documentation

AgentGate is the control plane every agent passes through, regardless of where that agent runs. One
plane with three faces: **Traffic** (`gateway`), **Trust** (`controlplane`), **Truth** (`fleetview`
plus the OTel pipeline).

**`SPEC.md` at the repository root is authoritative.** Everything in this directory expands it and
must agree with it exactly — names, error codes, headers, attribute names, policy stage order, gate
names. Where a document appears to disagree with `SPEC.md`, `SPEC.md` wins and the document is a
defect. Where `SPEC.md` is silent, the choice is marked **[Decision]** with the reasoning attached.

---

## The documents

| Document | What it is |
|---|---|
| [`01-architecture.md`](01-architecture.md) | Context, constraints, C4 levels 1–3, deployment topology, architectural principles, the twelve trade-offs taken with alternatives rejected, scaling and capacity model, failure domains and blast radius. |
| [`02-flows.md`](02-flows.md) | Nine operational flows as diagrams with walkthroughs and explicit failure branches: the 16-stage request, onboarding, promotion, quota lifecycle, failover, telemetry, chargeback, migration, incident response. |
| [`03-sequences.md`](03-sequences.md) | Fourteen message-level sequence diagrams with real component names: cold start, unary, streaming, both failover cases, breaker trip, quota settle, cache paths, guardrail blocks, registration, promotion, secret rotation, trace parentage, cost anomaly, shadow diffing. |
| [`04-gateway-contract.md`](04-gateway-contract.md) | **The published interface.** Every endpoint, field, header and error code with examples; streaming semantics; idempotency; the compatibility guarantee; copy-pasteable curl, Python and Go. |
| [`05-identity.md`](05-identity.md) | Agent identity model, the two issuance modes, RFC 8693 token exchange, claim reference, JWKS rotation, replay protection, scopes, the registration record, the promotion gate and its evidence, secret rotation, and an honest account of the delegation problem. |
| [`06-telemetry-schema.md`](06-telemetry-schema.md) | Resource attributes, span taxonomy and parentage, every attribute with type and cardinality, metric catalogue with a cardinality budget, log schema, content-capture policy, sampling policy, telemetry-trust metrics, a worked trace tree, and the managed-versus-self-hosted comparison. |
| [`07-slo-alerting.md`](07-slo-alerting.md) | SLIs with numerator, denominator and exclusions; SLOs; error budgets; burn-rate arithmetic; the full alert catalogue with PromQL, severity and runbook; on-call model; and the error-budget policy including what the team is forbidden from doing. |
| [`08-chargeback.md`](08-chargeback.md) | Usage record schema, pricing model and maintenance, rollups, the showback-to-chargeback maturity path, cache savings, disputes, reconciliation against the cloud bill, and the monthly export format. |
| [`09-migration.md`](09-migration.md) | The contract-preserving migration in operational detail: corpus, diff harness, shadow mechanics and cost, canary weights and automatic rollback triggers, per-consumer cutover checklist, comms plan, decommission criteria, and twelve risks with mitigations. |
| [`10-network-security.md`](10-network-security.md) | Fifteen network paths with FQDNs, ports and protocols; DNS and split-horizon strategy; TLS and certificate lifecycle; data classification per boundary; STRIDE threat model for the gateway and control plane; compliance controls mapped to what the code actually does. |
| [`11-delivery-plan.md`](11-delivery-plan.md) | Phase 0 through GA with outcome, deferrals, exit criteria and risks per phase; the decide-now versus stay-open table; and the written-handover protocol for a distributed team. |
| [`adr/`](adr/) | Thirteen decision records plus the template. Context, drivers, options, decision, consequences, revisit trigger. |

### Decision records

| ADR | Decision |
|---|---|
| [0001](adr/0001-openai-compatible-contract.md) | OpenAI-compatible wire contract, frozen and additive-only |
| [0002](adr/0002-go-for-gateway-and-control-plane.md) | Go for the gateway and control plane |
| [0003](adr/0003-one-plane-three-faces.md) | One plane with three faces, not three independent services |
| [0004](adr/0004-token-bucket-quota-reserve-settle.md) | Token-aware quota with reserve and settle |
| [0005](adr/0005-redis-for-distributed-limits.md) | Redis for distributed rate limits, quota and cache |
| [0006](adr/0006-circuit-breaker-parameters.md) | Circuit breaker shape and parameters |
| [0007](adr/0007-tail-sampling-policy.md) | Tail sampling at the collector gateway tier |
| [0008](adr/0008-content-capture-off-by-default-in-prod.md) | Content capture off by default in production |
| [0009](adr/0009-semantic-cache-default-off.md) | Semantic cache off by default |
| [0010](adr/0010-fail-closed-guardrails-for-restricted-data.md) | Guardrail failure mode per pool; fail-closed for restricted data |
| [0011](adr/0011-two-party-promotion-approval.md) | Two-party approval for production promotion |
| [0012](adr/0012-both-managed-and-self-hosted-observability.md) | Run both managed and self-hosted observability, split by question |
| [0013](adr/0013-strangler-migration-with-frozen-contract.md) | Strangler migration with the contract held fixed |

---

## Reading order

### If you are an engineer whose agent will call the gateway

You need to integrate correctly and get your agent into production. You do not need the internals.

| Order | Document | Why |
|---|---|---|
| 1 | [`04-gateway-contract.md`](04-gateway-contract.md) | The whole thing. It is the interface you are writing against. Pay particular attention to §1 (what will never change), §6.4 (the first-content-byte rule), §9 (how `max_tokens` affects your quota) and §12 (the pre-ship checklist) |
| 2 | [`05-identity.md`](05-identity.md) §§1–3, 7, 8 | How to get a token, what claims it carries, what the registration manifest must contain, and what the promotion gate will ask of you |
| 3 | [`02-flows.md`](02-flows.md) §§2–4 | Onboarding, promotion, and why a big `max_tokens` throttles you |
| 4 | [`06-telemetry-schema.md`](06-telemetry-schema.md) §§2, 4.7, 9 | What your SDK must emit. **`traceparent` propagation and telemetry completeness are promotion-gate inputs** — this is the section that catches teams out |
| 5 | [`08-chargeback.md`](08-chargeback.md) §§5, 7 | What your consumption costs, what the cache saves you, and how to query it |
| 6 | [`05-identity.md`](05-identity.md) §10.3 | If your agent handles per-user data: AgentGate does **not** implement on-behalf-of. Read this before assuming it does |

**Skip:** 01, 03, 07, 09, 10, 11, and the ADRs, unless you are curious.

### If you are a platform engineer building or running AgentGate

| Order | Document | Why |
|---|---|---|
| 1 | `SPEC.md` | Authoritative. Read it completely before anything here |
| 2 | [`01-architecture.md`](01-architecture.md) | The whole shape, and §7's trade-offs so you know what was rejected and why |
| 3 | [`04-gateway-contract.md`](04-gateway-contract.md) | The contract is frozen. Everything you build is constrained by it |
| 4 | [`02-flows.md`](02-flows.md) | Decision structure and failure branches for every flow |
| 5 | [`03-sequences.md`](03-sequences.md) | Message-level detail. §§4a/4b are the streaming rule you must not get wrong |
| 6 | [`06-telemetry-schema.md`](06-telemetry-schema.md) | Span and metric names are an operational contract. §5.1's cardinality budget binds from day one |
| 7 | [`07-slo-alerting.md`](07-slo-alerting.md) | What you are on call for, and §8's error-budget policy |
| 8 | [`05-identity.md`](05-identity.md) | The trust plane in full |
| 9 | [`11-delivery-plan.md`](11-delivery-plan.md) | Sequencing, what is deliberately deferred, and §11's handover protocol |
| 10 | [`09-migration.md`](09-migration.md) | If you are on the migration |
| 11 | [`08-chargeback.md`](08-chargeback.md) | If you are on cost |
| 12 | [`10-network-security.md`](10-network-security.md) | Before touching any network path |
| 13 | [`adr/`](adr/) | Read 0001, 0003 and 0004 first; the rest as they become relevant |

### If you are a client security reviewer

Ordered so that the trust boundaries come before the mechanics.

| Order | Document | What you are looking for |
|---|---|---|
| 1 | [`10-network-security.md`](10-network-security.md) | Every path with its FQDNs, ports, data classification and required review artefact. §6's STRIDE tables for the gateway and control plane, including §6.3's two most consequential residual risks. §7 maps compliance controls to what the code does, and **§7.1 lists what is policy rather than code** |
| 2 | [`05-identity.md`](05-identity.md) | Identity model, both issuance modes, claim verification, JWKS rotation, replay protection. §8 is the promotion gate and its audit evidence. §10 explains how this differs from conventional service-to-service auth, and **§10.3 states the delegation gap honestly** |
| 3 | [`06-telemetry-schema.md`](06-telemetry-schema.md) §7 | The content-capture policy and its **three independent enforcement controls**. This is where a reviewer usually starts asking hard questions |
| 4 | [`01-architecture.md`](01-architecture.md) §§1, 6, 9 | Constraints, principles, and the failure-domain table with each documented degradation |
| 5 | [`adr/0008`](adr/0008-content-capture-off-by-default-in-prod.md), [`0010`](adr/0010-fail-closed-guardrails-for-restricted-data.md), [`0011`](adr/0011-two-party-promotion-approval.md) | Content capture, fail-closed guardrails, two-party approval — the three decisions with the most direct compliance consequence, each with its rejected alternatives |
| 6 | [`04-gateway-contract.md`](04-gateway-contract.md) §§1, 7 | The frozen contract and the full error catalogue |
| 7 | [`08-chargeback.md`](08-chargeback.md) §8.3 | Why an unmetered path in reconciliation is treated as a **security finding**, not a billing variance |
| 8 | [`09-migration.md`](09-migration.md) §§4, 8.3 | Shadow-traffic handling, and why unattributed traffic blocks decommission |
| 9 | [`07-slo-alerting.md`](07-slo-alerting.md) §§5.6, 8.2 | Security alerts, and what the error-budget policy forbids |

**Where to look first for the answer to "what could go wrong":** `10-network-security.md` §6.3 and
`01-architecture.md` §9.

---

## Conventions used throughout

| Convention | Meaning |
|---|---|
| **[Decision]** | `SPEC.md` is silent here. A choice was made and the reasoning is stated inline |
| Policy stage numbers | Always the `SPEC.md` §3.2 order, 1 through 16, never renumbered |
| Error codes | Always the frozen `code` values from `SPEC.md` §2.4 |
| Diagrams | Mermaid, rendered inline. Quoted node labels throughout |
| Tables over prose | Wherever the content is tabular |
| Tone | Written for a senior engineer and a client security reviewer simultaneously. Precise, no marketing |

## Where the honest gaps are

Stated here so nobody has to find them by accident:

| Gap | Where it is discussed |
|---|---|
| No on-behalf-of / delegation. An agent's blast radius is its own entitlements, not the intersection with its caller's | [`05-identity.md`](05-identity.md) §10.3 |
| Production prompt-level debugging is not available; content capture is off | [`06-telemetry-schema.md`](06-telemetry-schema.md) §7, [`adr/0008`](adr/0008-content-capture-off-by-default-in-prod.md) |
| Quota fails **open** during a Redis outage, with an alert. It is a cost control, not a safety control | [`01-architecture.md`](01-architecture.md) §9, [`adr/0005`](adr/0005-redis-for-distributed-limits.md) |
| Replay protection degrades to per-pod during a Redis outage | [`05-identity.md`](05-identity.md) §5 |
| The control plane is the trust root; its compromise is total | [`10-network-security.md`](10-network-security.md) §6.3 |
| Per-request cost figures are **list price**; discounts are applied at cost-centre level | [`08-chargeback.md`](08-chargeback.md) §3.3 |
| A specific uninteresting request may not be retrievable as a trace | [`06-telemetry-schema.md`](06-telemetry-schema.md) §8.3 |
| Shadow traffic doubles model spend for its duration | [`09-migration.md`](09-migration.md) §4.2 |
| Prompt-injection resistance is partial; model output correctness is out of scope | [`10-network-security.md`](10-network-security.md) §7.1 |
