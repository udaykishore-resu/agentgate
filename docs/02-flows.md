# 02 — Flows

Operational flows through AgentGate. Each flow has a diagram, a prose walkthrough, and an explicit
list of failure branches. Message-level detail with participants and timing is in
`03-sequences.md`; this document is about decision structure.

Conventions used throughout:

- Rectangles are actions, rhombi are decisions, rounded nodes are terminal states.
- Every terminal error names the frozen `code` from `SPEC.md` §2.4.
- Policy stage numbers match `SPEC.md` §3.2 exactly and are never renumbered.

---

## 1. End-to-end inference request — all 16 policy stages

```mermaid
flowchart TD
  START(["Request arrives at gateway"]) --> P1["1 trace.start - root server span, continue W3C context"]

  P1 --> P2["2 authn - JWKS signature, iss, aud, exp, nbf, jti replay window"]
  P2 --> D2{"Token valid?"}
  D2 -->|"no"| E401(["401 unauthenticated"])
  D2 -->|"yes"| P3["3 authz - promoted for env, pool entitled, scope present"]

  P3 --> D3A{"Version promoted for env?"}
  D3A -->|"no"| E403A(["403 agent_not_promoted"])
  D3A -->|"yes"| D3B{"Pool in token model_pools and scope present?"}
  D3B -->|"no"| E403B(["403 forbidden_pool"])
  D3B -->|"yes"| P4["4 admission - body validate, size cap, param normalisation, context window check"]

  P4 --> D4A{"Body valid?"}
  D4A -->|"no"| E400(["400 invalid_request"])
  D4A -->|"yes"| D4B{"Logical model exists for tenant?"}
  D4B -->|"no"| E404(["404 unknown_model"])
  D4B -->|"yes"| D4C{"Prompt within logical model window?"}
  D4C -->|"no"| E413(["413 context_too_large"])
  D4C -->|"yes"| D4D{"Idempotency key seen with different body?"}
  D4D -->|"yes"| E409(["409 idempotency_conflict"])
  D4D -->|"no"| P5["5 ratelimit.requests - per-agent RPM, distributed token bucket"]

  P5 --> D5{"Under RPM limit?"}
  D5 -->|"no"| E429A(["429 rate_limited with Retry-After"])
  D5 -->|"yes"| P6["6 quota.tokens - estimate input plus max_tokens, reserve from TPM and monthly budget"]

  P6 --> D6{"Reserve succeeded?"}
  D6 -->|"no"| E429B(["429 quota_exceeded with retry_after_seconds"])
  D6 -->|"yes"| P7["7 guardrail.input - content-safety callout"]

  P7 --> D7{"Input verdict"}
  D7 -->|"block"| E403C(["403 guardrail_blocked - release reservation"])
  D7 -->|"redact"| P8["8 cache.lookup - exact SHA-256, then optional semantic"]
  D7 -->|"pass"| P8

  P8 --> D8{"Cache hit?"}
  D8 -->|"hit"| P15C["15 meter - billable false, savings_usd recorded"]
  D8 -->|"miss or bypass"| P9["9 transform.request - provider-agnostic to provider-native"]

  P9 --> P10["10 route - health filter, classification and residency filter, lowest surviving priority tier, weighted-least-outstanding"]
  P10 --> D10{"Any healthy compatible backend?"}
  D10 -->|"no"| E503(["503 no_healthy_backend"])
  D10 -->|"yes"| P11["11 invoke - retry, failover, circuit breaker, hedging window, deadline propagation"]

  P11 --> D11{"Invocation outcome"}
  D11 -->|"caller disconnected"| E499(["499 client_closed_request"])
  D11 -->|"deadline exceeded after retries"| E504(["504 provider_timeout"])
  D11 -->|"caller deadline exceeded"| E408(["408 client_timeout"])
  D11 -->|"unrecoverable upstream error"| E502(["502 provider_error"])
  D11 -->|"all backends exhausted"| E503
  D11 -->|"success"| P12["12 transform.response - provider-native to provider-agnostic"]

  P12 --> P13["13 guardrail.output - streaming-aware windowed scan"]
  P13 --> D13{"Output verdict"}
  D13 -->|"block"| E403D(["403 guardrail_blocked or SSE error frame if stream started"])
  D13 -->|"redact or pass"| P14["14 cache.store"]

  P14 --> P15["15 meter - actual tokens, cost, UsageRecord, settle reservation"]
  P15C --> P16
  P15 --> P16["16 trace.end - span attributes finalised, metrics recorded"]
  P16 --> DONE(["Response returned with x-agentgate headers"])

  E401 --> P15E["15 meter and 16 trace.end still run"]
  E403A --> P15E
  E429B --> P15E
  E503 --> P15E
  E502 --> P15E
  P15E --> ERRDONE(["RFC 9457 problem+json with request_id and trace_id"])
```

### 1.1 Walkthrough

**Stages 1–4, admission.** `trace.start` opens the root `gateway.request` span, continuing the
caller's `traceparent` if present. `authn` verifies the JWT against JWKS fetched from the control
plane and cached, checking `iss`, `aud`, `exp`/`nbf`, and the `jti` replay window. `authz` is three
independent checks — the agent's version is promoted for the requested `env`, the resolved pool is
in the token's `model_pools`, and the required scope (`models:invoke` or `models:embed`) is present.
`admission` validates and normalises the body, applies the size cap, resolves the logical model, and
compares the estimated prompt size against the logical model's window.

