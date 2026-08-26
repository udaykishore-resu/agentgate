# AgentGate — Enterprise Agentic AI Control Plane

**Status:** v1 interface FROZEN. Internal implementation may change; the wire contract in
`api/openapi/gateway.v1.yaml` may not.

AgentGate is the control plane every agent passes through, regardless of where that agent runs.
It is one plane with three faces:

| Plane | Component | Responsibility |
|---|---|---|
| **Traffic** | `gateway` | Provider-agnostic model API. Routing, pools, quotas, retries, failover, circuit breaking, cache, transformation, guardrails, streaming. |
| **Trust** | `controlplane` | Agent workload identity, registration on deploy, promotion gate to production, credential issuance, vault-backed secrets. |
| **Truth** | `fleetview` + OTel pipeline | Telemetry schema, trace/metric/log pipelines, fleet inventory, SLI/SLO/error budgets, cost attribution and chargeback, alerting. |

They are one binary set on purpose: identity is enforced *at* the gateway, and the gateway is
where most telemetry originates. Splitting them creates the seam this platform exists to avoid.

---

## 1. Core domain objects

### 1.1 Agent identity

An agent's workload identity is a URI, stable across deployments, and is the primary key for
everything else — telemetry attribution, quota, chargeback, promotion.

```
agent://<tenant>/<team>/<agent-name>
```

Example: `agent://fsclient/payments-risk/dispute-triage`

A *deployment* of that agent is qualified by environment and version:

```
agent://fsclient/payments-risk/dispute-triage@2.4.1?env=prod
```

Identity is presented to the gateway as an OAuth2 / OIDC access token whose claims carry the
agent coordinates. Two issuance modes are supported and both validate identically at the gateway:

1. **Federated workload identity (preferred, no secrets).** The agent's runtime platform issues a
   platform token (Kubernetes projected SA token, Azure managed identity, AWS IAM Roles Anywhere /
   IRSA, SPIFFE JWT-SVID). The control plane exchanges it for an AgentGate access token via
   RFC 8693 token exchange. No long-lived secret ever exists.
2. **Client credentials (fallback for runtimes with no workload identity).** A registered agent
   gets a `client_id` + `client_secret`, secret stored in the enterprise vault, rotated on a
   90-day clock, and never returned again after issuance.

### 1.2 Required token claims

```json
{
  "iss": "https://controlplane.agentgate.internal",
  "aud": "https://gateway.agentgate.internal",
  "sub": "agent://fsclient/payments-risk/dispute-triage",
  "exp": 1767225600, "iat": 1767222000, "jti": "01J...",
  "agent_id": "agt_01J8Z9X2QK",
  "tenant": "fsclient",
  "team": "payments-risk",
  "agent_name": "dispute-triage",
  "agent_version": "2.4.1",
  "env": "prod",
  "cost_center": "CC-4471",
  "runtime": "aks",
  "scopes": ["models:invoke", "models:embed"],
  "model_pools": ["general-chat", "long-context"],
  "attestation": "workload-identity"
}
```

`cost_center` is the chargeback key. A token without it cannot reach `env=prod`.

### 1.3 Registration record

```json
{
  "agent_id": "agt_01J8Z9X2QK",
  "identity": "agent://fsclient/payments-risk/dispute-triage",
  "display_name": "Dispute Triage Agent",
  "owner": { "team": "payments-risk", "email": "payments-risk@client.example", "oncall": "PD-PAYRISK", "cost_center": "CC-4471" },
  "runtime": "aks",
  "framework": "langgraph",
  "data_classification": "confidential",
  "requested_pools": ["general-chat", "long-context"],
  "quota": { "tokens_per_minute": 120000, "requests_per_minute": 600, "monthly_token_budget": 900000000 },
  "versions": [
    { "version": "2.4.1", "env": "prod", "state": "active", "promoted_at": "...", "promoted_by": "..." },
    { "version": "2.5.0", "env": "staging", "state": "pending_promotion" }
  ]
}
```

