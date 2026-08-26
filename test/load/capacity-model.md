# AgentGate Capacity Model

An analytical model of what AgentGate costs to run at a given request rate, with the arithmetic
shown, so that a sizing decision can be argued with rather than believed.

The model predicts; the tests in this directory measure. Where they disagree, the measurement wins
and the model gets fixed. Every constant below is labelled either **measured** (from a named test or
production metric) or **assumed** (an estimate, with its basis stated). Assumed constants are the
ones to attack first when a prediction is wrong.

---

## 1. Inputs

### 1.1 Traffic mix

| Symbol | Meaning | Value | Source |
|---|---|---|---|
| `λ` | Arrival rate | variable | The thing we are sizing for |
| — | Share that is embeddings | 15% | Assumed, from the fleet's current mix |
| — | Share of chat that streams | 60% | Assumed, matches production for interactive agents |
| — | Mean input tokens | 600 | Measured, `agentgate_gateway_tokens_total{direction="input"}` / requests |
| — | Mean output tokens | 256 | Measured, same for `direction="output"` |

### 1.2 Service times

`W` is the time a request occupies a slot in the gateway, end to end.

| Request type | Gateway overhead | Provider time | `W` | Source |
|---|---|---|---|---|
| Unary chat | 0.05 s | 2.35 s | **2.40 s** | Measured, baseline p50; provider is `mockprovider` at production-representative latency |
| Streaming chat | 0.05 s | 7.95 s | **8.00 s** | Measured, baseline; 256 tokens at ~35 tok/s plus 0.4 s prefill |
| Embeddings | 0.02 s | 0.13 s | **0.15 s** | Measured |

Note what dominates: **98% of `W` is provider time.** Every capacity number below is really a
statement about how many provider calls we can hold open, not about how fast our code is.

### 1.3 Blended service time

Per 100 requests: 15 embeddings, and of the remaining 85 chat requests, 60% stream (51) and 40% are
unary (34).

```
W_blended = (15 × 0.15  +  51 × 8.00  +  34 × 2.40) / 100
          = (2.25       +  408.00     +  81.60)     / 100
          = 491.85 / 100
          = 4.92 s per request
```

Streaming is 51% of requests and 83% of the occupancy. Any change to the stream ratio changes every
number in this document, which is why it is the first thing to check when a prediction is wrong.

---

## 2. Little's law applied to the gateway

```
L = λ × W
```

`L` is the mean number of requests concurrently in flight across the gateway fleet. With `λ` in
requests per minute, convert to requests per second first.

| Target | `λ` (rpm) | `λ` (rps) | `W` (s) | **`L` concurrent** |
|---|---|---|---|---|
| Small | 100 | 1.667 | 4.92 | **8.2** |
| Medium | 1,000 | 16.667 | 4.92 | **82.0** |
| Large | 10,000 | 166.667 | 4.92 | **820.0** |

```
100 rpm:     (100 / 60) × 4.92  =  1.667 × 4.92  =   8.20
1,000 rpm:   (1000 / 60) × 4.92 = 16.667 × 4.92  =  82.00
10,000 rpm:  (10000 / 60) × 4.92 = 166.667 × 4.92 = 820.00
```

Little's law gives the **mean**. Sizing to the mean guarantees queueing half the time. Peak
concurrency runs materially above it — the baseline run shows a peak-to-mean ratio of about 1.6 for
this mix, so plan against `L_peak = 1.6 × L`:

| Target | `L` mean | `L_peak` |
|---|---|---|
| 100 rpm | 8.2 | 13.1 |
| 1,000 rpm | 82.0 | 131.2 |
| 10,000 rpm | 820.0 | 1,312.0 |

---

## 3. Per-pod concurrency ceiling

### 3.1 What the theoretical limits say

**CPU.** Measured CPU cost per request, from a profiled baseline run:

| Request type | CPU | Composition |
|---|---|---|
| Unary chat | 3.5 ms | JSON parse and serialise, SHA-256 cache key over the normalised body, JWT verify against a cached JWKS, token estimation, provider transformation, span assembly |
| Streaming chat | 8.0 ms | The above, plus ~60 SSE frames at ~0.05 ms each, plus one guardrail window scan per 256 output tokens, plus incremental transformation |
| Embeddings | 0.8 ms | Smaller body, no guardrail window |

```
CPU_blended = (15 × 0.8 + 51 × 8.0 + 34 × 3.5) / 100
            = (12.0     + 408.0    + 119.0)    / 100
            = 539.0 / 100
            = 5.39 ms CPU per request
```

A 2 vCPU pod at a 60% utilisation target supplies 1,200 ms of CPU per second:

```
requests/s per pod (CPU-bound)  = 1200 / 5.39 = 222.6 rps
concurrency per pod (CPU-bound) = 222.6 × 4.92 = 1,095 concurrent
```

**Memory.** Per in-flight stream, itemised:

| Component | Bytes | Basis |
|---|---|---|
| Goroutine stacks, reader and writer | 32 KiB | 2 goroutines, grown from 8 KiB under real frame handling |
| Retained request body plus normalised copy for the cache key | 5 KiB | 600 tokens at 4 chars/token, held twice |
| Provider response read buffer | 32 KiB | `bufio` reader on the upstream connection |
| Guardrail output window buffer | 8 KiB | 256 tokens of text plus the JSON callout envelope |
| Span attributes across 16 policy stages | 4 KiB | ~250 B per stage |
| TLS connection buffers to the backend | 32 KiB | 16 KiB read + 16 KiB write |
| Subtotal | 113 KiB | |
| Allocator and per-request overhead, +20% | 23 KiB | |
| **Total per in-flight stream** | **~136 KiB, round to 150 KiB** | |

An in-flight **unary** request is cheaper — no guardrail window, no SSE framing — at roughly
**90 KiB**.

Pod memory budget, 4 GiB limit:

```
base RSS (runtime, JWKS cache, routing table, metric registry)   400 MiB
remaining                                                       3,696 MiB
usable at a 60% live-heap target (GOGC headroom)                2,218 MiB

streams supported = 2218 MiB / 0.146 MiB = 15,190 concurrent streams
```

**Memory is not the constraint.** Not remotely — it exceeds the CPU limit by 14x.

**File descriptors and connections.** Each in-flight request holds one inbound connection and shares
a pooled outbound one. At 1,095 concurrent per pod: 1,095 inbound + ~30 pooled outbound + margin,
comfortably under a 65,536 `nofile` limit.

### 3.2 What the measurement says

`ramp.js` against build 1.14.2, 16 pods at 2 vCPU / 4 GiB:

| Concurrency (fleet) | Per pod | Overhead p95 | Verdict |
|---|---|---|---|
| 1,600 | 100 | 38 ms | pass |
| 2,000 | 125 | 44 ms | pass |
| **2,400** | **150** | **58 ms** | **last passing step** |
| 2,800 | 175 | 96 ms | breach |
| 3,200 | 200 | 210 ms, 429 shedding begins | breach |

**Measured knee: 150 concurrent per pod.**

### 3.3 The gap, stated honestly

The model predicts a CPU-bound ceiling of ~1,095 per pod. The measurement says 150. That is a
factor of seven, and it means **the binding constraint is not one this model accounts for.** The
candidates, in order of suspicion:

1. **Per-backend concurrency caps.** Each backend carries its own cap (SPEC §3.1). At 2,400 fleet
   concurrent with 98% of occupancy at the provider, ~2,350 concurrent backend calls are spread
   across two tier-1 backends at 60/40 — 1,410 and 940. If a backend cap sits near 1,400, the queue
   forms there and the gateway's own resources are irrelevant.
2. **Go scheduler and GC pressure** from thousands of live goroutines each holding a network buffer.
   Live-heap-proportional GC cost rises with concurrent streams even when total memory is low.
3. **Redis round-trip amplification.** ~6 Redis ops per request (§6); at 490 rps fleet-wide that is
   ~2,900 ops/s against one connection pool per pod. Pool waiting shows up as gateway overhead and
   is indistinguishable from our own slowness in the SLI.