**Stages 5–6, cost control.** RPM is checked first because it is cheaper and because a request that
fails RPM should not consume token-quota accounting. `quota.tokens` estimates input tokens plus
`max_tokens` and reserves that many from the agent's bucket, keyed `tenant:team:agent:env`, and also
checks the monthly budget. The reservation is the point at which a request has claimed capacity;
every terminal path after this point must release or settle it.

**Stages 7–8, safety and cache.** The input guardrail runs before cache lookup so that a blocked
prompt never becomes a cache key and never touches a provider. Cache lookup is exact-match first;
semantic match only if the pool has it enabled and the classification permits. A hit skips stages
9–14 entirely and goes straight to `meter`, where it is recorded with `billable:false` and a
`savings_usd` figure.

**Stages 9–12, the backend call.** `transform.request` converts the provider-agnostic body into
provider-native form. `route` applies the selection order from `SPEC.md` §3.1: health filter, then
classification and residency compatibility, then the lowest priority tier with survivors, then
weighted-least-outstanding within that tier. `invoke` owns all backend I/O and everything that
happens when it goes wrong. `transform.response` converts back.

**Stages 13–16, safety and accounting.** Output guardrail scanning is windowed for streams:
approximately 256-token windows are buffered, scanned, and released, so a violation is caught before
the caller sees it while keeping time-to-first-token acceptable. `cache.store` writes the entry with
the originating trace id, so a future hit is still fully attributable. `meter` settles the
reservation against actual usage, records cost, and emits the immutable `UsageRecord`. `trace.end`
finalises span attributes and records metrics.

### 1.2 Failure branches

| Branch | Code | Status | Reservation handling | Caller-visible signal |
|---|---|---|---|---|
| Signature, issuer, audience, expiry or replay failure | `unauthenticated` | 401 | None taken | `WWW-Authenticate`, problem+json |
| Version not promoted for env | `agent_not_promoted` | 403 | None taken | `detail` names the version and env |
| Pool not in entitlement, or scope missing | `forbidden_pool` | 403 | None taken | `detail` names the pool |
| Body malformed or unsupported parameter | `invalid_request` | 400 | None taken | `detail` names the field |
| Logical model unknown for tenant | `unknown_model` | 404 | None taken | `GET /v1/models` is the remedy |
| Prompt exceeds logical model window | `context_too_large` | 413 | None taken | `detail` carries estimated and maximum |
| Same idempotency key, different body | `idempotency_conflict` | 409 | None taken | 24 h key window |
| RPM exceeded | `rate_limited` | 429 | None taken | `Retry-After` |
| TPM or monthly budget exceeded | `quota_exceeded` | 429 | Reserve refused | `Retry-After`, `retry_after_seconds`, ratelimit headers |
| Input guardrail blocked | `guardrail_blocked` | 403 | Released in full | `x-agentgate-guardrail: blocked:<category>` |
| No healthy compatible backend at route time | `no_healthy_backend` | 503 | Released in full | `Retry-After` |
| All backends exhausted during invoke | `no_healthy_backend` | 503 | Released in full | `x-agentgate-attempts` reflects the attempts made |
| Upstream unrecoverable error | `provider_error` | 502 | Released in full | `x-agentgate-provider` names the last backend tried |
| Upstream deadline exceeded after retries | `provider_timeout` | 504 | Released in full | — |
| Caller deadline exceeded | `client_timeout` | 408 | Released in full | — |
| Caller disconnected mid-stream | `client_closed_request` | 499 | Settled at tokens actually generated | Nothing; the caller is gone. Usage record still written |
| Output guardrail blocked, unary | `guardrail_blocked` | 403 | Settled at actual | Response body replaced by problem+json |
| Output guardrail blocked, stream already started | — | Stream already 200 | Settled at actual | SSE `error` frame, no `[DONE]`, `x-agentgate-guardrail` already sent as `pass` in headers; the error frame carries the block |

The last row is the one consumers most often get wrong. Once the response headers are on the wire,
the status code is fixed. A guardrail block discovered mid-stream can only be signalled in-band.
This is documented in `04-gateway-contract.md` §6.

---

## 2. Agent onboarding — repo to identity

```mermaid
flowchart TD
  A1["Engineer adds agentgate.yaml registration manifest to the agent repository"] --> A2["Manifest declares identity, owner, on-call, cost centre, data classification, runtime, framework, requested pools, requested quota"]
  A2 --> A3["Pull request - platform team reviews manifest as code"]
  A3 --> D1{"Manifest schema valid and fields complete?"}
  D1 -->|"no"| A3F(["CI fails - registration_complete would not pass"])
  D1 -->|"yes"| A4["Merge to main"]

  A4 --> A5["CI pipeline builds agent image and runs agentctl register --env dev"]
  A5 --> A6["controlplane upserts registration record and allocates agent_id"]
  A6 --> D2{"Runtime supports workload identity?"}

  D2 -->|"yes"| B1["Federated mode - platform issues projected SA token, managed identity, IRSA or SPIFFE JWT-SVID"]
  B1 --> B2["Agent calls controlplane token exchange RFC 8693 at runtime"]
  B2 --> B3["AgentGate access token issued with agent claims, attestation workload-identity"]

  D2 -->|"no"| C1["Client-credentials mode - controlplane issues client_id and client_secret"]
  C1 --> C2["Secret written to enterprise vault, never returned again, 90-day rotation clock starts"]
  C2 --> C3["Agent reads secret from vault at start, exchanges for access token"]
  C3 --> B3

  B3 --> A7["First gateway call in dev succeeds"]
  A7 --> A8["identity_attested gate begins to be satisfiable"]
  A8 --> A9["Telemetry begins flowing, completeness ratio computed every 60s"]
  A9 --> DONE(["Agent live in dev, eligible to request promotion to staging"])
```