### 1.4 Promotion gate

`dev → staging → prod`. Each transition runs **automated checks** first; production additionally
requires **human approval**. Automated gates (all must pass):

| Gate | Rule |
|---|---|
| `registration_complete` | Owner, on-call, cost centre, data classification present. |
| `identity_attested` | Agent has authenticated at least once with `attestation=workload-identity` in the source env. |
| `telemetry_healthy` | ≥ 95% of the agent's gateway requests in the source env produced a complete, correctly-attributed trace over the last 24h and ≥ 100 requests observed. |
| `error_budget` | Agent's own success SLI ≥ its objective over the last 7d in the source env. |
| `guardrail_clean` | No unresolved critical guardrail violations in the last 7d. |
| `quota_declared` | Requested quota ≤ team's allocated envelope, or an exception is attached. |
| `cost_projection` | Projected monthly spend within the team's budget, or an exception is attached. |
| `security_review` | For `prod` only: a linked, non-expired security review reference (ServiceNow CHG/RITM). |

Human approval for `prod` is a two-party rule: one **owning-team approver** and one
**platform approver**, neither of whom may be the requester. Approvals are recorded with actor,
timestamp, and the exact evaluated gate snapshot, so an auditor can reconstruct why a version was
allowed into production. The gate emits a ServiceNow-compatible change payload; when the
integration is disabled it falls back to a documented manual path with the same recorded evidence.

---

## 2. Gateway v1 wire contract (FROZEN)

Base path: `https://gateway.agentgate.internal/v1`

Wire-compatible with the OpenAI Chat Completions shape so existing SDKs and agent frameworks work
unmodified. This is the interface published to the external engineering team and it does not change.

