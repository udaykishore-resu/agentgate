# Load Test Scenario Matrix

Every scenario in this directory, with its hypothesis, method, pass criteria, and — the column
people skip and then regret — what it does **not** prove.

A test's value is bounded by what it can observe. Being explicit about the boundary is what keeps a
green suite from being mistaken for a guarantee.

---

## S1 — Smoke

| | |
|---|---|
| **Script** | `k6/smoke.js` |
| **Hypothesis** | The deployment implements the v1 contract correctly on every endpoint, on success and on error. |
| **Method** | One VU, five iterations. `/v1/models`, `/v1/token-count`, unary chat, streaming chat, `/v1/embeddings`, and a deliberate `unknown_model` to exercise the error path. Asserts the SPEC §2.3 header set on every response and the RFC 9457 problem+json shape on the error. |
| **Pass criteria** | Every check passes. `checks: rate==1.0`. No tolerance. |
| **What it proves** | The build is wired correctly: routing works, tokens validate, streaming produces a well-formed SSE body, errors carry stable codes and the standard headers. Safe as a deployment gate. |
| **What it does not prove** | Anything under concurrency. Anything about latency, capacity, isolation, or failure behaviour. A system can pass smoke and fall over at 10 rps. |
| **Run against** | Any environment including production, with a load-test identity. |

---

## S2 — Baseline

| | |
|---|---|
| **Script** | `k6/baseline.js` |
| **Hypothesis** | At expected production volume, AgentGate meets every SLO in SPEC §5 with headroom. |
| **Method** | Constant arrival rate at the target rpm for 30 minutes. Production-like mix: 60% streaming, 40% unary, 15% of total is embeddings. Prompt size jittered ±20% around the configured mean so a single cache key or token count cannot dominate. Cache off by default. |
| **Pass criteria** | Availability ≥ 99.9%; gateway overhead p95 < 60ms; TTFT p95 < 1200ms; token mint p95 < 150ms; contract headers 100%; `[DONE]` terminator 100%; failover rate < 2%; attempts p99 ≤ 3; **dropped iterations < 10**. |
| **What it proves** | The SLOs hold at this volume, on this build, on this infrastructure, with this backend. The result is quotable in the operational readiness review. |
| **What it does not prove** | Behaviour at 1.1x the tested volume. Behaviour over hours. Behaviour when anything fails. It says nothing about how close to the knee this volume sits — that requires S3, and without S3 a baseline pass is a data point with no context. |
| **Run against** | Staging at production shape. Never production. |

---

## S3 — Ramp to the knee

| | |
|---|---|
| **Script** | `k6/ramp.js` |
| **Hypothesis** | There is a concurrency level at which p95 gateway overhead breaches 60ms, and past it behaviour degrades predictably rather than collapsing. |
| **Method** | Ramping VUs, stepped: hold each level for 3 minutes with a 30s ramp between steps. One VU holds one in-flight request, so VUs and concurrency are the same number. Every sample is tagged with its concurrency level so per-level percentiles are recoverable. **No latency threshold** — breaching is the objective. |
| **Pass criteria** | The knee is found and recorded. Past the knee, degradation is via 429 load shedding, not 5xx: `agentgate_unexpected_5xx < 50` across the whole ramp. Contract headers hold at every level. |
| **What it proves** | The single most useful number this directory produces: the concurrency at which the SLO breaks. Everything about headroom policy is derived from it. Also shows the *shape* of failure — a gentle rise, a cliff, or a collapse — which determines how much warning production will give you. |
| **What it does not prove** | Sustained behaviour at any level; each step is only three minutes. The knee measured with synthetic backends is our knee, not the system's — a real provider's concurrency limits may bind first. Does not prove recovery after saturation; that is S5. |
| **Run against** | Isolated staging. This saturates shared infrastructure. |

---

## S4 — Soak

| | |
|---|---|
| **Script** | `k6/soak.js` |
| **Hypothesis** | Nothing accumulates. Latency at hour four matches latency at hour one, and memory is flat. |
| **Method** | Constant arrival rate at ~60% of expected volume for four hours. Samples are tagged `early` / `mid` / `late`; the summary compares first-window against last-window p95. Counts token refreshes explicitly, because a soak crosses at least one token expiry and a broken refresh path fails at minute 15 rather than hour four. |
| **Pass criteria** | p95 drift < 5% between early and late windows. SLOs hold across the whole window. Token refreshes > 0. Contract 100%. Paired with server-side checks: `container_memory_working_set_bytes` flat, `go_goroutines` flat, Redis memory stable. |
| **What it proves** | No leak in the request path, the stream handler, the connection pools, or the token cache. Token refresh works. Cache and Redis key growth is bounded. |
| **What it does not prove** | Anything about behaviour at high load — it runs deliberately below the knee. A leak with a longer time constant than four hours; a weekly job or a monthly boundary; anything triggered by a deploy, a failover, or a certificate rotation mid-run. Four hours is a compromise, not a proof of indefinite stability. |
| **Run against** | Staging, overnight. |

