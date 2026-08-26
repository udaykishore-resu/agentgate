# ADR 0002 — Go for the gateway and control plane

**Status:** Accepted
**Date:** 2026-06-04
**Deciders:** Platform lead, client architecture review board
**Consulted:** Client platform engineering, client security
**Affects:** `cmd/`, `internal/`, hiring, the client's run function

---

## Context

AgentGate's traffic plane is a high-concurrency streaming proxy in front of model providers. Its
control plane is a low-throughput service holding audit-grade state. Both must be operated by a small
senior team and eventually handed to the client's run function.

The competing approach is not another language — it is not writing a service at all, and instead
expressing routing, quota, retries and attribution as policy expressions in the cloud's managed
AI-gateway product. That option is covered in detail here because it is the one a client architecture
board will ask about first.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Streaming, high-concurrency I/O with predictable latency and memory | Very high |
| 2 | The behaviours we need must be *expressible* and *unit-testable* | Very high |
| 3 | Cloud neutrality is a stated requirement | High |
| 4 | Small senior team: operational simplicity of the artefact matters | High |
| 5 | First-class OpenTelemetry support | High |
| 6 | Hand-over to the client's run function | Medium |
| 7 | Raw single-core throughput | Low. The workload is I/O-bound |

## Options Considered

### Option A — Go

| Pros | Cons |
|---|---|
| Goroutine-per-stream is the natural model for thousands of concurrent SSE streams | GC pauses exist, though `GOMEMLIMIT` and modern Go make them a non-issue at this scale |
| Static binary, small container, fast start, trivial deployment | Less expressive type system than Rust or Kotlin |
| Predictable memory; `GOMEMLIMIT` gives a hard ceiling | Generics are recent and the ecosystem still shows it |
| Mature, first-class OpenTelemetry SDK and collector ecosystem | |
| Excellent `net/http` and HTTP/2 support, including server-sent events | |
| Standard in cloud-native infrastructure, so the client's platform engineers already read it | |
| Straightforward concurrency testing with the race detector | |

### Option B — Managed API management with AI-gateway policy expressions

Express the policy chain as policy expressions in Azure API Management or an AWS equivalent.

| Pros | Cons |
|---|---|
| No service to operate, patch or scale | **Token-aware reserve/settle quota is not expressible** |
| Already reviewed and present in the client's environment | **Streaming-windowed guardrail scanning is not expressible** |
| Native integration with the client's edge controls | **Circuit-breaker state shared across replicas is not expressible** |
| | **Per-stage timing attributes are not expressible** |
| | Policy expressions are close to untestable — no unit tests, no race detector, no local run |
| | The least portable artefact in either cloud; directly contradicts driver 3 |
| | Debugging is by trial and error against a deployed instance |

Rejected. The four "not expressible" rows are not incidental features; they are most of what
distinguishes AgentGate from a reverse proxy.

### Option C — Rust

| Pros | Cons |
|---|---|
| Best-in-class latency and memory predictability, no GC | Team velocity for a small senior team on a time-critical delivery |
| Strong type system catches whole error classes | OpenTelemetry ecosystem less mature than Go's |
| | Hiring and hand-over into a client run function is materially harder |
| | The workload is I/O-bound; Rust's advantage is largely in the part that is not the bottleneck |

Rejected on drivers 4 and 6. Genuinely a close call on driver 1, and it would be the choice if the
gateway were CPU-bound.

### Option D — JVM, Kotlin or Java with a reactive stack

| Pros | Cons |
|---|---|
| Very mature ecosystem; likely already in the client's estate | Heavier runtime footprint and slower start, which matters for HPA responsiveness |
| Strong observability tooling | Reactive streaming code is harder to reason about than goroutines |
| Large hiring pool | Memory tuning for thousands of concurrent streams is more involved |

Rejected on drivers 1 and 4, though it is the option a client with a Java-heavy estate would push
for.

### Option E — Node or Python

| Pros | Cons |
|---|---|
| Fastest to prototype; the AI ecosystem is Python-first | Single-threaded event loop or the GIL for a high-concurrency proxy |
| | Streaming under load is where both languages are weakest |
| | Memory per connection is significantly higher |
| | Dependency and supply-chain surface is larger, which matters in this client's review |

Rejected on driver 1.

## Decision

**Go for `gateway`, `controlplane`, `fleetview`, `guardrails`, and the CLIs.**

Implementation constraints that follow:

1. Cloud-specific behaviour lives behind interfaces in `internal/provider`, `internal/store` and
   `internal/identity`. **No cloud SDK type appears in `internal/gateway/policy`.**
2. `GOMEMLIMIT` set to 80% of the container memory limit, with a hard in-flight ceiling below it that
   triggers load shedding.
3. Race detector on in CI for all concurrency-touching packages.
4. OpenTelemetry Go SDK, no wrapper abstraction over it — a telemetry abstraction layer would defeat
   the semantic conventions.
5. Dependencies kept minimal and pinned; each addition is a reviewed decision because the client
   reviews the dependency tree.

The managed API-management product remains available **at the edge** for org-standard concerns —
WAF, subscription keys, org-wide throttling — in front of an unchanged gateway. That is an additive
layer, not an alternative implementation.

## Consequences

### Positive

- The policy chain is ordinary Go, so it is unit-testable, locally runnable, and profileable.
- Goroutine-per-stream makes the streaming implementation, including the first-content-byte failover
  rule and windowed guardrail scanning, straightforward to express and to reason about.
- Static binaries and small containers make deployment and rollback fast, which the migration relies
  on.
- Cloud neutrality is achievable in practice, not just in principle.
- The client's platform engineers can read the code without new skills.

### Negative

- We own the code, the CVEs, the performance work and the on-call. The managed option would have
  outsourced all four.
- Go's error handling is verbose, and the policy chain has many error paths. Mitigated by typed
  errors mapping directly to the frozen `code` values.
- A GC language means tail-latency work is a permanent, if minor, concern.
- If the client's estate is JVM-heavy, this is a skills outlier for their run function.

### Neutral

- Go's type system is less expressive than Rust's or Kotlin's. For a service whose complexity is in
  concurrency and policy rather than in domain modelling, this has not been a constraint.

### What this forecloses

Sub-millisecond, GC-free tail latency. Recovering it would mean a Rust rewrite of the hot path. Given
provider latency is 300–3000 ms and the overhead budget is 60 ms, this is not a constraint we expect
to hit.

## Revisit when

The measured p99 gateway overhead is dominated by GC pauses rather than by policy work — visible in
the per-stage duration attributes showing time unaccounted for between stages. Nothing else reopens
this.
