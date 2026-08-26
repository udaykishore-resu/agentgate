# AgentGate Load Testing

The capacity-proof method: what each test answers, how to run it, how to read the result, and what
"proven" means.

Capacity is not proven by a number. It is proven by a set of measurements, each answering a
different question, taken against a build and an environment you can name. A green baseline run on
its own proves that the system was fine at that volume, once. That is worth much less than it looks.

---

## 1. What "proven" means

Capacity is proven for a given volume when **all** of the following hold, against the same build,
in the same environment, within the same week:

| # | Claim | Proven by | Threshold |
|---|---|---|---|
| 1 | It is correct at all | `smoke.js` | Every check passes, no exceptions |
| 2 | It meets the SLOs at expected volume | `baseline.js` | SLO thresholds green, zero dropped iterations |
| 3 | We know where it breaks | `ramp.js` | The knee is measured and recorded |
| 4 | We run with headroom below the knee | `ramp.js` + the headroom policy | Expected peak ≤ 50% of measured knee |
| 5 | It does not drift | `soak.js` | p95 drift < 5% over 4h, no memory trend |
| 6 | It degrades in the promised order | `spike.js` | Batch sheds before interactive; no 5xx |
| 7 | Backend failure is invisible to callers | `failover.js` | Zero 5xx that should have failed over |
| 8 | The streaming contract holds under load | `streaming.js` | Zero contract violations, TTFT p95 within SLO |
| 9 | Tenants are isolated | `multitenant.js` | Zero collateral limiting, zero cross-agent cache hits |

Fail any one and capacity is **not** proven — you have a partial result and a known gap. Record the
gap explicitly rather than rounding it to a pass.

Results expire. A capacity proof is valid for one build and one infrastructure shape. Re-run the
full set on: a gateway minor version, a change to the policy chain, a new backend or provider, a
node type or instance size change, a Redis or Postgres tier change, or a doubling of fleet volume.

---

## 2. The tests

| Script | Question it answers | Duration | Run against |
|---|---|---|---|
| `k6/smoke.js` | Is this deployment correct enough to test? | ~1 min | Any environment, including prod |
| `k6/baseline.js` | Do we meet the SLOs at expected production volume? | 30 min | Staging at production shape |
| `k6/ramp.js` | At what concurrency does p95 breach? Where is the knee? | ~45 min | Isolated staging only |
| `k6/soak.js` | Does anything drift over hours? | 4 h | Staging, overnight |
| `k6/spike.js` | Does a 10x burst shed correctly or collapse? | ~11 min | Isolated staging only |
| `k6/failover.js` | Does a bad backend reach the caller? | ~12 min | Staging with fault injection |
| `k6/streaming.js` | Does the SSE contract hold under load? | 20 min | Staging |
| `k6/multitenant.js` | Can one agent starve another? | 15 min | Staging with several registered agents |

`k6/lib/common.js` holds everything shared: token minting, request builders, contract assertions,
SSE parsing, custom metrics. Read it before writing a new scenario.

**Only `smoke.js` may be run against production**, and only with a load-test agent identity whose
quota is sized for it. Every other script here is designed to push a system past its limits and will
consume real provider spend and real capacity.

---

## 3. Running them

### Prerequisites

```bash
k6 version                # v0.49 or later
export AGENTGATE_GATEWAY=https://gateway.staging.agentgate.internal
export AGENTGATE_CONTROLPLANE=https://controlplane.staging.agentgate.internal
export AGENTGATE_ENV=staging
export AGENTGATE_CLIENT_ID=...        # a registered load-test agent
export AGENTGATE_CLIENT_SECRET=...
```

The load-test agent must be registered like any other agent, with a quota sized for the test. A test
that spends its first three minutes being rate limited measures the rate limiter, not the gateway.

### The sequence

```bash
# 1. Correctness gate. Everything else is meaningless if this is red.
k6 run test/load/k6/smoke.js

# 2. SLO conformance at expected volume.
k6 run -e AGENTGATE_TARGET_RPM=1000 -e AGENTGATE_DURATION=30m test/load/k6/baseline.js

# 3. Find the knee. Isolated environment - this saturates things.
k6 run -e AGENTGATE_RAMP_START=25 -e AGENTGATE_RAMP_STEP=25 -e AGENTGATE_RAMP_MAX=500 \
       test/load/k6/ramp.js

# 4. Streaming contract and TTFT.
k6 run test/load/k6/streaming.js

# 5. Failure behaviour.
k6 run test/load/k6/failover.js
k6 run -e AGENTGATE_BASE_RPM=200 -e AGENTGATE_SPIKE_MULTIPLIER=10 test/load/k6/spike.js

# 6. Isolation.
k6 run test/load/k6/multitenant.js

# 7. Drift. Overnight.
k6 run -e AGENTGATE_SOAK_DURATION=4h -e AGENTGATE_TARGET_RPM=600 test/load/k6/soak.js
```