This gap is a live finding, not an accepted fact. Until it is resolved, **size from the measured
150, not from the modelled 1,095** — but do not stop investigating, because closing this gap is
worth roughly seven times the current pod count.

### 3.4 Headroom policy

Operate at **no more than 50% of the measured knee**: 75 concurrent per pod.

The 50% is chosen so that losing one availability zone — a third of the pods — leaves the survivors
at 75 / (2/3) = 112 concurrent per pod, still below the 150 knee. That is the whole justification,
and it is the reason the number is 50 rather than 70.

---

## 4. Gateway pod count

```
pods_for_load = ceil(L_peak / 75)
pods_for_az_loss = ceil(pods_for_load / (2/3))     # survive losing one of three AZs
pods = max(pods_for_az_loss, 3, rounded up to a multiple of 3)
```

| Target | `L_peak` | `L_peak / 75` | ceil | ÷ (2/3) | Floor and AZ rounding | **Pods** |
|---|---|---|---|---|---|---|
| 100 rpm | 13.1 | 0.17 | 1 | 1.5 → 2 | min 3, multiple of 3 | **3** |
| 1,000 rpm | 131.2 | 1.75 | 2 | 3.0 → 3 | min 3 → raise to 6 for rolling upgrades | **6** |
| 10,000 rpm | 1,312.0 | 17.49 | 18 | 27.0 → 27 | multiple of 3 | **27** |

The 1,000 rpm case is raised from 3 to 6 deliberately: with `maxUnavailable: 0` and a PDB of 75%
minAvailable, a 3-pod deployment cannot roll without either surging to 4 or breaching the budget.
Six pods roll cleanly two at a time.

At 10,000 rpm the fleet is 27 pods × 2 vCPU = 54 vCPU, of which the model says the request path
needs `166.7 × 5.39 ms = 0.90 vCPU` — **under 2% utilisation.** The pod count is driven entirely by
the concurrency ceiling from §3.3, and that is the strongest argument for chasing the gap.

---

## 5. Backend connection pool sizing

98% of `W` is provider time, so concurrent backend calls ≈ 0.98 × `L_peak`.

| Target | `L_peak` | Concurrent backend calls | Azure at 60% | Bedrock at 40% |
|---|---|---|---|---|
| 100 rpm | 13.1 | 12.8 | 7.7 | 5.1 |
| 1,000 rpm | 131.2 | 128.6 | 77.1 | 51.4 |
| 10,000 rpm | 1,312.0 | 1,285.8 | 771.5 | 514.3 |

**Azure OpenAI over HTTP/2** multiplexes; connections needed per pod:

```
10,000 rpm: 771.5 concurrent / 27 pods = 28.6 concurrent per pod
            28.6 / 100 max_concurrent_streams = 0.29 connections
```

One connection would carry it, but head-of-line blocking on a single connection turns one slow
stream into everyone's problem. Set `MaxConnsPerHost = 4`, `MaxIdleConnsPerHost = 4`,
`IdleConnTimeout = 90s`.

**Bedrock over HTTP/1.1** needs one connection per concurrent request:

```
10,000 rpm: 514.3 concurrent / 27 pods = 19.0 concurrent per pod
            + 33% headroom for burst = 25.3 -> pool size 26
```

| Target | Pods | Azure conns/pod | Bedrock conns/pod | Total outbound conns (fleet) |
|---|---|---|---|---|
| 100 rpm | 3 | 4 | 8 (floor) | 36 |
| 1,000 rpm | 6 | 4 | 12 | 96 |
| 10,000 rpm | 27 | 4 | 26 | 810 |

A pool sized below concurrent demand shows up as gateway overhead, not as a backend error — the
request waits for a connection inside our process and the SLI blames us. Check
`agentgate_redis_pool_wait_seconds` and its backend equivalent before concluding the code is slow;
see [gateway-latency-regression.md](../../docs/runbooks/gateway-latency-regression.md) §6.4.

---

## 6. Redis operations per request

From the policy chain in SPEC §3.2 and the bucket design in §3.4:

| Stage | Operation | Ops | Note |
|---|---|---|---|
| `authn` | `SET NX EX` on `ag:jti:{jti}` | 1 | Replay window |
| `ratelimit.requests` | `EVALSHA` token bucket on `:rpm` | 1 | One atomic Lua script |
| `quota.tokens` reserve | `EVALSHA` on `:tpm` | 1 | Reserve estimated input + `max_tokens` |
| `cache.lookup` | `GET` on `ag:cache:exact:{tenant}:{sha}` | 1 | Exact match |
| `cache.store` | `SETEX` | 0.65 | Only on a miss; 35% hit ratio assumed |
| `meter` settle | `EVALSHA` releasing the reserve and incrementing the monthly counter | 1 | Both keys hash-tagged into one slot |
| Circuit breaker | — | 0 | Per-pod in-memory state, exported as a metric |
| Semantic cache | — | 0 | Off by default (SPEC §3.5) |
| **Total** | | **5.65, round to 6** | |

With semantic caching enabled, add one embedding call to the provider (not to Redis) plus a vector
scan over the tenant's recent entries — that is the change that put `cache.lookup` in the hot path
in [gateway-latency-regression.md](../../docs/runbooks/gateway-latency-regression.md) §6.2.

**Throughput:**

```
100 rpm:     1.667 rps × 6 =     10 ops/s
1,000 rpm:  16.667 rps × 6 =    100 ops/s
10,000 rpm: 166.667 rps × 6 =  1,000 ops/s
```

A single Redis node handles 100,000+ ops/s, so **Redis throughput is never the constraint here.**
Redis sizing is driven by memory and by round-trip latency, and Redis latency matters
disproportionately because six sequential round trips sit directly in the 60ms overhead budget:

```
6 round trips × 1.0 ms p99 in-VNet = 6.0 ms of the 60 ms budget (10%)
6 round trips × 5.0 ms p99 degraded = 30.0 ms of the 60 ms budget (50%)
```

That is why `RedisDegraded` alerts at a p99 of 50 ms rather than waiting for an outage: at 50 ms the
overhead SLO is gone on Redis alone.

**Memory:**

| Component | Arithmetic (10,000 rpm) | Size |
|---|---|---|
| Cache entries | 65% miss × 10,000 rpm × 60 min TTL = 390,000 entries × 1.6 KiB | 624 MiB |
| JTI replay window | 10,000 rpm × 15 min = 150,000 keys × 80 B | 12 MiB |
| Rate limit and quota buckets | 500 agents × 3 keys × 100 B | 0.15 MiB |
| Monthly budget counters | 40 cost centres × 200 B | negligible |
| **Subtotal** | | **636 MiB** |
| Provision at 3x for fragmentation and burst | | **2 GiB** |

Cache entry size: a 256-token response is ~1 KiB of text, plus metadata (originating trace id, token
counts, cost, model, timestamps) at ~0.5 KiB, plus ~100 B of Redis key and object overhead.

| Target | Cache entries | Redis memory needed | Provisioned |
|---|---|---|---|
| 100 rpm | 3,900 | 6.4 MiB | 1 GiB (tier minimum) |
| 1,000 rpm | 39,000 | 64 MiB | 1 GiB |
| 10,000 rpm | 390,000 | 636 MiB | 2 GiB |

Set `maxmemory-policy volatile-lru`, not `allkeys-lru`. Under `allkeys-lru` a memory spike evicts
rate-limit buckets and JTI keys alongside cache entries, which silently weakens two controls — the
failure described in [redis-unavailable.md](../../docs/runbooks/redis-unavailable.md) §6.3.

---

## 7. Postgres write rate from usage records

One immutable `UsageRecord` per completed request (SPEC §6), written from the usage stream rather
than synchronously in the request path — which is why a Postgres outage degrades registration and
promotion but not traffic.

```
row size:  ~400 B of data + ~300 B of index = 700 B effective
```