### 2.1 Walkthrough

Onboarding starts in the agent's own repository, not in a platform console. The registration
manifest is reviewed as code by the platform team, which is where the ownership, on-call and cost
centre fields actually get checked by a human. CI registers the agent in `dev` on every merge, so
the registry never drifts from what is deployed.

The branch at "runtime supports workload identity" is the single most consequential decision in
onboarding. Federated mode means no long-lived secret exists at any point. Client credentials mode
is a fallback that carries a 90-day rotation obligation and a vault dependency; teams choosing it
should know they are choosing operational work.

`identity_attested` cannot be satisfied by registration alone. The agent must actually authenticate
with `attestation=workload-identity` in the source environment — a registered agent that has never
run is not promotable, which is deliberate.

### 2.2 Failure branches

| Branch | Symptom | Resolution |
|---|---|---|
| Manifest missing owner, on-call, cost centre or classification | CI fails at schema validation | Fix the manifest; this is `registration_complete` shifted left |
| Requested pool does not exist for the tenant | Registration succeeds, `authz` later fails with `forbidden_pool` | Platform adds the pool or the team requests a different one |
| Requested quota exceeds team envelope | Registration succeeds; `quota_declared` gate fails at promotion | Attach an exception or reduce the request |
| Workload identity federation not configured for the runtime | Token exchange fails at runtime | Configure federation, or fall back to client credentials with the rotation obligation accepted |
| Client secret leaked or lost | Agent cannot authenticate | Rotation runbook; see flow 11 in `03-sequences.md` |
| Agent registers but never calls | `identity_attested` never satisfied | Expected; run the agent in dev before requesting promotion |

---

## 3. Promotion gate — dev to staging to prod

```mermaid
flowchart TD
  R1["Team requests promotion via agentctl promote or fleetview UI"] --> R2["controlplane evaluates all automated gates against the source env"]

  R2 --> G1{"registration_complete - owner, on-call, cost centre, classification present"}
  G1 -->|"fail"| FAIL(["Promotion refused - gate snapshot returned with the failing gate named"])
  G1 -->|"pass"| G2{"identity_attested - authenticated at least once with attestation workload-identity in source env"}
  G2 -->|"fail"| FAIL
  G2 -->|"pass"| G3{"telemetry_healthy - completeness at or above 95 percent over 24h and at least 100 requests observed"}
  G3 -->|"fail"| FAIL
  G3 -->|"pass"| G4{"error_budget - agent success SLI meets its objective over 7d in source env"}
  G4 -->|"fail"| FAIL
  G4 -->|"pass"| G5{"guardrail_clean - no unresolved critical guardrail violations in 7d"}
  G5 -->|"fail"| FAIL
  G5 -->|"pass"| G6{"quota_declared - requested quota within team envelope or exception attached"}
  G6 -->|"fail"| FAIL
  G6 -->|"pass"| G7{"cost_projection - projected monthly spend within team budget or exception attached"}
  G7 -->|"fail"| FAIL
  G7 -->|"pass"| G8{"Target env is prod?"}

  G8 -->|"no - staging"| STG["Version state set to active in staging, gateway registry cache invalidated"]
  STG --> STGDONE(["Promoted to staging on automated gates alone"])

  G8 -->|"yes"| G9{"security_review - linked non-expired ServiceNow CHG or RITM reference"}
  G9 -->|"fail"| FAIL
  G9 -->|"pass"| SNAP["Gate snapshot frozen - every gate result, input values, timestamp, evaluator version"]

  SNAP --> D1{"ServiceNow integration enabled?"}
  D1 -->|"yes"| SN1["Emit ServiceNow-compatible change payload, create or link change record"]
  D1 -->|"no"| SN2["Documented manual fallback - change reference entered by hand, same recorded evidence"]

  SN1 --> AP1["Approval request raised to owning team and platform approvers"]
  SN2 --> AP1

  AP1 --> AP2{"Owning-team approver approves and is not the requester?"}
  AP2 -->|"no"| PEND["Pending - request remains open until expiry"]
  AP2 -->|"yes"| AP3{"Platform approver approves, is not the requester, and is not the owning-team approver?"}
  AP3 -->|"no"| PEND
  AP3 -->|"yes"| PROM["Version state set to active in prod, previous prod version state set to superseded"]

  PROM --> INV["Registry change published, gateway registry caches invalidated within TTL"]
  INV --> AUD["Audit record written - actor, timestamp, gate snapshot, change reference, both approvals"]
  AUD --> PRODDONE(["Version live in prod - authz stage now permits it"])

  PEND --> EXP{"Approval window expired?"}
  EXP -->|"yes"| FAIL
  EXP -->|"no"| AP2
```

### 3.1 Walkthrough

Gates are evaluated in a fixed order and the first failure stops evaluation, but the response
returns the full snapshot with every gate's state so a team fixes everything in one pass rather than
discovering failures one at a time.

`telemetry_healthy` and `error_budget` are the two gates that cannot be satisfied by paperwork. They
require the agent to have actually run, correctly instrumented, at acceptable quality, in the source
environment. This is the mechanism that makes telemetry a control rather than a dashboard: an agent
whose traces do not arrive cannot reach production.

The two-party rule is enforced structurally, not by convention. The requester is excluded, and the
two approvers must be distinct and drawn from different groups — one owning-team, one platform.
Approvals are recorded with actor, timestamp and the exact evaluated gate snapshot, so an auditor
can reconstruct not just that a version was approved but what was true at the moment it was
approved.