### Configuration

Every knob is an environment variable, all optional, all with defaults in `lib/common.js`.

| Variable | Default | Meaning |
|---|---|---|
| `AGENTGATE_GATEWAY` | `https://gateway.agentgate.internal` | Gateway base URL |
| `AGENTGATE_CONTROLPLANE` | `https://controlplane.agentgate.internal` | Token exchange endpoint |
| `AGENTGATE_AUTH_MODE` | `client_credentials` | `client_credentials` or `static` |
| `AGENTGATE_TOKEN` | — | Pre-minted token, for `static` mode |
| `AGENTGATE_CLIENT_ID` / `_SECRET` | — | Load-test agent credentials |
| `AGENTGATE_MODEL` | `general-chat` | Logical model under test |
| `AGENTGATE_POOL` | `general-chat` | Pool, sent as `x-agentgate-pool` |
| `AGENTGATE_STREAM_RATIO` | `0.6` | Share of requests that stream |
| `AGENTGATE_PROMPT_TOKENS` | `600` | Approximate input size |
| `AGENTGATE_MAX_TOKENS` | `256` | Requested output ceiling |
| `AGENTGATE_CACHE` | `off` | `on`, `off` or `refresh` — see below |
| `AGENTGATE_PRIORITY` | `interactive` | `interactive` or `batch` |
| `AGENTGATE_TARGET_RPM` | per script | Arrival rate |
| `AGENTGATE_DURATION` | per script | Run length |

**On `AGENTGATE_CACHE`.** Default `off`, deliberately. With the cache on you are measuring your
prompt corpus's hit ratio, not the gateway's capacity — a repetitive corpus produces a beautiful and
completely fictional result. Set `on` only when you are specifically measuring the production mix,
and say so when you report the number.

---

## 4. Acceptance thresholds

Taken directly from SPEC §5. These are encoded in each script's `thresholds` block, so the SLO and
the test cannot drift apart.

| SLI | Objective (SPEC §5) | Threshold in the scripts | Enforced by |
|---|---|---|---|
| Gateway availability | 99.9% non-5xx, non-`no_healthy_backend` | `agentgate_availability_rate: rate>=0.999` | baseline, soak, failover |
| Gateway overhead, unary | p95 < 60ms excluding provider time | `agentgate_gateway_overhead_ms: p(95)<60` | baseline, soak |
| TTFT, streaming | p95 < 1200ms | `agentgate_ttft_ms: p(95)<1200` | baseline, streaming, soak |
| Control plane token exchange | p95 < 150ms, 99.95% success | `agentgate_token_mint_ms: p(95)<150` | baseline, soak |

Contract thresholds, which are not SLOs but are non-negotiable:

| Assertion | Threshold | Enforced by |
|---|---|---|
| SPEC §2.3 headers present on every response | `agentgate_contract_headers_ok: rate==1.0` | all |
| Every stream terminates with `[DONE]` | `agentgate_done_terminator_seen: rate==1.0` | streaming, soak |
| Usage frame present when requested | `agentgate_usage_frame_seen: rate==1.0` | streaming |
| No 5xx that should have failed over | `agentgate_should_have_failed_over: count==0` | failover |
| Stream failover only before first content | `agentgate_stream_failover_after_content: count==0` | failover, streaming |
| Batch sheds before interactive | `agentgate_shed_rate{tier:interactive}: rate<0.05` | spike |
| No collateral rate limiting | `agentgate_collateral_limited: count==0` | multitenant |
| No cross-agent cache hits | `agentgate_cross_agent_cache_hit: count==0` | multitenant |

Derived gates that are not in the SPEC but exist because incidents taught us to check:

| Assertion | Threshold | Why |
|---|---|---|
| Retry depth | `agentgate_attempts: p(99)<=3` | The retry budget must bound attempts, not just count them |
| Failover rate at steady state | `agentgate_failover_rate: rate<0.02` | Routine failover at baseline means a backend is already sick |
| Dropped iterations | `dropped_iterations: count<10` | A green run at half the intended volume is not a pass |
| Soak drift | p95 late vs early < 5% | Leaks present as drift long before they present as failure |

---