| Target | Rows/s | Rows/day | Bytes/day | Per 30 days |
|---|---|---|---|---|
| 100 rpm | 1.67 | 144,000 | 101 MiB | 3.0 GiB |
| 1,000 rpm | 16.67 | 1,440,000 | 1.0 GiB | 30 GiB |
| 10,000 rpm | 166.67 | 14,400,000 | 10.1 GiB | 302 GiB |

```
10,000 rpm: 166.67 rows/s × 86,400 s = 14,400,000 rows/day
            14,400,000 × 700 B = 10,080,000,000 B = 10.1 GiB/day
            × 30 = 302 GiB/month
```

Batched at 500 rows per transaction, the write rate is `166.67 / 500 = 0.33 transactions/s` — a
non-event for Postgres. **Storage growth is the constraint, not write throughput.** At 10,000 rpm:
partition `usage_records` by day, keep 90 days hot (907 GiB), and export older partitions to the
Parquet chargeback path (SPEC §4.4) before dropping them.

Other Postgres writers, for completeness:

| Writer | Rate | Note |
|---|---|---|
| Token issuance log | 500 agents × 4/hour = 0.56/s | 15-minute token TTL |
| Promotion gate evaluations | ~50/day | Negligible |
| Registry changes | ~20/day | Negligible |

**Connection budget.** The rule from the readiness review (A6.4) is that the sum of all pools must
sit below `max_connections` with at least 30% headroom:

| Service | Pods (10,000 rpm) | Pool per pod | Total |
|---|---|---|---|
| gateway (authz cache refresh only) | 27 | 2 | 54 |
| controlplane | 6 | 20 | 120 |
| fleetview | 3 | 10 | 30 |
| usage writer | 3 | 5 | 15 |
| Migrations and operators | — | 10 | 10 |
| **Total** | | | **229** |

```
required max_connections = 229 / 0.70 = 327  ->  set 400
headroom = (400 - 229) / 400 = 42.8%   (rule requires >= 30%)
```

---

## 8. Collector throughput per span

**Spans per gateway request** (SPEC §4.2):

| Span | Count |
|---|---|
| `gateway.request` | 1 |
| `gateway.policy.<stage>` | 16 |
| `gen_ai.chat` or `gen_ai.embeddings` | 1.004 (one per attempt; 0.4% failover rate) |
| `gateway.guardrail` | 2 (input and output) |
| `gateway.cache` | 1 |
| **Gateway subtotal** | **21** |
| Agent-side `agent.invoke` / `.step` / `.tool`, ~6 per run over ~3 gateway calls | 2 |
| **Total** | **23 spans per request** |

**Ingest.** Tail sampling decides after a trace is complete, so the collector must receive
everything:

```
100 rpm:     1.667 rps × 23 =      38 spans/s
1,000 rpm:  16.667 rps × 23 =     383 spans/s
10,000 rpm: 166.667 rps × 23 =  3,834 spans/s
```

**Export.** Tail sampling keeps 100% of errors, guardrail blocks, failovers and requests above p99
latency, plus a 5% baseline (SPEC §4.4):

```
always-keep classes: errors 0.1% + guardrail blocks 0.5% + failovers 0.4% + >p99 1.0% = 2.0%
baseline on the rest: 98.0% × 5% = 4.9%
kept total = 6.9%

10,000 rpm export: 3,834 × 0.069 = 265 spans/s
```

**Sizing.** With `batch`, `memory_limiter`, `k8sattributes`, `attributes`, redaction and
`tail_sampling` in the pipeline, throughput is **~5,000 spans/s per vCPU** (measured on the gateway
collector pool; tail sampling dominates). Target 50% utilisation:

```
10,000 rpm: 3,834 / 5,000 = 0.77 vCPU at 100%  ->  1.53 vCPU at 50%  ->  2 vCPU
```

Tail sampling holds each trace until its decision wait expires:

```
decision wait 30 s × 3,834 spans/s = 115,020 spans held
115,020 × 600 B = 69 MiB
× 3 for the queue, the decision cache and allocator overhead = 207 MiB
provision 2 GiB per replica, memory_limiter at 1,500 MiB
```