The ServiceNow branch matters for delivery: the integration is not on the critical path. When it is
disabled, the documented manual path produces the same evidence, with the change reference entered
by hand. What is never optional is the evidence.

### 3.2 Failure branches

| Branch | Cause | What the team does |
|---|---|---|
| `telemetry_healthy` fails at 94% | Missing OTLP export from one replica, or clock skew | `fleetview` shows the per-agent completeness breakdown; fix instrumentation and wait for the 24 h window |
| `telemetry_healthy` fails on volume | Fewer than 100 requests in the source env | Generate representative traffic; a version with no evidence is not promotable |
| `error_budget` fails | The agent itself is failing, often from its own retry loop | Fix the agent; the gate is measuring the agent's own success SLI |
| `guardrail_clean` fails | Unresolved critical violation in the last 7 d | Resolve or formally accept the violation with a record |
| `cost_projection` fails | Projection based on staging volume exceeds budget | Attach a funded exception or reduce scope |
| `security_review` expired | The linked CHG or RITM aged out | Re-link a current reference; expiry is checked at evaluation, not at approval |
| One approval obtained, second never arrives | Approver unavailable | Request expires; re-request. **[Decision]** default approval window is 72 h |
| Requester attempts to self-approve | Structural rule violation | Rejected by the control plane, recorded as a rejected attempt in the audit trail |
| ServiceNow unavailable during a production incident fix | Integration down | Documented manual fallback with the same evidence. This is why the fallback exists |
| Rollback needed after promotion | Bad version in prod | Promote the previous version, which is a state change with its own two-party approval; **[Decision]** an emergency path allows a single platform approver to revert to a previously-active version, recorded as an emergency action and reviewed within 24 h |

---

## 4. Token quota reserve and settle lifecycle

```mermaid
flowchart TD
  Q1["Stage 6 quota.tokens begins"] --> Q2["Estimate input tokens - tokenizer or four-chars-per-token heuristic"]
  Q2 --> Q3["reserve_amount equals estimated_input plus max_tokens"]
  Q3 --> Q4["Atomic Lua script against Redis bucket tenant:team:agent:env"]
  Q4 --> D1{"Bucket has capacity and monthly budget not exhausted?"}

  D1 -->|"no"| Q5["Compute retry_after_seconds from refill rate"]
  Q5 --> QFAIL(["429 quota_exceeded - no reservation held"])

  D1 -->|"yes"| Q6["Reservation held, reservation_id attached to request context"]
  Q6 --> Q7["Stages 7 to 14 proceed"]

  Q7 --> D2{"Terminal outcome"}
  D2 -->|"success unary"| S1["actual equals usage.input_tokens plus usage.output_tokens from provider"]
  D2 -->|"success stream"| S2["actual accumulated from usage frame, or counted from delivered chunks if provider omits usage"]
  D2 -->|"cache hit"| S3["actual equals zero billable tokens - full reservation released, savings_usd recorded"]
  D2 -->|"error before invoke"| S4["actual equals zero - full reservation released"]
  D2 -->|"error after partial generation"| S5["actual equals tokens generated before failure"]
  D2 -->|"client disconnect mid-stream"| S5

  S1 --> SET["Settle - release reserve_amount minus actual back to the bucket"]
  S2 --> SET
  S3 --> SET
  S4 --> SET
  S5 --> SET

  SET --> M1["Record actual against monthly budget counter"]
  M1 --> M2["Emit UsageRecord with input_tokens, output_tokens, cost_usd, billable flag"]
  M2 --> M3["Set x-agentgate-ratelimit-limit-tokens, -remaining-tokens, -reset headers"]
  M3 --> QDONE(["Settled"])

  Q6 -.->|"process crash or panic"| TTL["Reservation TTL expires and capacity is reclaimed automatically"]
  TTL --> QDONE
```

### 4.1 Walkthrough

Reservation exists because post-hoc metering cannot stop the request that blows the budget. A single
long generation can be a material share of a minute's quota, so the platform claims capacity before
the request runs and returns the unused portion afterwards.

The estimate is deliberately conservative: input estimate plus the caller's `max_tokens`, which is
the maximum the request can possibly consume. Over-reservation temporarily under-serves the agent;
settle corrects it within the request's own lifetime.

Every terminal path settles, including errors and disconnects. A reservation that is never settled
is a slow quota leak, which is why reservations also carry a TTL — **[Decision]** the reservation
TTL is the request deadline plus 30 s, so a crashed pod cannot strand capacity for more than that.

The `x-agentgate-ratelimit-*` headers reflect post-settle state, so a caller pacing itself on those
headers sees the true remaining budget rather than the reserved-but-unused figure.

### 4.2 Failure branches

| Branch | Behaviour |
|---|---|
| Redis unavailable at reserve | Fail-open with a per-pod local ceiling as backstop; `agentgate.ratelimit.decisions` records the degraded decision and an alert fires. Rationale in `01-architecture.md` §9 |
| Redis unavailable at settle | Settle is retried with a bounded queue; if it never lands, the reservation TTL reclaims capacity and the monthly budget is reconciled from usage records, which are on a separate path |
| Provider omits usage on a stream | Tokens counted from delivered chunks using the same tokenizer as the estimate; `agentgate.usage.estimated=true` set on the span so the figure is not mistaken for authoritative |
| Monthly budget exhausted mid-minute | `quota_exceeded` with a `retry_after_seconds` reflecting the month boundary, not the bucket refill; the `detail` says which limit was hit |
| `max_tokens` omitted by caller | Logical model default is used for the reservation. **[Decision]** documented in `04-gateway-contract.md` because it affects a caller's effective quota |