---

## S5 — Spike

| | |
|---|---|
| **Script** | `k6/spike.js` |
| **Hypothesis** | A 10x burst in ten seconds is absorbed by shedding batch-priority traffic first, per SPEC §3.3, and interactive traffic keeps working. |
| **Method** | Two concurrent arrival-rate scenarios — interactive and batch — sharing the same stage shape: 3 min baseline, 10s ramp to 10x, 3 min hold, 30s release, 4 min recovery. Each request is tagged with its tier and phase so shed rates are directly comparable. |
| **Pass criteria** | Batch shed rate materially exceeds interactive; interactive shed rate < 5%; every 429 carries `Retry-After`; zero unexpected 5xx; recovery latency returns to baseline within the 4-minute window. |
| **What it proves** | Priority-based load shedding is real and correctly ordered. The gateway degrades rather than collapsing. `Retry-After` is populated, so well-behaved clients back off instead of amplifying. |
| **What it does not prove** | That the *clients* honour `Retry-After` — that is a property of their SDKs, checked in the readiness review, not here. Does not prove behaviour for a spike sustained past three minutes, where provider quotas and autoscaling become the binding constraints. Does not prove the autoscaler reacts in time; HPA response is deliberately outside the window. |
| **Run against** | Isolated staging. |

---

## S6 — Failover

| | |
|---|---|
| **Script** | `k6/failover.js` |
| **Hypothesis** | When a backend fails, no caller finds out. Traffic moves to a surviving backend within the retry budget, and no 5xx that could have been failed over reaches a caller. |
| **Method** | Steady arrival rate while a backend is faulted — `mockprovider` fault injection by default, or a real `agentctl backend drain` in drain mode. Records the serving backend before the fault so "traffic moved" is a concrete comparison. Distinguishes `no_healthy_backend` (a capacity finding) from `provider_error` / `provider_timeout` (a failover defect). Asserts the streaming rule from SPEC §2.5: a failover frame may only appear before the first content byte. |
| **Pass criteria** | `agentgate_should_have_failed_over == 0`. Availability > 99.5% through the fault. `agentgate_backend_moved > 50%` — proof the fault landed. Attempts p99 ≤ 3, proving the retry budget bounds depth. Zero stream failovers after first content. |
| **What it proves** | Failover works under load and is invisible to callers. The retry budget bounds attempt depth rather than merely counting attempts. The streaming failover contract holds. |
| **What it does not prove** | Behaviour when *all* backends fail — that is a different test and its expected outcome is a clean `no_healthy_backend`, not failover. Does not prove the circuit breaker's timing characteristics; a breaker that trips slowly still passes this test if failover absorbs the traffic. **Does not cover timeout-shaped failures** with the default `FAULT_KIND=error`, which is exactly the gap that produced the incident in `docs/runbooks/postmortem-template.md`. Run it with `AGENTGATE_FAULT_KIND=timeout` as well; that variant is the more dangerous failure shape. |
| **Run against** | Staging with fault injection available. |

---

## S7 — Streaming

| | |
|---|---|
| **Script** | `k6/streaming.js` |
| **Hypothesis** | The SSE contract in SPEC §2.5 holds under sustained load, and TTFT p95 stays within 1200ms. |
| **Method** | Constant arrival rate, all streaming, with 25% long generations (1200 `max_tokens`) so streams run past 15 seconds and the heartbeat requirement actually applies. Parses the full SSE body: counts data frames, content chunks, heartbeats, checks for `[DONE]`, the usage frame, malformed JSON, and error frames. Cross-checks the usage frame's `completion_tokens` against `x-agentgate-tokens-output` — two independent code paths that must agree. |
| **Pass criteria** | TTFT p95 < 1200ms. `[DONE]` on 100% of streams. Usage frame on 100%. Zero malformed frames, zero streams without content, zero heartbeat gaps on streams over 16s, zero failover frames after first content, zero usage frames missing cost or tokens. Mean inter-token p95 < 250ms. |
| **What it proves** | Every element of the streaming contract that a consuming SDK depends on. Notably, that a stream never ends silently without `[DONE]` — the failure that causes agents to act on truncated model output without any error. |
| **What it does not prove** | Per-frame timing. k6 buffers the response body, so mid-stream stalls are invisible: a stream that delivers all its tokens in a two-second burst after a twenty-second pause passes every assertion here. For stall detection use the gateway's `agentgate_stream_intertoken_seconds_bucket` and [streaming-stalls.md](../../docs/runbooks/streaming-stalls.md). Also does not prove behaviour through an intermediate proxy that buffers — that is an ingress property and must be tested from outside the cluster. |
| **Run against** | Staging. Ideally also once from outside the ingress, to catch proxy buffering. |