Replicas are set by HA rather than throughput, with one hard constraint: **tail sampling requires
all spans of a trace to reach the same collector**, so the node-tier collectors must export through
a `loadbalancing` exporter keyed on trace ID. Getting this wrong produces sampling decisions made on
partial traces, which looks exactly like the telemetry loss in
[telemetry-incomplete.md](../../docs/runbooks/telemetry-incomplete.md).

| Target | Ingest spans/s | Export spans/s | Gateway collector replicas | vCPU each | Memory each |
|---|---|---|---|---|---|
| 100 rpm | 38 | 3 | 3 | 0.5 | 1 GiB |
| 1,000 rpm | 383 | 26 | 3 | 1 | 1 GiB |
| 10,000 rpm | 3,834 | 265 | 3 | 2 | 2 GiB |

The node-tier collector runs as a DaemonSet with `batch` and `memory_limiter` only, at 0.25 vCPU /
512 MiB per node — its job is buffering and enrichment, not sampling.

---

## 9. Sizing table

Everything above, assembled. Three-AZ deployment throughout.

| | **100 rpm** | **1,000 rpm** | **10,000 rpm** |
|---|---|---|---|
| **Traffic** | | | |
| Requests/s | 1.67 | 16.67 | 166.67 |
| Mean concurrency `L` | 8.2 | 82.0 | 820.0 |
| Peak concurrency `L_peak` | 13.1 | 131.2 | 1,312.0 |
| **Gateway** | | | |
| Pods | 3 | 6 | 27 |
| CPU / memory per pod | 2 vCPU / 4 GiB | 2 vCPU / 4 GiB | 2 vCPU / 4 GiB |
| Concurrency ceiling per pod | 150 | 150 | 150 |
| Operating concurrency per pod | 4 | 22 | 49 |
| HPA min / max | 3 / 6 | 6 / 12 | 27 / 40 |
| Fleet CPU actually used | 0.01 vCPU | 0.09 vCPU | 0.90 vCPU |
| **Control plane** | | | |
| Pods | 3 | 3 | 6 |
| CPU / memory per pod | 1 vCPU / 2 GiB | 1 vCPU / 2 GiB | 2 vCPU / 4 GiB |
| **Fleetview** | | | |
| Pods | 2 | 3 | 3 |
| CPU / memory per pod | 1 vCPU / 2 GiB | 1 vCPU / 2 GiB | 2 vCPU / 4 GiB |
| **Guardrails** | | | |
| Pods | 3 | 4 | 12 |
| Callout rate (input + one window per 256 output tokens) | 3.3/s | 33/s | 333/s |
| **Redis** | | | |
| Ops/s | 10 | 100 | 1,000 |
| Memory needed | 6.4 MiB | 64 MiB | 636 MiB |
| Provisioned | 1 GiB | 1 GiB | 2 GiB |
| Tier | Standard C1 | Standard C1 | Standard C3 |
| **Postgres** | | | |
| Usage rows/day | 144 k | 1.44 M | 14.4 M |
| Storage/month | 3.0 GiB | 30 GiB | 302 GiB |
| vCPU / memory | 2 / 8 GiB | 4 / 16 GiB | 8 / 32 GiB |
| `max_connections` | 200 | 200 | 400 |
| Partitioning | none | monthly | daily, 90-day hot |
| **Collectors (gateway tier)** | | | |
| Ingest spans/s | 38 | 383 | 3,834 |
| Export spans/s | 3 | 26 | 265 |
| Replicas | 3 | 3 | 3 |
| CPU / memory per replica | 0.5 vCPU / 1 GiB | 1 vCPU / 1 GiB | 2 vCPU / 2 GiB |
| **Backend connections** | | | |
| Azure conns/pod (HTTP/2) | 4 | 4 | 4 |
| Bedrock conns/pod (HTTP/1.1) | 8 | 12 | 26 |
| **Provider capacity required** | | | |
| Concurrent backend calls | 12.8 | 128.6 | 1,285.8 |
| Input tokens/min | 60 k | 600 k | 6.0 M |
| Output tokens/min | 25.6 k | 256 k | 2.56 M |