## 5. How TTFT is measured, and the caveat

k6's `http` module buffers the whole response, so per-frame arrival times are not observable. The
scripts use `res.timings.waiting` — time to first byte — as the client-side TTFT.

That is a good proxy and a slightly optimistic one. If the gateway flushes response headers before
the first content frame, TTFB measures time-to-headers, and the client figure will read lower than
the true time-to-first-token.

**Always cross-check against the gateway's own measurement for the same window**, and report the
larger of the two:

```bash
promtool query instant https://prometheus.internal \
  'histogram_quantile(0.95, sum by (le, pool) (rate(agentgate_gateway_ttft_seconds_bucket[5m])))'
```

A gap under about 50ms means the client figure is trustworthy. A larger gap means headers are being
flushed early; use the server figure and note the discrepancy in the result record.

The same limitation applies to inter-token latency: the scripts derive the **mean**
`(total - ttft) / (output_tokens - 1)` from the authoritative usage frame. That number moves when a
provider's generation rate degrades, which is what it is for. It cannot show a mid-stream stall —
for that, use the gateway's `agentgate_stream_intertoken_seconds_bucket` and
[docs/runbooks/streaming-stalls.md](../../docs/runbooks/streaming-stalls.md).

If per-frame timing becomes necessary, build k6 with an SSE extension:

```bash
xk6 build --with github.com/phymbert/xk6-sse
```

and add a variant script. Do not change these scripts to require an extension — a test that needs a
custom binary is a test that stops being run.

---

## 6. Reading a baseline result

Worked example. AgentGate 1.14.2, staging at production shape: 16 gateway pods at 2 vCPU / 4 GiB,
Redis Standard C3, Postgres 8 vCPU, target 1,000 rpm, stream ratio 0.6, cache off.

```
k6 run -e AGENTGATE_TARGET_RPM=1000 -e AGENTGATE_DURATION=30m test/load/k6/baseline.js
```

### Result

| Metric | p50 | p95 | p99 | Threshold | Verdict |
|---|---|---|---|---|---|
| `agentgate_gateway_overhead_ms` | 18.4 | 41.2 | 87.6 | p95 < 60 | pass |
| `agentgate_ttft_ms` | 412 | 968 | 1,644 | p95 < 1200 | pass |
| `agentgate_unary_duration_ms` | 1,340 | 3,180 | 5,910 | informational | — |
| `agentgate_stream_duration_ms` | 3,720 | 8,940 | 14,200 | informational | — |
| `agentgate_intertoken_mean_ms` | 31 | 58 | 96 | informational | — |
| `agentgate_token_mint_ms` | 41 | 88 | 143 | p95 < 150 | pass |
| `agentgate_provider_time_ms` | 1,290 | 3,110 | 5,800 | informational | — |

| Rate / counter | Value | Threshold | Verdict |
|---|---|---|---|
| `agentgate_availability_rate` | 99.981% | ≥ 99.9% | pass |
| `agentgate_success_rate` | 99.94% | informational | — |
| `agentgate_contract_headers_ok` | 100% | == 100% | pass |
| `agentgate_stream_contract_ok` | 99.97% | > 99.9% | pass |
| `agentgate_done_terminator_seen` | 100% | == 100% | pass |
| `agentgate_usage_frame_seen` | 99.99% | > 99.9% | pass |
| `agentgate_failover_rate` | 0.41% | < 2% | pass |
| `agentgate_attempts` p99 | 2 | ≤ 3 | pass |
| `agentgate_unexpected_5xx` | 3 | < 10 | pass |
| `agentgate_rate_limited` | 0 | — | — |
| `agentgate_no_healthy_backend` | 0 | — | — |
| `dropped_iterations` | 0 | < 10 | pass |
| `http_reqs` | 30,014 | ≈ 30,000 expected | pass |
| `agentgate_cost_usd` sum | $18.42 | — | $0.00061/request |

### Interpretation

**The run is a pass, and here is what it actually tells you.**

*Overhead p95 of 41ms against a 60ms budget* leaves 19ms of headroom, about 32%. That is
comfortable but not generous. The p99 of 87.6ms is already past the p95 budget, which means one in a
hundred requests is spending more than the SLO's worth of time inside our policy chain. Worth
looking at the stage breakdown before it becomes the p95:

```bash
promtool query instant https://prometheus.internal \
  'topk(5, histogram_quantile(0.99, sum by (le, stage) (rate(agentgate_gateway_policy_duration_seconds_bucket[30m]))))'
```