---

## 5. Failover and circuit breaker

```mermaid
flowchart TD
  I1["Stage 11 invoke - backend selected"] --> I2["Attempt against backend with timeout min of remaining deadline and backend_timeout"]
  I2 --> D1{"Outcome"}

  D1 -->|"success"| OK(["Response returned - x-agentgate-attempts set"])

  D1 -->|"non-retryable 4xx from provider"| E502(["502 provider_error - no retry, no failover"])

  D1 -->|"retryable - 429 with Retry-After, 5xx, connection error, timeout before first byte"| R1["Record failure in breaker sliding window of 50"]

  R1 --> D2{"Breaker trip condition - 50 percent failure rate or 10 consecutive failures?"}
  D2 -->|"yes"| B1["Breaker opens for 30s - agentgate.breaker.state gauge set to open"]
  D2 -->|"no"| R2{"Retry budget available - under retry_budget_ratio of deadline and fleet retry rate under 10 percent?"}

  R2 -->|"yes"| R3["Exponential backoff with full jitter, retry same backend"]
  R3 --> I2
  R2 -->|"no"| B1

  B1 --> F1{"Another healthy backend in the same priority tier?"}
  F1 -->|"yes"| F2["Failover within tier - set agentgate.failover.from"]
  F2 --> D3{"Stream already started - first content byte delivered?"}
  D3 -->|"no"| F3["Emit event agentgate.failover SSE frame, restart stream on new backend"]
  F3 --> I2
  D3 -->|"yes"| ERRFRAME(["SSE error frame - stream is never silently restarted after first content byte, no DONE"])

  F1 -->|"no"| F4{"Next priority tier has healthy compatible backends?"}
  F4 -->|"yes"| F5["Failover to next tier - typically on-prem inference"]
  F5 --> D3
  F4 -->|"no"| E503(["503 no_healthy_backend"])

  B1 --> H1["After 30s breaker moves to half-open"]
  H1 --> H2["Admit up to 5 probe requests"]
  H2 --> D4{"Probes succeed?"}
  D4 -->|"yes"| H3["Breaker closes - backend returns to rotation"]
  D4 -->|"no"| B1
```

### 5.1 Breaker state machine

```mermaid
stateDiagram-v2
  [*] --> Closed
  Closed --> Open: 50 percent failure rate over sliding window of 50, or 10 consecutive failures
  Open --> HalfOpen: 30s elapsed
  HalfOpen --> Closed: 5 probes succeed
  HalfOpen --> Open: any probe fails
  Closed --> Closed: success recorded in window
  note right of Open
    Backend excluded from selection.
    agentgate.breaker.state gauge reports open.
    All backends open in a pool yields no_healthy_backend.
  end note
```

### 5.2 Walkthrough

Retries are attempted only for idempotent failures: 429 carrying `Retry-After`, 5xx, connection
errors, and timeouts that occur before the first byte. A timeout after the first byte is not
retryable because the provider may have already done the work and, for streams, the caller may
already have seen output.

Two budgets bound retries. The per-request budget caps time spent retrying at `retry_budget_ratio`
of the deadline, default 0.25. The fleet-wide budget caps retries at 10% of request volume, which is
what stops a partial provider outage from becoming a self-inflicted denial of service. When the
fleet budget is exhausted, retries stop and requests fail fast; this is the intended behaviour and
should be visible on `agentgate.retry.attempts`.

Failover moves within the tier first, then to the next priority tier. In the reference topology the
next tier is on-prem inference, which is why capacity there needs to be planned for provider-outage
load rather than steady-state load.

The stream case is the one with a real semantic constraint. Before the first content byte, the
caller has seen nothing, so restarting on another backend is invisible and correct; an
`event: agentgate.failover` frame is emitted so an observant client can record it. After the first
content byte, restarting would produce a response that is a concatenation of two different
generations. The contract forbids it: the stream ends with an SSE `error` frame and no `[DONE]`.

### 5.3 Failure branches

| Branch | Behaviour | Caller sees |
|---|---|---|
| Provider returns 429 without `Retry-After` | Treated as retryable with backoff derived from the schedule, not the header | Increased `x-agentgate-attempts` |
| All backends in tier open, next tier classification-incompatible | Not a valid failover target; classification filter runs before tier selection | `no_healthy_backend` |
| Breaker flaps — closes then immediately reopens | Half-open probe budget of 5 limits damage per cycle; sustained flapping raises the breaker-open alert | Elevated latency and attempts |
| Failover succeeds but on a more expensive backend | Correct behaviour; cost attributed to the backend actually used | `x-agentgate-provider`, `x-agentgate-cost-usd` reflect reality |
| Hedged request wins on the second backend | First attempt cancelled; only the winning attempt is billed | `x-agentgate-attempts` reflects attempts made |
| Deadline exhausted mid-failover | No further attempts | `provider_timeout` |
| Caller disconnects during failover | Attempt cancelled, reservation settled at actual | `client_closed_request` recorded, nothing delivered |

---

## 6. Telemetry flow — three runtimes into the collector tiers