**Provider capacity is the line that gets missed.** At 10,000 rpm the platform needs 6 million input
and 2.56 million output tokens per minute of provisioned throughput across the pool. That is a
purchasing decision with a lead time, and it will bind long before any number in the Gateway rows
does.

---

## 10. Validating the model

Each prediction has a test that checks it. Run them, compare, and correct the model — a model nobody
falsifies is decoration.

| Prediction | Validated by | How to compare |
|---|---|---|
| `W_blended = 4.92 s` | `baseline.js` | `agentgate_unary_duration_ms` and `agentgate_stream_duration_ms` medians, re-blended by the achieved mix |
| `L = λ × W` | `baseline.js` | `sum(agentgate_gateway_inflight)` during the run against the predicted `L` |
| Peak-to-mean 1.6 | `baseline.js` | max vs mean of `agentgate_gateway_inflight` |
| Knee at 150/pod | `ramp.js` | The measured knee, per §3.2 |
| 150 KiB per stream | `soak.js` | `container_memory_working_set_bytes` delta divided by mean concurrent streams |
| 6 Redis ops/request | any run | `sum(rate(agentgate_redis_commands_total[5m])) / sum(rate(agentgate_gateway_requests_total[5m]))` |
| 23 spans/request | any run | `sum(rate(otelcol_receiver_accepted_spans[5m])) / sum(rate(agentgate_gateway_requests_total[5m]))` |
| 6.9% export ratio | any run | `sum(rate(otelcol_exporter_sent_spans[5m])) / sum(rate(otelcol_receiver_accepted_spans[5m]))` |
| 700 B per usage row | production | `pg_total_relation_size('usage_records') / count(*)` |
| Pool sizing adequate | `ramp.js` | `agentgate_redis_pool_wait_seconds` and the backend equivalent stay near zero up to the knee |

```bash
# The one-line reality check on spans per request, worth running after every deploy:
promtool query instant https://prometheus.internal \
  'sum(rate(otelcol_receiver_accepted_spans[5m])) / sum(rate(agentgate_gateway_requests_total[5m]))'
```

A drift in that ratio means someone added a span in a loop, and it is the leading indicator for
[collector-backpressure.md](../../docs/runbooks/collector-backpressure.md).

---

## 11. Open items

1. **The 7x gap between the modelled and measured per-pod ceiling (§3.3).** The single most valuable
   thing on this list. Instrument per-backend concurrency-cap waiting and GC assist time during a
   ramp, and find out which of the three candidates it is.
2. **`W` is measured against `mockprovider`.** Real provider latency has a much heavier tail, and a
   heavier tail raises `L` for the same `λ`. Re-derive `W` from production
   `agentgate_gateway_duration_seconds{phase="total"}` once there is a month of it.
3. **Peak-to-mean of 1.6** is from a single baseline run with a smooth arrival process. Real agent
   traffic is burstier — batch jobs land together. Derive it from production
   `agentgate_gateway_inflight` instead.
4. **Guardrail callout capacity is modelled by rate only.** Its own service time is not in the model,
   yet it sits inside `W` for every request and inside the TTFT path for every stream.
5. **Semantic caching is excluded.** Enabling it adds an embedding call to the request path and
   changes `W`, `L`, pod count and provider token demand. The model needs a variant before that is
   turned on anywhere it matters.

---

## Related

- [`README.md`](README.md) — how to run the tests and how to read a result
- [`scenarios.md`](scenarios.md) — the test matrix and its known gaps
- [`../../docs/runbooks/operational-readiness-review.md`](../../docs/runbooks/operational-readiness-review.md)
  §A7 — where this model is cited as evidence
- [`../../docs/runbooks/redis-unavailable.md`](../../docs/runbooks/redis-unavailable.md) — the Redis
  ops-per-request figure in operational form
- [`../../docs/runbooks/collector-backpressure.md`](../../docs/runbooks/collector-backpressure.md) —
  spans per request when it goes wrong
- [`../../docs/runbooks/postgres-failover.md`](../../docs/runbooks/postgres-failover.md) — the
  connection budget arithmetic in §7