*TTFT p95 of 968ms against 1200ms* leaves 19% headroom — tighter than the overhead margin, and TTFT
is dominated by provider prefill, which we do not control. A provider getting 25% slower breaches
this SLO with no change on our side. This is the number to watch after any pool weight change.

*Provider time p95 of 3,110ms against a stream duration p95 of 8,940ms* confirms that most of the
wall clock belongs to the model, not to us. That is the healthy shape. When those two converge, we
have become the bottleneck.

*Failover rate 0.41%* — roughly one request in 244 needed a second attempt. Non-zero is normal.
Above 2% at steady state means a backend is already degrading and the pool is quietly compensating.

*3 unexpected 5xx out of 30,014* is 0.01%, comfortably inside the availability budget. Look at them
anyway: three is a small enough number to explain individually, and "we did not look" is how a
pattern becomes an incident.

*Zero dropped iterations* is what makes the rest of the table meaningful. Had k6 dropped 20% of
iterations, every latency figure above would describe a system running at 800 rpm while the report
claimed 1,000.

*Cost per request of $0.00061* with the cache off. With the production cache mix the same test runs
about $0.00042, and the difference is the cache's contribution to the platform's business case.

**What this result does not prove:** anything about 1,100 rpm. The knee from `ramp.js` for this same
build was 2,400 concurrent equivalent — see `capacity-model.md` — so 1,000 rpm sits at roughly 40%
of the knee, which satisfies the headroom policy. Without that ramp result, this baseline is a
single data point with no idea how close to the edge it sits.

---

## 7. Recording results

Every result that anyone will later cite goes in `test/load/results/` with this header. A result
without an environment description is not a result.

```
build:            agentgate/gateway:1.14.2
environment:      staging-eastus
gateway:          16 pods, 2 vCPU / 4 GiB, HPA min 16 max 32
redis:            Azure Cache Standard C3, 6 GB
postgres:         Flexible Server, 8 vCPU, 32 GB
backends:         mockprovider (fixed 1.2s latency, 40 tok/s generation)
                  NOTE: synthetic backend. Provider-time figures are not real.
date:             2026-08-26
operator:         <name>
command:          k6 run -e AGENTGATE_TARGET_RPM=1000 ... test/load/k6/baseline.js
verdict:          pass
notes:            p99 overhead 87.6ms is above the p95 budget; raised AGP-931.
```

The backend note matters more than anything else in the header. A run against `mockprovider`
measures **our** overhead accurately and tells you nothing real about TTFT or provider time. A run
against real provider deployments measures everything but costs money and is subject to their
variance. Say which one you did, every time.

---

## 8. Common ways to get a wrong answer

Each of these has produced a confidently reported and completely wrong number at least once.

1. **Ignoring `dropped_iterations`.** k6 drops iterations when it cannot sustain the arrival rate.
   The latency numbers then describe a lower volume than the report claims. Check it first, every
   run.
2. **Running with the cache on and a repetitive corpus.** A 90% hit ratio makes any gateway look
   magnificent. Default is `off` for this reason.
3. **Measuring against `mockprovider` and quoting TTFT.** The mock's generation rate is a
   configuration value, not a measurement.
4. **Load generator saturation.** If the k6 host is CPU-bound, you are measuring the load generator.
   Watch it: `k6 run --out json` plus host CPU, or run distributed.
5. **Testing a rate-limited identity.** If the load-test agent's quota is below the test rate, the
   test measures the rate limiter. Check `agentgate_rate_limited` is zero on capacity runs.
6. **A single prompt shape.** One prompt size means one cache key, one token count, one code path.
   The scripts jitter prompt size for exactly this reason.
7. **Comparing runs across different backend configurations.** A pool weight change moves cost and
   latency. Two runs are only comparable if the routing table was identical, and the routing table
   is not in the k6 output — put it in the result header.
8. **Treating a passed threshold as a proven system.** The threshold is a floor. The interesting
   information is in the distribution, the p99, and the shape of the failure at the knee.

---

## 9. Related

- [`scenarios.md`](scenarios.md) — the full test matrix: hypothesis, method, what each test proves
  and what it does not
- [`capacity-model.md`](capacity-model.md) — the analytical model and the sizing table the tests
  validate
- [`../../docs/runbooks/operational-readiness-review.md`](../../docs/runbooks/operational-readiness-review.md)
  — section A7 is where these results are cited
- [`../../docs/runbooks/postmortem-template.md`](../../docs/runbooks/postmortem-template.md) — the
  worked example ends in an action item to add a failure mode to `failover.js`, which is how this
  suite is supposed to grow