### 2.1 Endpoints

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/chat/completions` | Chat completion, streaming (SSE) or unary |
| POST | `/v1/embeddings` | Embeddings |
| GET | `/v1/models` | Logical models the caller is entitled to |
| GET | `/v1/models/{id}` | One logical model |
| POST | `/v1/token-count` | Pre-flight token estimate against a logical model |
| GET | `/healthz` `/readyz` | Liveness / readiness (unauthenticated) |
| GET | `/metrics` | Prometheus scrape (network-restricted) |

### 2.2 Request

`Authorization: Bearer <access token>` is required on every `/v1/*` call.

```jsonc
POST /v1/chat/completions
{
  "model": "general-chat",           // LOGICAL model, never a provider deployment name
  "messages": [{"role":"user","content":"..."}],
  "max_tokens": 1024,
  "temperature": 0.2,
  "stream": true,
  "stream_options": {"include_usage": true},
  "tools": [...], "tool_choice": "auto",
  "response_format": {"type":"json_object"},
  "metadata": {                       // optional, joins app context to the trace
    "session_id": "sess_123",
    "conversation_id": "conv_9",
    "step": "classify",
    "tags": ["dispute","tier2"]
  }
}
```

Optional request headers (all `x-agentgate-*`, all safe to omit):

| Header | Meaning |
|---|---|
| `x-agentgate-session-id` | Conversation/session correlation, appears on every span |
| `x-agentgate-request-priority` | `interactive` \| `batch` — selects pool tier and queue discipline |
| `x-agentgate-cache` | `on` \| `off` \| `refresh` (default from policy) |
| `x-agentgate-pool` | Explicit pool override; rejected if not in token's `model_pools` |
| `x-agentgate-idempotency-key` | De-duplicates retried non-stream requests for 24h |
| `traceparent` / `tracestate` | W3C context; the gateway continues the caller's trace |

### 2.3 Response

Body is OpenAI-shaped. Every response — success or error — carries:

| Header | Meaning |
|---|---|
| `x-agentgate-request-id` | Gateway request id (== root span id hex) |
| `x-agentgate-trace-id` | W3C trace id, for cross-referencing in the observability backend |
| `x-agentgate-provider` | Provider that served it (`azure-openai`, `bedrock`, `onprem-vllm`, …) |
| `x-agentgate-model` | Concrete backend model/deployment used |
| `x-agentgate-pool` | Pool selected |
| `x-agentgate-attempts` | Number of backend attempts made |
| `x-agentgate-cache` | `hit` \| `miss` \| `bypass` \| `refresh` |
| `x-agentgate-tokens-input` / `-output` | Billed tokens |
| `x-agentgate-cost-usd` | Attributed cost for this call, 6dp |
| `x-agentgate-ratelimit-limit-tokens` / `-remaining-tokens` / `-reset` | Token budget state |
| `x-agentgate-guardrail` | `pass` \| `blocked:<category>` \| `redacted:<category>` |
| `Retry-After` | On 429/503 |

### 2.4 Errors — RFC 9457 problem+json, stable codes

```json
{ "type":"https://agentgate.internal/errors/quota_exceeded",
  "title":"Token quota exceeded",
  "status":429,
  "detail":"agent://fsclient/payments-risk/dispute-triage exceeded 120000 tokens/min",
  "code":"quota_exceeded",
  "request_id":"...", "trace_id":"...", "retry_after_seconds": 17 }
```

| HTTP | `code` | Cause |
|---|---|---|
| 400 | `invalid_request` | Malformed body / unsupported parameter |
| 401 | `unauthenticated` | Missing, expired, or unverifiable token |
| 403 | `forbidden_pool` | Pool not in token's entitlement |
| 403 | `agent_not_promoted` | Version not promoted for this environment |
| 403 | `guardrail_blocked` | Content safety policy denied the request or response |
| 404 | `unknown_model` | Logical model does not exist for this tenant |
| 408 | `client_timeout` | Caller deadline exceeded |
| 409 | `idempotency_conflict` | Same key, different body |
| 413 | `context_too_large` | Prompt exceeds logical model's window |
| 429 | `rate_limited` | Requests-per-minute limit |
| 429 | `quota_exceeded` | Token-per-minute or monthly budget |
| 499 | `client_closed_request` | Caller disconnected mid-stream |
| 502 | `provider_error` | Upstream returned an unrecoverable error |
| 503 | `no_healthy_backend` | Every backend in the pool is open-circuit or drained |
| 504 | `provider_timeout` | Upstream deadline exceeded after retries |

**Compatibility rule (frozen):** fields may be *added* to responses; no field is removed or
retyped, no `code` is repurposed, no status code for an existing `code` changes. Breaking change
means `/v2`, run side-by-side, with a published deprecation window.

### 2.5 Streaming

SSE, `data:` frames of OpenAI chunk objects, terminated by `data: [DONE]`.
Additional AgentGate frames, all ignorable by a strict OpenAI client:

- `event: agentgate.usage` — final usage + cost, emitted before `[DONE]` when
  `stream_options.include_usage` is true.
- `event: agentgate.failover` — emitted when the stream is restarted on another backend before any
  content token has been delivered. After first content byte the stream is never silently
  restarted; it ends with an SSE `error` frame instead.
- Heartbeat comment `:` every 15s so idle proxies don't drop long generations.

---

## 3. Routing and policy

### 3.1 Logical model → pool → backends

```
logical model "general-chat"
  └── pool "general-chat"  (strategy: weighted-least-load, tier: interactive)
        ├── backend azure-openai/gpt-4o-mini   weight 60  priority 1
        ├── backend bedrock/claude-haiku       weight 40  priority 1
        └── backend onprem-vllm/llama-3.1-8b   weight 100 priority 2   ← failover tier
```

Backends carry: provider adapter, concrete model, weight, priority, per-backend concurrency cap,
per-backend timeout, cost per 1M input/output tokens, data-residency and classification labels.

**Selection order:** filter by health (circuit closed) → filter by classification/residency
compatibility → lowest priority tier with survivors → weighted-least-outstanding within tier.

### 3.2 Policy chain (executed in this order, per request)

```
1  trace.start          root server span, W3C context continued
2  authn                JWT: signature (JWKS), iss, aud, exp/nbf, jti replay window
3  authz                agent promoted for env? pool entitled? scope present?
4  admission            body validate, size cap, param normalisation, context window check
5  ratelimit.requests   per-agent RPM (distributed token bucket)
6  quota.tokens         per-agent TPM + monthly budget; reserve estimate, settle on completion
7  guardrail.input      content-safety callout (fail-open/closed per policy)
8  cache.lookup         exact-match, then optional semantic match
9  transform.request    provider-agnostic → provider-native
10 route                pool selection + backend selection
11 invoke               retry / failover / circuit breaker / hedging window
12 transform.response   provider-native → provider-agnostic
13 guardrail.output     streaming-aware scan
14 cache.store
15 meter               tokens, cost, chargeback record
16 trace.end            span attributes finalised, metrics recorded
```

Every stage is a named `policy.Stage` and is individually timed; its duration lands on the span as
`agentgate.policy.<stage>.duration_ms`. That is how "where did the latency go" is answered without
guessing.

### 3.3 Resilience

- **Retry:** idempotent failures only (429 with `Retry-After`, 5xx, connection errors, timeouts
  before first byte). Exponential backoff, full jitter, budget-capped: a request may not spend more
  than `retry_budget_ratio` (default 0.25) of its deadline retrying, and the *fleet-wide* retry
  budget is capped at 10% of request volume to prevent retry storms.
- **Failover:** after in-backend retries are exhausted, move to the next backend in the tier, then
  the next tier. Streaming responses only fail over before first content byte.
- **Circuit breaker:** per backend, 3-state (closed / open / half-open), sliding window of
  50 requests, trips at 50% failure rate or 10 consecutive failures, opens for 30s, half-open
  admits 5 probes. Breaker state is exported as a metric and drives `no_healthy_backend`.
- **Load shedding:** when gateway concurrency exceeds the configured ceiling, `batch`-priority
  requests are shed with 429 before `interactive` ones.
- **Deadline propagation:** client deadline (or default) becomes the context deadline; each attempt
  gets `min(remaining, backend_timeout)`.

### 3.4 Rate limiting and quota

Token-aware, not just request-aware:

1. **Estimate** input tokens (tokenizer or 4-chars-per-token heuristic) + `max_tokens`.
2. **Reserve** that many tokens from the agent's bucket. Refuse with `quota_exceeded` if reserve
   fails.
3. **Settle** on completion: release the difference between reserved and actual, and record actual
   against the monthly budget.

Buckets are keyed `tenant:team:agent:env` and, for shared-capacity protection, also
`pool:backend`. Redis-backed distributed buckets in production (single atomic Lua script);
in-memory buckets for local/dev with identical semantics.

### 3.5 Caching

- **Exact:** SHA-256 over normalised (model, messages, tools, temperature, top_p, max_tokens,
  response_format, tenant). Never shared across tenants. Skipped for `temperature > 0.2` unless the
  policy explicitly allows it, and never used for tool-calling requests by default.
- **Semantic (optional, per-pool):** embed the final user turn, cosine ≥ threshold (default 0.97)
  against the tenant's recent cache entries. Off by default; requires a data-classification
  allowance because it changes what "the same question" means.
- Cache entries carry the originating trace id, so a cache hit is still fully attributable.

### 3.6 Guardrails

A `GuardrailProvider` interface with three implementations shipped: `noop`, `builtin` (regex/entropy
PII + denylist), and `callout` (HTTP to a managed content-safety service or the client's own
filtering service). Policy per pool declares categories, thresholds, action (`block` | `redact` |
`annotate`), and failure mode (`fail_open` for availability-first pools, `fail_closed` for
regulated pools — `fail_closed` is the default for `data_classification=restricted`).
Output scanning is windowed for streams: content is buffered in ~256-token windows, scanned, then
released, so a violation is caught before the caller sees it while keeping time-to-first-token low.

---

## 4. Telemetry schema

OpenTelemetry, aligned to the `gen_ai.*` semantic conventions and extended with `agentgate.*` for
what those conventions do not yet cover (ownership, chargeback, policy, promotion).

### 4.1 Resource attributes (every signal from every runtime)

| Attribute | Example | Source |
|---|---|---|
| `service.name` | `dispute-triage` | agent |
| `service.version` | `2.4.1` | agent |
| `service.namespace` | `payments-risk` | agent |
| `deployment.environment.name` | `prod` | agent |
| `agentgate.agent.id` | `agt_01J8Z9X2QK` | registry |
| `agentgate.agent.identity` | `agent://fsclient/payments-risk/dispute-triage` | token |
| `agentgate.tenant.id` | `fsclient` | token |
| `agentgate.team.id` | `payments-risk` | token |
| `agentgate.owner.email` | `payments-risk@client.example` | registry |
| `agentgate.cost_center` | `CC-4471` | token |
| `agentgate.runtime` | `aks` \| `eks` \| `aca` \| `vm` \| `lambda` | agent |
| `agentgate.framework` | `langgraph` \| `semantic-kernel` \| `custom` | agent |

The gateway **stamps or corrects** the ownership attributes from the verified token — an agent
cannot lie about who pays. Where an agent's own attributes disagree with the token, the token wins
and `agentgate.attribution.corrected=true` is set so the mismatch is visible and fixable.

### 4.2 Span taxonomy

| Span | Kind | Emitted by |
|---|---|---|
| `agent.invoke` | SERVER/INTERNAL | agent SDK — one agent run |
| `agent.step` | INTERNAL | agent SDK — one reasoning/plan step |
| `agent.tool` | CLIENT | agent SDK — one tool call |
| `gateway.request` | SERVER | gateway — root of the gateway's work |
| `gateway.policy.<stage>` | INTERNAL | gateway — one policy stage |
| `gateway.guardrail` | CLIENT | gateway — guardrail callout |
| `gateway.cache` | INTERNAL | gateway |
| `gen_ai.chat` | CLIENT | gateway — one backend attempt |
| `gen_ai.embeddings` | CLIENT | gateway |
| `controlplane.register` / `.promote` / `.token_exchange` | SERVER | control plane |

Key span attributes on `gen_ai.chat`:

```
gen_ai.system                = azure.ai.openai | aws.bedrock | vllm
gen_ai.operation.name        = chat
gen_ai.request.model         = gpt-4o-mini          (concrete)
gen_ai.response.model        = gpt-4o-mini-2024-07-18
gen_ai.request.max_tokens / .temperature / .top_p
gen_ai.usage.input_tokens / .output_tokens
gen_ai.response.finish_reasons = ["stop"]
agentgate.logical_model      = general-chat
agentgate.pool               = general-chat
agentgate.backend            = azure-openai/gpt-4o-mini
agentgate.attempt            = 2
agentgate.failover.from      = bedrock/claude-haiku
agentgate.cost.usd           = 0.001842
agentgate.cache              = miss
agentgate.ttft_ms            = 214
agentgate.stream             = true
```

Prompt and completion content is **not** on spans by default. When
`AGENTGATE_CAPTURE_CONTENT=redacted|full` is enabled it is emitted as span *events*
(`gen_ai.content.prompt` / `gen_ai.content.completion`) on a separate pipeline with its own
retention and access control, redacted by the guardrail engine first. Financial-services default
is `off` in prod, `redacted` in non-prod.

### 4.3 Metrics

| Metric | Type | Labels |
|---|---|---|
| `agentgate.gateway.requests` | counter | tenant, team, agent, env, pool, backend, status, code |
| `agentgate.gateway.duration` | histogram | + stream |
| `agentgate.gateway.ttft` | histogram | pool, backend |
| `agentgate.gateway.tokens` | counter | + direction(input/output) |
| `agentgate.gateway.cost_usd` | counter | tenant, team, agent, env, cost_center, backend |
| `agentgate.gateway.inflight` | up-down counter | pool |
| `agentgate.ratelimit.decisions` | counter | agent, decision(allow/limit/quota) |
| `agentgate.cache.lookups` | counter | pool, result |
| `agentgate.guardrail.decisions` | counter | pool, category, action |
| `agentgate.breaker.state` | gauge | backend, state |
| `agentgate.retry.attempts` | counter | backend, reason |
| `agentgate.fleet.agents` | gauge | env, state |
| `agentgate.telemetry.completeness` | gauge | agent, env — the trust metric for the plane itself |

### 4.4 Pipelines

```
agent SDK ─OTLP/gRPC─┐
gateway    ─OTLP/gRPC─┼─► OTel Collector (agent, per node)
controlplane ─OTLP────┘        │ batch, memory_limiter, k8sattributes
                               ▼
                    OTel Collector (gateway, HA pool)
                    ├─ processors: attributes(stamp), redaction, tail_sampling, routing
                    ├─► traces  → Langfuse / self-hosted OTLP store  (+ Azure Monitor / X-Ray)
                    ├─► metrics → Prometheus / Managed Prometheus
                    ├─► logs    → Loki / Log Analytics
                    └─► cost    → chargeback exporter (Parquet → warehouse)
```

Tail sampling: keep 100% of errors, 100% of guardrail blocks, 100% of failovers, 100% of requests
> p99 latency, and 5% baseline. Content pipeline is a separate, non-sampled, access-controlled path.

### 4.5 Telemetry trust ("is it complete and correctly attributed?")

Every 60s the fleet service computes, per agent per env:

- `traces_expected` (gateway request count) vs `traces_received` (spans arriving with that agent's
  resource attributes) → **completeness ratio**.
- `orphan_span_ratio` — spans whose parent never arrived.
- `unattributed_ratio` — spans missing owner/cost-centre attributes.
- `clock_skew_p99` — agent-reported vs gateway-observed timestamps.

These are exported as metrics, drive the `telemetry_degraded` alert, and are a **hard input to the
promotion gate**: an agent whose telemetry is not trustworthy cannot be promoted to production.

---

## 5. SLIs / SLOs

| Service | SLI | Objective (28d) | Error budget |
|---|---|---|---|
| Gateway availability | non-5xx, non-`no_healthy_backend` / total | 99.9% | 40m19s |
| Gateway latency (unary) | p95 gateway overhead (excl. provider time) < 60ms | 99% of minutes | — |
| Gateway TTFT (stream) | p95 time-to-first-token < 1200ms | 99% | — |
| Control plane | token exchange success, p95 < 150ms | 99.95% | 20m |
| Telemetry completeness | per-agent completeness ≥ 0.98 | 99% of agents | — |
| Cost anomaly detection | anomaly surfaced before the consuming team reports it | 95% | — |

**Alerting** is multi-window multi-burn-rate (2%/1h + 5%/6h fast pages, 10%/3d ticket) on the
availability and latency SLOs; threshold alerts on breaker-open, telemetry completeness, and
cost anomaly (EWMA + 3σ per agent per hour, plus a hard daily-spend ceiling per cost centre).
Every alert has a runbook and every runbook is linked from the alert annotation.

---

## 6. Chargeback / showback

Each completed request emits an immutable `UsageRecord`:

```json
{ "ts":"2026-08-26T14:22:01Z", "request_id":"...", "trace_id":"...",
  "tenant":"fsclient","team":"payments-risk","agent_id":"agt_01J8Z9X2QK","env":"prod",
  "cost_center":"CC-4471","logical_model":"general-chat",
  "provider":"azure-openai","backend_model":"gpt-4o-mini",
  "input_tokens":1842,"output_tokens":311,"cached_tokens":0,
  "unit_cost_input_per_1m":0.15,"unit_cost_output_per_1m":0.60,
  "cost_usd":0.000462,"cache":"miss","attempts":1,"billable":true }
```

Written to the usage stream (Kafka/Event Hubs in production, append-only file locally), rolled up
hourly and daily by cost centre, exposed at `/api/v1/chargeback` and as a monthly export. Cached
hits are recorded with `billable:false` and a `savings_usd` figure so the platform can show what it
saved, which is how a shared platform justifies itself.

---

## 7. Migration of the existing service (contract-preserving)

The client already runs a first-generation gateway that consumers integrate against. The migration
is a strangler with the contract held fixed:

1. **Freeze & document** the existing contract; publish it as `gateway.v1.yaml`. AgentGate v1
   *is* that contract.
2. **Compatibility suite** — golden request/response corpus captured from production traffic,
   replayed against both implementations; byte-level diff on body, header and error mapping.
3. **Shadow** — AgentGate receives mirrored traffic, responses discarded, diffs reported. No
   consumer impact.
4. **Canary** — 1% → 5% → 25% → 50% → 100% by consumer, weighted at the DNS/front-door layer, with
   automatic rollback on SLO burn.
5. **Cutover per consumer**, never per endpoint, so a single consumer never sees two behaviours.
6. **Decommission** the legacy service after 30 days at 100% with zero contract diffs.

Rollback at every stage is a weight change, not a deploy.

---

## 8. Network, DNS, TLS, security review

| Path | Mechanism | Review |
|---|---|---|
| Agent → Gateway | Internal load balancer, private DNS zone `*.agentgate.internal`, mTLS optional, TLS 1.3 | Standard internal |
| Gateway → cloud model provider | Private Endpoint / PrivateLink, no public egress, provider-managed identity | Firewall + data-flow review |
| Gateway → on-prem inference | ExpressRoute/Direct Connect, private DNS forwarder, internal CA cert pinning | Network + crypto review |
| Gateway → third-party model provider | Egress via inspecting proxy, FQDN allowlist, mTLS to proxy, per-provider outbound IP | Third-party risk + DLP review |
| Gateway/CP → Vault | Private Endpoint, workload identity, no static creds | Secrets review |
| Collector → observability backend | Private Endpoint where available; otherwise egress proxy + FQDN allowlist | Data-residency review |
| Everything | Certificates from enterprise PKI, ACME-automated, 90-day rotation, expiry alert at 21d | Crypto standard |

Each path ships with the diagram, the exact FQDNs/ports, the data classification crossing it, and
the review artefact it needs — because in this environment the security review, not the code, is
the critical path.

---

## 9. Repository layout

```
cmd/          gateway, controlplane, fleetview, guardrails, mockprovider, agentctl, loadgen
internal/     config, gateway(+policy), provider, resilience, ratelimit, cache, guardrails,
              identity, registry, telemetry, cost, fleet, store, httpx
api/openapi/  gateway.v1.yaml (FROZEN), controlplane.v1.yaml
deploy/       docker, compose, k8s, terraform/{azure,aws}, otel, prometheus, grafana
docs/         architecture, flows, sequences, ADRs, runbooks, migration, security
test/         integration, load
examples/     Go SDK usage, agent registration manifest
```

## 10. Cloud mapping (core is cloud-neutral)

| Concern | Azure | AWS |
|---|---|---|
| Edge API management | API Management (+ AI gateway policies) | API Gateway / ALB |
| Compute | AKS / Container Apps | EKS / ECS Fargate |
| Workload identity | Entra ID workload identity federation, managed identity | IRSA / IAM Roles Anywhere |
| Secrets | Key Vault | Secrets Manager |
| Models | Azure OpenAI / AI Foundry | Bedrock |
| Private connectivity | Private Endpoint + Private DNS zones | PrivateLink + Route53 private zones |
| Observability | Azure Monitor / App Insights (+ Langfuse) | CloudWatch / X-Ray (+ Langfuse) |
| Usage stream | Event Hubs | Kinesis / MSK |
| State | Azure Database for PostgreSQL, Azure Cache for Redis | RDS PostgreSQL, ElastiCache |