---

## S8 — Multi-tenant fair share

| | |
|---|---|
| **Script** | `k6/multitenant.js` |
| **Hypothesis** | A runaway agent is contained by its own bucket and does not degrade anyone else. |
| **Method** | One arrival-rate scenario per agent, each with its own identity, quota and drive rate. Three well-behaved agents run at 60% of their quotas; a fourth runs at 20x its quota. Each agent sends a prompt containing a unique marker string with `temperature=0` and `x-agentgate-cache: on`, so a cache entry leaking across the boundary is detectable in the response body. |
| **Pass criteria** | Runaway limited on > 50% of its requests. **Zero** 429s for well-behaved agents. Well-behaved p95 within SLO while the runaway runs. **Zero** cross-agent cache hits. Rate-limit headers reflect each agent's own quota. |
| **What it proves** | Bucket keying is per `tenant:team:agent:env` and not collapsing agents together. Cache keys are tenant-scoped. One agent cannot consume another's quota. |
| **What it does not prove** | That well-behaved latency is unaffected by shared *concurrency* — this is the subtle one. The test asserts an absolute p95, but the honest comparison is against a control run with the runaway scenario removed. Run both and compare; a p95 that rises with the runaway present means the agents share something the quota does not govern, which is precisely the mechanism in the worked postmortem. Also does not prove isolation of Postgres, the collector, or the guardrail service — a runaway agent can saturate those without touching anyone's token bucket. |
| **Run against** | Staging with several registered load-test agents. |

---

## Coverage summary

| Property | Covered by | Confidence |
|---|---|---|
| Wire contract, success paths | S1, S2 | High |
| Wire contract, error paths | S1 | Medium — only `unknown_model` is exercised deliberately; other codes appear opportunistically |
| SLO conformance at target volume | S2 | High |
| Capacity limit | S3 | High for our overhead; medium overall, because provider limits are synthetic |
| Long-run stability | S4 | Medium — four hours only |
| Load shedding order | S5 | High |
| Failover, error-shaped failures | S6 | High |
| Failover, timeout-shaped failures | S6 with `FAULT_KIND=timeout` | Medium — must be run explicitly, easy to skip |
| Streaming contract | S7 | High |
| Mid-stream stalls | not covered here | Low — server-side metrics only |
| Tenant isolation, quota and cache | S8 | High |
| Tenant isolation, shared concurrency | S8 with a control run | Medium — requires two runs and a manual comparison |
| Guardrail behaviour under load | not covered | None — see the gap list |
| Cost attribution accuracy | partial: S2 records cost per request | Low — no reconciliation against `usage_records` |
| Telemetry completeness under load | not covered | None — see the gap list |

## Known gaps

Written down because an unwritten gap becomes an assumed capability.

1. **Guardrail load behaviour.** No scenario drives content that trips guardrails at volume. The
   guardrail callout is in the TTFT path and its saturation behaviour is untested. Needs a scenario
   with a corpus of content that reliably triggers each category.
2. **Telemetry completeness under load.** SPEC §5 sets a completeness objective, and nothing here
   verifies it holds at volume — precisely when the collector is most likely to shed. Needs a check
   that reconciles request count against traces received during a baseline run.
3. **Cost reconciliation.** The scripts record `x-agentgate-cost-usd` but never compare it against
   the `usage_records` written for the same window. A discrepancy is a chargeback defect and would
   currently go unnoticed.
4. **Mid-stream stall detection.** Requires an SSE extension; see
   [README §5](README.md#5-how-ttft-is-measured-and-the-caveat).
5. **Ingress and proxy path.** Every scenario runs inside the cluster. Buffering proxies, idle
   timeouts and TLS termination behaviour on the real caller path are untested from here.
6. **Real provider variance.** Runs against `mockprovider` have deterministic backend latency. Real
   provider p99 variance is a material part of TTFT and is not represented.
7. **Sustained spike.** S5 holds a spike for three minutes. Autoscaler behaviour and provider quota
   exhaustion both bind on a longer horizon and are untested.

Each gap is either an accepted limitation with a stated reason, or a backlog item. None of them is
allowed to be a surprise.