```mermaid
flowchart TD
  subgraph src["Signal sources"]
    K8S["AKS or EKS agent pod - OTel SDK"]
    LAM["Lambda agent - OTel SDK, short-lived"]
    VMA["VM-hosted agent - OTel SDK, long-lived"]
    GWS["gateway - OTel SDK"]
    CPS["controlplane - OTel SDK"]
  end

  K8S -->|"OTLP gRPC to node-local endpoint"| CAG["Collector agent tier - DaemonSet"]
  GWS -->|"OTLP gRPC"| CAG
  CPS -->|"OTLP"| CAG
  LAM -->|"OTLP direct - no DaemonSet available, flush before freeze"| CGW
  VMA -->|"OTLP direct over private link"| CGW

  CAG --> PROC1["Agent-tier processors - batch, memory_limiter, k8sattributes"]
  PROC1 -->|"load-balancing exporter keyed on trace id"| CGW["Collector gateway tier - HA pool"]

  CGW --> PROC2["attributes processor - stamp verified ownership from gateway-authored attributes"]
  PROC2 --> D1{"Ownership attributes disagree with token-derived values?"}
  D1 -->|"yes"| CORR["Correct to token values and set agentgate.attribution.corrected true"]
  D1 -->|"no"| PROC3
  CORR --> PROC3["redaction processor"]

  PROC3 --> D2{"Span carries content events?"}
  D2 -->|"yes"| CONTENT["Content pipeline - separate, non-sampled, access-controlled, own retention"]
  D2 -->|"no"| PROC4["tail_sampling processor"]

  PROC4 --> D3{"Sampling decision"}
  D3 -->|"error"| KEEP["Keep - 100 percent"]
  D3 -->|"guardrail block"| KEEP
  D3 -->|"failover occurred"| KEEP
  D3 -->|"latency above p99"| KEEP
  D3 -->|"otherwise"| BASE["Keep 5 percent baseline"]

  KEEP --> ROUTE["routing processor"]
  BASE --> ROUTE

  ROUTE --> OUT1["traces to Langfuse or self-hosted OTLP store"]
  ROUTE --> OUT2["traces to Azure Monitor or X-Ray"]
  ROUTE --> OUT3["metrics to Prometheus or managed Prometheus"]
  ROUTE --> OUT4["logs to Loki or Log Analytics"]
  ROUTE --> OUT5["cost to chargeback exporter - Parquet to warehouse"]

  CONTENT --> CSTORE["Content store - restricted access, separate retention policy"]

  OUT3 --> FV["fleetview - computes completeness, orphan ratio, unattributed ratio, clock skew every 60s"]
  FV --> TRUST["agentgate.telemetry.completeness gauge - hard input to the promotion gate"]
```

### 6.1 Walkthrough

Three runtimes, two ingress paths. Kubernetes workloads and the platform services themselves emit to
a node-local collector, which batches, applies `memory_limiter`, and enriches with Kubernetes
attributes. Lambda and VM agents emit directly to the gateway tier because no DaemonSet exists for
them; they lose Kubernetes enrichment they do not need.

Lambda deserves specific attention: the execution environment can freeze immediately after the
handler returns, so the SDK must flush synchronously before returning. An unflushed Lambda span is
the most common cause of a completeness ratio that mysteriously sits at 90%.

The agent tier uses a load-balancing exporter keyed on trace id. This is not optional. Tail sampling
requires that every span of a trace arrives at the same gateway-tier instance; without trace-affine
routing the sampler sees fragments and makes inconsistent decisions.

Attribute stamping is where the platform's ownership guarantee is realised. The gateway derives
ownership from the verified token; the collector reconciles what the agent self-reported against
that. Disagreement is corrected in favour of the token and flagged with
`agentgate.attribution.corrected=true` so it is visible and fixable rather than silently papered
over.

The content pipeline is physically separate. Prompt and completion content never rides the sampled
trace path; it is emitted as span events on a dedicated pipeline with its own retention and access
control, redacted by the guardrail engine first. In production for this client the default is `off`.

### 6.2 Failure branches

| Branch | Effect | Detection |
|---|---|---|
| Agent-tier collector OOM | Spans dropped at the node | `memory_limiter` refusals; completeness ratio drops for agents on that node |
| Gateway-tier collector down | Agent-tier persistent queue buffers, then drops | Queue-size metric; completeness drop; **traffic is unaffected** |
| Load-balancing exporter misconfigured | Trace fragments split across samplers, inconsistent decisions | `orphan_span_ratio` rises sharply |
| Lambda does not flush before freeze | Missing agent spans, gateway spans present | `orphan_span_ratio` and completeness both degrade for that agent only |
| Clock skew on a VM agent | Spans appear out of order or with negative durations | `clock_skew_p99` metric; NTP remediation |
| Backend observability store rejects or throttles | Retry with backoff from the persistent sending queue | Exporter failure metrics |
| Content capture accidentally enabled in prod | Sensitive content in a store not provisioned for it | Configuration is asserted in the deploy pipeline and alerted on; see `06-telemetry-schema.md` §7 |

---

## 7. Cost attribution and chargeback

```mermaid
flowchart TD
  C1["Stage 15 meter - request completed"] --> C2["Resolve unit costs for the backend actually used - per 1M input and output tokens"]
  C2 --> C3["cost_usd equals input_tokens times unit_cost_input plus output_tokens times unit_cost_output, divided by one million, rounded to 6dp"]
  C3 --> C4["Compose immutable UsageRecord - ts, request_id, trace_id, tenant, team, agent_id, env, cost_center, logical_model, provider, backend_model, tokens, unit costs, cost_usd, cache, attempts, billable"]

  C4 --> D1{"Served from cache?"}
  D1 -->|"yes"| C5["billable false, savings_usd equals the cost the call would have incurred"]
  D1 -->|"no"| C6["billable true"]

  C5 --> C7["Append to usage stream - Event Hubs or Kinesis in prod, append-only file locally"]
  C6 --> C7

  C7 --> C8["Set x-agentgate-cost-usd response header, 6dp"]
  C7 --> R1["Hourly rollup by cost centre, team, agent, env, backend"]
  R1 --> R2["Daily rollup"]
  R2 --> R3["Monthly rollup"]

  R3 --> X1["Chargeback API at /api/v1/chargeback"]
  R3 --> X2["Monthly export - one invoice line per cost centre with agent-level detail"]

  X2 --> REC1["Reconciliation against the cloud provider bill"]
  REC1 --> D2{"Variance within tolerance?"}
  D2 -->|"yes"| INV(["Invoice line published to the cost centre owner"])
  D2 -->|"no"| REC2["Variance investigation - unit price drift, unmetered path, provider-side rounding, failed settle"]
  REC2 --> REC3["Correction recorded as an adjustment line, never by mutating UsageRecords"]
  REC3 --> INV

  R1 --> AN1["Cost anomaly detection - EWMA plus three sigma per agent per hour"]
  AN1 --> D3{"Anomaly or daily ceiling breach?"}
  D3 -->|"yes"| AN2["Alert to owning team on-call and FinOps"]
  D3 -->|"no"| AN3["No action"]
```

### 7.1 Walkthrough

Cost is computed at the moment of metering against the backend actually used, not the backend
requested. A request that failed over from a cheap backend to an expensive one is billed at the
expensive one, because that is what happened.

`UsageRecord` is immutable. Corrections are adjustment lines, never mutations. This is what makes
the record set usable as evidence: an auditor can replay the stream and arrive at the same number
that appeared on the invoice.

Cache hits are recorded rather than omitted. A hit with `billable:false` and a `savings_usd` figure
is how the platform demonstrates value to the teams funding it; omitting cache hits would make the
platform look less used than it is and hide the saving entirely.

Reconciliation against the cloud bill is the honest part of the design. The gateway's figure and the
provider's invoice will not agree exactly — rounding, provider-side batching discounts, and
unmetered paths all contribute. The process is to detect variance, explain it, and record an
adjustment, not to assume the gateway is right.

### 7.2 Failure branches

| Branch | Effect | Handling |
|---|---|---|
| Usage stream unavailable | Records buffered locally, replayed on recovery | Requests are never failed for a metering failure |
| Unit price table stale after a provider price change | Systematic cost error until corrected | Prices are versioned with effective dates; see `08-chargeback.md` §3 |
| Provider omits usage on a stream | Token counts estimated, `agentgate.usage.estimated=true` | Reconciliation catches systematic drift |
| `cost_center` missing from token | Cannot reach `env=prod` at all — the claim is required | `authz` refuses; this is a SPEC-level rule |
| Request fails after partial generation | Tokens generated are still billed, `billable:true` | The provider charged for them |
| Duplicate record from a stream replay | Deduplicated on `request_id` at rollup | `request_id` is unique per gateway request |

---

## 8. Migration — freeze to decommission

```mermaid
flowchart TD
  M1["Phase 1 - Freeze and document the existing contract, publish as gateway.v1.yaml"] --> M2["Phase 2 - Compatibility suite - golden corpus captured from production traffic"]
  M2 --> M2A["Replay corpus against legacy and AgentGate, byte-level diff on body, headers and error mapping"]
  M2A --> D1{"Zero unexplained diffs?"}
  D1 -->|"no"| M2B["Fix AgentGate, or record an accepted diff with rationale and consumer impact assessment"]
  M2B --> M2A
  D1 -->|"yes"| M3["Phase 3 - Shadow - AgentGate receives mirrored traffic, responses discarded"]

  M3 --> M3A["Diff report per consumer, per endpoint, per error class"]
  M3A --> D2{"Shadow diff rate acceptable and stable for the agreed soak period?"}
  D2 -->|"no"| M3B["Fix and continue shadow - no consumer impact so this loop is cheap"]
  M3B --> M3A
  D2 -->|"yes"| M4["Phase 4 - Canary by consumer at 1 percent"]

  M4 --> M4A["5 percent"] --> M4B["25 percent"] --> M4C["50 percent"] --> M4D["100 percent for this consumer"]

  M4 --> D3{"SLO burn or contract diff detected?"}
  M4A --> D3
  M4B --> D3
  M4C --> D3
  M4D --> D3
  D3 -->|"yes"| RB["Automatic rollback - weight change at the DNS or front-door layer, not a deploy"]
  RB --> M3B

  M4D --> M5["Phase 5 - Cutover recorded for this consumer, never per endpoint"]
  M5 --> D4{"All consumers cut over?"}
  D4 -->|"no"| M4
  D4 -->|"yes"| M6["Phase 6 - Soak at 100 percent for 30 days with zero contract diffs"]

  M6 --> D5{"Thirty days clean?"}
  D5 -->|"no"| RB
  D5 -->|"yes"| M7["Decommission legacy service - read-only, then stopped, then deleted"]
  M7 --> DONE(["Migration complete - legacy contract now owned by AgentGate v1"])
```

### 8.1 Walkthrough

The migration is a strangler with the contract held fixed. The key move is Phase 1: the existing
contract is documented and frozen as `gateway.v1.yaml`, and AgentGate v1 *is* that contract. No
consumer changes code, so no consumer has to be scheduled.

Cutover is per consumer, never per endpoint. A consumer that saw one behaviour on
`/v1/chat/completions` and another on `/v1/embeddings` would be debugging a difference the platform
created. Per-consumer cutover means a single consumer always sees one implementation.

Rollback at every stage is a weight change at the routing layer. This is the property that makes the
canary steps safe enough to be automatic: rollback is seconds, not a deploy cycle.

The 30-day zero-diff soak before decommission is not a formality. Monthly-cycle behaviours — month
end batch jobs, quarterly reporting agents — only appear in a window that long.

### 8.2 Failure branches

| Branch | Response |
|---|---|
| Corpus diff on an error mapping | Highest-priority class of diff. Error codes and statuses are frozen contract; a mismatch is a defect, not a difference of opinion |
| Diff only under load | Shadow at production volume before canary; concurrency-dependent behaviour will not appear in a replay harness |
| Consumer depends on undocumented legacy behaviour | Found in shadow. Either replicate it and document it as part of v1, or negotiate a change with that consumer before their canary |
| SLO burn during canary | Automatic rollback to 0% for that consumer; investigation before re-attempting |
| A consumer cannot be identified | Cutover is blocked for that traffic. **[Decision]** unattributed traffic remains on legacy until identified; migrating traffic whose owner is unknown means having no one to call when it breaks |
| Legacy service needed after decommission | Decommission is staged: read-only, then stopped but restorable for 30 days, then deleted |

---

## 9. Incident flow

```mermaid
flowchart TD
  A1["Alert fires - multi-window burn rate, breaker open, telemetry degraded, or cost anomaly"] --> A2["Alert annotation carries the runbook link - every alert has one"]
  A2 --> T1["On-call acknowledges within the page SLA"]

  T1 --> T2{"Is model traffic affected?"}
  T2 -->|"no"| T3["Ticket severity - handle in hours, not now"]
  T2 -->|"yes"| T4{"Scope of impact"}

  T4 -->|"one agent"| S1["Likely agent-side - quota, guardrail policy, bad prompt shape. Owning team engaged"]
  T4 -->|"one pool or backend"| S2["Likely provider-side - check agentgate.breaker.state and provider status"]
  T4 -->|"all traffic"| S3["Platform-side - declare incident, page platform lead, open comms channel"]

  S1 --> R1["Follow runbook"]
  S2 --> R1
  S3 --> R1

  R1 --> D1{"Recent change in the last 24h - gateway deploy, policy change, pool config, canary weight?"}
  D1 -->|"yes"| M1["Roll back the change first, diagnose after"]
  D1 -->|"no"| D2{"Dependency healthy - Redis, control plane, guardrail service, collector?"}

  D2 -->|"no"| M2["Apply the documented degradation - quota fail-open, stale JWKS, buffered usage records"]
  D2 -->|"yes"| D3{"Provider healthy?"}
  D3 -->|"no"| M3["Shift weight to the failover tier or manually open the breaker on the affected backend"]
  D3 -->|"yes"| M4["Deep diagnosis - per-stage duration attributes on the gateway.request span show where latency or failure sits"]

  M1 --> V1["Verify - error rate and burn rate returning to baseline"]
  M2 --> V1
  M3 --> V1
  M4 --> V1

  V1 --> D4{"Mitigated?"}
  D4 -->|"no"| D5{"Escalation criteria met - 30 minutes without mitigation, or severity increased?"}
  D5 -->|"yes"| ESC["Escalate - platform lead, provider support, client incident manager"]
  ESC --> M4
  D5 -->|"no"| M4
  D4 -->|"yes"| C1["Communicate mitigation to affected consuming teams and the client incident channel"]

  C1 --> C2["Monitor for the burn-rate window to clear"]
  C2 --> P1["Postmortem scheduled within 5 working days - blameless, written"]
  P1 --> P2["Postmortem records timeline, contributing factors, error budget consumed, detection gap, action items with owners and dates"]
  P2 --> P3{"Error budget exhausted for the period?"}
  P3 -->|"yes"| P4["Error-budget policy engages - feature work stops, reliability work only. See 07-slo-alerting.md"]
  P3 -->|"no"| P5["Action items enter the backlog with owners"]
  P4 --> P5
  P5 --> DONE(["Closed"])
```

### 9.1 Walkthrough

The first decision after acknowledgement is scope, because scope determines who is even involved.
One agent affected is usually not a platform incident: it is quota, a guardrail policy, or the
agent's own behaviour, and the owning team is the right responder. One pool or backend affected is
usually a provider incident and the mitigation is traffic shifting. All traffic affected is a
platform incident with a declared severity and a comms channel.

"Recent change?" comes before "deep diagnosis" deliberately. Rolling back a change is faster than
understanding it, and the architecture is built so that rollback is cheap — canary weights, pool
configuration, and promotion state are all reversible without a deploy.

Per-stage duration attributes are the diagnostic backbone. `agentgate.policy.<stage>.duration_ms` on
the `gateway.request` span answers "where did the latency go" directly. An incident where the answer
is not immediately visible in that data is itself a finding for the postmortem.

The error-budget policy branch at the end is not decorative. It is the mechanism that converts
reliability from an aspiration into a constraint on what the team is allowed to work on next.

### 9.2 Decision points, stated explicitly

| Decision | Criterion |
|---|---|
| Page vs ticket | Page for fast-burn windows and total-traffic impact. Ticket for slow burn, single-agent issues, and telemetry degradation that is not affecting traffic |
| Declare an incident | Model traffic affected across more than one consumer, or any duration of total outage |
| Roll back before diagnosing | Any change in the preceding 24 h that plausibly touches the symptom |
| Manually open a breaker | Provider is degraded in a way the automatic breaker is not catching, such as correct-status-code garbage responses |
| Escalate | 30 minutes without mitigation, severity increases, or a dependency owned by another team is implicated |
| Engage the owning team rather than the platform | Impact confined to one agent identity |
| Postmortem required | Any page, any customer-visible incident, any error-budget consumption above 10% of the period budget in a single event |
