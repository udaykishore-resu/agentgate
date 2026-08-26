# 04 — Gateway v1 Wire Contract

**Status: FROZEN.** This document is the published interface for consuming teams. The normative
machine-readable source is `api/openapi/gateway.v1.yaml`. Where this document and the OpenAPI
document disagree, the OpenAPI document wins and this document is a defect.

**Base URL:** `https://gateway.agentgate.internal/v1`

The gateway is wire-compatible with the OpenAI Chat Completions shape. Existing SDKs and agent
frameworks work unmodified by pointing their base URL at the gateway and supplying an AgentGate
access token as the bearer credential.

---

## 1. What will never change, and what may

This is the compatibility guarantee. Read it before you write anything that depends on the gateway.

### 1.1 Frozen — will not change within `/v1`

| Guarantee | Detail |
|---|---|
| Endpoint paths and methods | The seven entries in §2 |
| Request field names, types and meanings | An accepted field keeps its name, type and semantics |
| Response field names and types | No field is ever removed or retyped |
| The set of error `code` values | No `code` is ever repurposed to mean something else |
| HTTP status for an existing `code` | The status paired with a `code` never changes |
| Error body format | RFC 9457 `application/problem+json` with `code`, `request_id`, `trace_id` |
| Streaming termination | `data: [DONE]` terminates every successful stream, and is always last |
| `agentgate.usage` ordering | When emitted, always before `[DONE]` |
| Header names | Every `x-agentgate-*` header in §5 keeps its name and meaning |
| Authentication scheme | `Authorization: Bearer <access token>` |
| Logical model addressing | `model` is always a logical model, never a provider deployment name |

### 1.2 May change — design for it

| May change | What you must tolerate |
|---|---|
| New fields added to response bodies | Ignore unknown fields. Do not use strict deserialisation that rejects them |
| New `x-agentgate-*` response headers | Ignore unknown headers |
| New SSE `event:` types | Ignore unknown event types. A strict OpenAI client already does |
| New error `code` values | Handle an unknown `code` by falling back to the HTTP status class |
| New optional request headers and fields | Nothing required of you |
| New logical models appearing in `GET /v1/models` | Nothing required of you |
| Which provider or backend serves a request | Never depend on `x-agentgate-provider` or `x-agentgate-model` for correctness. They are observability, not contract |
| Latency characteristics | Governed by the SLOs in `07-slo-alerting.md`, not by this contract |
| Number of attempts made | `x-agentgate-attempts` is informational |

### 1.3 Breaking change policy

A breaking change means `/v2`, published as a separate base path, run side-by-side with `/v1`, with
a published deprecation window. `/v1` is not modified in place to accommodate `/v2`.

### 1.4 Client rules that follow from the above

1. Use non-strict JSON deserialisation, or explicitly ignore unknown fields.
2. Branch on `code`, not on `detail` or `title`. `detail` is human-readable and its wording may
   change.
3. Treat an unknown `code` as its HTTP status class.
4. Never parse `detail` to extract values. Everything machine-readable is a field.
5. Log `x-agentgate-request-id` and `x-agentgate-trace-id` on every response, success or failure.
   These are what a support conversation starts from.

---

## 2. Endpoints

| Method | Path | Purpose | Auth |
|---|---|---|---|
| POST | `/v1/chat/completions` | Chat completion, streaming SSE or unary | Bearer |
| POST | `/v1/embeddings` | Embeddings | Bearer |
| GET | `/v1/models` | Logical models the caller is entitled to | Bearer |
| GET | `/v1/models/{id}` | One logical model | Bearer |
| POST | `/v1/token-count` | Pre-flight token estimate against a logical model | Bearer |
| GET | `/healthz` | Liveness | None |
| GET | `/readyz` | Readiness | None |
| GET | `/metrics` | Prometheus scrape | Network-restricted |

`/healthz` and `/readyz` are unauthenticated by design so that load balancers and orchestrators can
use them. They return no tenant-identifying information. `/metrics` is network-restricted to the
platform's scrape range; it is not exposed to agent networks.

---

## 3. Authentication

Every `/v1/*` call requires:

```
Authorization: Bearer <AgentGate access token>
```

The token is obtained from the control plane, either through RFC 8693 token exchange of a platform
workload identity token, or through client credentials. See `05-identity.md`. The token carries the
agent's coordinates as claims; the gateway derives tenant, team, agent, version, environment,
cost centre, entitled pools and scopes from it. Nothing about identity is taken from the request
body or from headers.

Required scope by endpoint:

| Endpoint | Required scope |
|---|---|
| `/v1/chat/completions` | `models:invoke` |
| `/v1/embeddings` | `models:embed` |
| `/v1/models`, `/v1/models/{id}` | `models:invoke` or `models:embed` |
| `/v1/token-count` | `models:invoke` or `models:embed` |

---

## 4. Requests

### 4.1 `POST /v1/chat/completions`

```jsonc
{
  "model": "general-chat",
  "messages": [
    {"role": "system", "content": "You classify disputes."},
    {"role": "user", "content": "Cardholder reports a duplicate charge on 2026-08-14."}
  ],
  "max_tokens": 1024,
  "temperature": 0.2,
  "top_p": 1.0,
  "stream": true,
  "stream_options": {"include_usage": true},
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "lookup_transaction",
        "description": "Look up a transaction by id",
        "parameters": {
          "type": "object",
          "properties": {"transaction_id": {"type": "string"}},
          "required": ["transaction_id"]
        }
      }
    }
  ],
  "tool_choice": "auto",
  "response_format": {"type": "json_object"},
  "metadata": {
    "session_id": "sess_123",
    "conversation_id": "conv_9",
    "step": "classify",
    "tags": ["dispute", "tier2"]
  }
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `model` | string | yes | **Logical** model. Never a provider deployment name. `GET /v1/models` enumerates what you may use |
| `messages` | array | yes | OpenAI message shape. `role` one of `system`, `user`, `assistant`, `tool` |
| `max_tokens` | integer | no | Upper bound on generated tokens. Also the amount reserved from your token quota — see §9 |
| `temperature` | number | no | Values above 0.2 bypass the exact cache unless pool policy allows otherwise |
| `top_p` | number | no | Part of the cache key |
| `stream` | boolean | no | Default false |
| `stream_options.include_usage` | boolean | no | When true, an `agentgate.usage` event is emitted before `[DONE]` |
| `tools` | array | no | Tool-calling requests bypass the cache by default |
| `tool_choice` | string or object | no | `auto`, `none`, `required`, or a named function |
| `response_format` | object | no | `{"type":"json_object"}` or `{"type":"text"}`. Part of the cache key |
| `metadata` | object | no | Joins application context to the trace. See §4.3 |

### 4.2 `POST /v1/embeddings`

```jsonc
{
  "model": "embed-standard",
  "input": ["dispute narrative one", "dispute narrative two"],
  "encoding_format": "float"
}
```

`input` accepts a string or an array of strings. Token quota is reserved on the estimated input size;
there is no `max_tokens` component because embeddings generate no output tokens.

### 4.3 `metadata`

`metadata` is not sent to the model provider. It is attached to the trace so application context is
queryable alongside model telemetry.

| Field | Type | Cardinality guidance |
|---|---|---|
| `session_id` | string | High cardinality, safe — it is a span attribute, never a metric label |
| `conversation_id` | string | High cardinality, safe |
| `step` | string | **Keep low cardinality.** Use a fixed vocabulary such as `classify`, `retrieve`, `summarise`. Do not put ids here |
| `tags` | array of string | Keep to a bounded vocabulary |

Do not put personal data, account identifiers, or free text in `metadata`. It is telemetry, and
telemetry retention is not the same as content retention.

### 4.4 `POST /v1/token-count`

```jsonc
{
  "model": "general-chat",
  "messages": [{"role": "user", "content": "..."}]
}
```

Returns an estimate for the given logical model so a caller can check context-window fit before
spending a request. It does not reserve quota and does not contact a provider.

```json
{
  "model": "general-chat",
  "input_tokens": 1842,
  "context_window": 128000,
  "fits": true
}
```

### 4.5 Optional request headers

All are `x-agentgate-*` and all are safe to omit.

| Header | Values | Meaning |
|---|---|---|
| `x-agentgate-session-id` | string | Conversation or session correlation. Appears on every span for the request |
| `x-agentgate-request-priority` | `interactive` \| `batch` | Selects pool tier and queue discipline. `batch` is shed first under load shedding |
| `x-agentgate-cache` | `on` \| `off` \| `refresh` | Overrides the pool's default cache behaviour. `refresh` forces a provider call and overwrites the entry |
| `x-agentgate-pool` | string | Explicit pool override. **Rejected with 403 `forbidden_pool` if the pool is not in the token's `model_pools`** |
| `x-agentgate-idempotency-key` | string | De-duplicates retried non-stream requests for 24 h. See §8 |
| `traceparent` | W3C format | The gateway continues your trace. Strongly recommended |
| `tracestate` | W3C format | Propagated |

**`x-agentgate-request-priority` is load-bearing.** Marking bulk work as `batch` is what keeps it
from competing with interactive traffic, and it is shed first when the gateway is over its
concurrency ceiling. Marking everything `interactive` degrades the platform for everyone including
you.

---

## 5. Response headers

Present on every response, success or error, unless noted.

| Header | Example | Meaning |
|---|---|---|
| `x-agentgate-request-id` | `a1b2c3d4e5f60718` | Gateway request id. Equal to the root span id in hex |
| `x-agentgate-trace-id` | `4bf92f3577b34da6a3ce929d0e0e4736` | W3C trace id for cross-referencing in the observability backend |
| `x-agentgate-provider` | `azure-openai` | Provider that served it. Also `bedrock`, `onprem-vllm` |
| `x-agentgate-model` | `gpt-4o-mini` | Concrete backend model or deployment used |
| `x-agentgate-pool` | `general-chat` | Pool selected |
| `x-agentgate-attempts` | `2` | Number of backend attempts made |
| `x-agentgate-cache` | `hit` \| `miss` \| `bypass` \| `refresh` | Cache outcome |
| `x-agentgate-tokens-input` | `1842` | Billed input tokens |
| `x-agentgate-tokens-output` | `311` | Billed output tokens |
| `x-agentgate-cost-usd` | `0.000462` | Attributed cost for this call, 6 decimal places |
| `x-agentgate-ratelimit-limit-tokens` | `120000` | Your token budget for the window |
| `x-agentgate-ratelimit-remaining-tokens` | `78171` | Remaining after settle |
| `x-agentgate-ratelimit-reset` | `17` | Seconds until the token bucket refills to full |
| `x-agentgate-guardrail` | `pass` \| `blocked:<category>` \| `redacted:<category>` | Guardrail outcome |
| `Retry-After` | `17` | Present on 429 and 503 |

On a streaming response these headers are flushed with the 200 status **before** any content. Token,
cost and guardrail headers on a stream therefore reflect the state at header-flush time; the
authoritative final figures arrive in the `agentgate.usage` event.

---

## 6. Streaming semantics

Streaming is Server-Sent Events. `Content-Type: text/event-stream`.

### 6.1 Frame types

| Frame | Form | Meaning |
|---|---|---|
| Content chunk | `data: {OpenAI chunk object}` | Standard OpenAI streaming chunk |
| Heartbeat | `: ` comment line, every 15 s of idle | Keeps intermediary proxies from dropping long generations. An SSE comment, not an event. Ignored by every conforming client |
| Usage | `event: agentgate.usage` followed by `data: {...}` | Final usage and cost. Emitted only when `stream_options.include_usage` is true. Always immediately before `[DONE]` |
| Failover | `event: agentgate.failover` followed by `data: {...}` | The stream was restarted on another backend **before any content token was delivered** |
| Error | `event: error` followed by `data: {problem+json}` | Terminal in-band failure. No `[DONE]` follows |
| Termination | `data: [DONE]` | End of a successful stream. Always last |

### 6.2 Example stream

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
x-agentgate-request-id: a1b2c3d4e5f60718
x-agentgate-trace-id: 4bf92f3577b34da6a3ce929d0e0e4736
x-agentgate-provider: azure-openai
x-agentgate-model: gpt-4o-mini
x-agentgate-pool: general-chat

data: {"id":"chatcmpl-9x","object":"chat.completion.chunk","created":1787000000,"model":"general-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"Duplicate"},"finish_reason":null}]}

data: {"id":"chatcmpl-9x","object":"chat.completion.chunk","created":1787000000,"model":"general-chat","choices":[{"index":0,"delta":{"content":" charge"},"finish_reason":null}]}

: 

data: {"id":"chatcmpl-9x","object":"chat.completion.chunk","created":1787000000,"model":"general-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

event: agentgate.usage
data: {"request_id":"a1b2c3d4e5f60718","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","usage":{"prompt_tokens":1842,"completion_tokens":311,"total_tokens":2153},"cost_usd":0.000462,"provider":"azure-openai","model":"gpt-4o-mini","cache":"miss","attempts":1}

data: [DONE]
```

### 6.3 Failover event

```
event: agentgate.failover
data: {"request_id":"a1b2c3d4e5f60718","from":"azure-openai/gpt-4o-mini","to":"bedrock/claude-haiku","reason":"provider_error","attempt":3}
```

Emitted only before the first content byte. This is a hard rule.

### 6.4 The first-content-byte rule

| Situation | Behaviour |
|---|---|
| Backend fails **before** the first content byte reaches the caller | The gateway may retry and may fail over to another backend. An `agentgate.failover` event is emitted. The caller receives one complete, coherent generation and never knows it was restarted, except through the event and `x-agentgate-attempts` |
| Backend fails **after** the first content byte reaches the caller | The stream is **never** silently restarted. It terminates with an `event: error` frame and **no** `[DONE]`. The caller holds a partial generation |

The reason is correctness: splicing two generations together produces a response that no model
actually produced. The caller is the only party that knows whether a partial answer is usable, so
the decision to retry is left to the caller.

### 6.5 Client requirements for streams

1. Treat **absence of `[DONE]`** as failure, even if you received content. Do not rely on connection
   close alone.
2. Handle `event: error` and read `code` from its payload.
3. Ignore unknown `event:` types.
4. Ignore comment lines beginning with `:`.
5. Set your client read timeout above 15 s so the heartbeat interval does not trip it. **A read
   timeout below 15 s will spuriously fail long generations.**
6. If your HTTP client buffers responses, disable buffering for this endpoint. Buffered SSE defeats
   the purpose of streaming and can hide the heartbeat.

---

## 7. Errors

All errors are RFC 9457 `application/problem+json`.

```json
{
  "type": "https://agentgate.internal/errors/quota_exceeded",
  "title": "Token quota exceeded",
  "status": 429,
  "detail": "agent://fsclient/payments-risk/dispute-triage exceeded 120000 tokens/min",
  "code": "quota_exceeded",
  "request_id": "a1b2c3d4e5f60718",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "retry_after_seconds": 17
}
```

| Field | Always present | Notes |
|---|---|---|
| `type` | yes | `https://agentgate.internal/errors/<code>` |
| `title` | yes | Short human-readable summary. Wording may change |
| `status` | yes | Matches the HTTP status |
| `detail` | yes | Human-readable. **Do not parse** |
| `code` | yes | **Branch on this** |
| `request_id` | yes | Matches `x-agentgate-request-id` |
| `trace_id` | yes | Matches `x-agentgate-trace-id` |
| `retry_after_seconds` | on 429 and 503 | Mirrors `Retry-After` |

### 7.1 Error catalogue

| HTTP | `code` | Cause | Retryable | What to do |
|---|---|---|---|---|
| 400 | `invalid_request` | Malformed body or unsupported parameter | No | Fix the request. `detail` names the field |
| 401 | `unauthenticated` | Missing, expired, or unverifiable token | After refresh | Obtain a fresh token and retry once |
| 403 | `forbidden_pool` | Pool not in the token's entitlement | No | Request entitlement, or drop `x-agentgate-pool` |
| 403 | `agent_not_promoted` | Version not promoted for this environment | No | Promote the version. See `05-identity.md` |
| 403 | `guardrail_blocked` | Content-safety policy denied the request or response | No | Do not retry. The same content will be blocked again |
| 404 | `unknown_model` | Logical model does not exist for this tenant | No | Call `GET /v1/models` |
| 408 | `client_timeout` | Caller deadline exceeded | Yes, with a longer deadline | Increase the deadline or reduce `max_tokens` |
| 409 | `idempotency_conflict` | Same idempotency key, different body | No | Use a new key, or send the identical body |
| 413 | `context_too_large` | Prompt exceeds the logical model's window | No | Reduce the prompt, or use a long-context logical model |
| 429 | `rate_limited` | Requests-per-minute limit | Yes | Honour `Retry-After`. Back off; do not tight-loop |
| 429 | `quota_exceeded` | Token-per-minute or monthly budget | Yes | Honour `Retry-After`. A monthly breach needs a budget conversation, not a retry |
| 499 | `client_closed_request` | Caller disconnected mid-stream | n/a | Recorded for attribution. You will not see this response; you are gone |
| 502 | `provider_error` | Upstream returned an unrecoverable error | Yes, sparingly | The gateway already retried and failed over. Back off |
| 503 | `no_healthy_backend` | Every backend in the pool is open-circuit or drained | Yes | Honour `Retry-After`. This is a platform-side condition |
| 504 | `provider_timeout` | Upstream deadline exceeded after retries | Yes, sparingly | Consider a smaller `max_tokens` or a different pool |

### 7.2 Worked examples

**401 `unauthenticated`**

```json
{
  "type": "https://agentgate.internal/errors/unauthenticated",
  "title": "Token could not be verified",
  "status": 401,
  "detail": "token expired at 2026-08-26T14:19:31Z",
  "code": "unauthenticated",
  "request_id": "9f3a1c88b2004411",
  "trace_id": "1a2b3c4d5e6f70819a0b1c2d3e4f5061"
}
```

**403 `agent_not_promoted`**

```json
{
  "type": "https://agentgate.internal/errors/agent_not_promoted",
  "title": "Agent version is not promoted for this environment",
  "status": 403,
  "detail": "agent://fsclient/payments-risk/dispute-triage@2.5.0 is not active in env=prod; active version is 2.4.1",
  "code": "agent_not_promoted",
  "request_id": "b7c8d9e0f1a21323",
  "trace_id": "2b3c4d5e6f708192a3b4c5d6e7f80912"
}
```

**403 `guardrail_blocked`**

```json
{
  "type": "https://agentgate.internal/errors/guardrail_blocked",
  "title": "Content safety policy denied the request",
  "status": 403,
  "detail": "input blocked by category pii.account_number",
  "code": "guardrail_blocked",
  "request_id": "c1d2e3f405162738",
  "trace_id": "3c4d5e6f708192a3b4c5d6e7f8091a2b"
}
```

Response header: `x-agentgate-guardrail: blocked:pii.account_number`. The matched content is never
returned.

**413 `context_too_large`**

```json
{
  "type": "https://agentgate.internal/errors/context_too_large",
  "title": "Prompt exceeds the logical model context window",
  "status": 413,
  "detail": "estimated 141203 tokens exceeds general-chat window of 128000; consider logical model long-context",
  "code": "context_too_large",
  "request_id": "d4e5f60718293a4b",
  "trace_id": "4d5e6f708192a3b4c5d6e7f8091a2b3c"
}
```

**429 `rate_limited`**

```json
{
  "type": "https://agentgate.internal/errors/rate_limited",
  "title": "Request rate limit exceeded",
  "status": 429,
  "detail": "agent://fsclient/payments-risk/dispute-triage exceeded 600 requests/min",
  "code": "rate_limited",
  "request_id": "e5f60718293a4b5c",
  "trace_id": "5e6f708192a3b4c5d6e7f8091a2b3c4d",
  "retry_after_seconds": 6
}
```

**503 `no_healthy_backend`**

```json
{
  "type": "https://agentgate.internal/errors/no_healthy_backend",
  "title": "No healthy backend available",
  "status": 503,
  "detail": "all backends in pool general-chat are open-circuit or drained",
  "code": "no_healthy_backend",
  "request_id": "f60718293a4b5c6d",
  "trace_id": "6f708192a3b4c5d6e7f8091a2b3c4d5e",
  "retry_after_seconds": 30
}
```

**In-stream error frame**

```
event: error
data: {"type":"https://agentgate.internal/errors/provider_error","title":"Upstream error after content delivery","status":502,"detail":"connection reset by upstream after 214 tokens","code":"provider_error","request_id":"a1b2c3d4e5f60718","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}
```

Note `status` in the payload is 502 even though the HTTP status was 200. The HTTP status was fixed
when headers flushed; the payload `status` is the semantic outcome.

---

## 8. Idempotency

`x-agentgate-idempotency-key` de-duplicates retried **non-streaming** requests for 24 hours.

| Rule | Behaviour |
|---|---|
| Same key, identical body, within 24 h | The original response is replayed, including status, body and `x-agentgate-request-id`. The provider is not called again |
| Same key, different body | 409 `idempotency_conflict` |
| Key on a streaming request | Ignored. Streams are not replayable |
| Key omitted | No de-duplication. A network-level retry may produce a second generation and a second charge |
| Key on a request that failed before reaching a provider | The failure is not cached. Retrying with the same key is a fresh attempt |

Keys are scoped per agent identity. Two agents using the same key value do not collide.

**[Decision]** Only terminal responses that reached a provider are stored under the key. Caching
transient platform failures under an idempotency key would make a retryable condition permanently
sticky for 24 hours, which is the opposite of what the caller wants.

Use a UUID v4 or a deterministic hash of the logical work unit. Do not reuse keys across semantically
different requests.

---

## 9. Quota, rate limits and how `max_tokens` affects you

The gateway enforces two limits per agent, keyed `tenant:team:agent:env`:

| Limit | Enforced at stage | Error on breach |
|---|---|---|
| Requests per minute | 5 `ratelimit.requests` | 429 `rate_limited` |
| Tokens per minute, and monthly token budget | 6 `quota.tokens` | 429 `quota_exceeded` |

Token quota is **reserved before the request runs** and settled afterwards:

1. The gateway estimates your input tokens and adds your `max_tokens`.
2. That total is reserved from your bucket. If the reservation fails you get `quota_exceeded`.
3. When the request completes, the difference between reserved and actual is released.

**Practical consequence:** a large `max_tokens` reserves a large amount of quota even if the model
generates a short answer. Setting `max_tokens: 8192` for responses that are typically 200 tokens
reduces your effective request throughput by a factor of roughly forty. Set `max_tokens` to a
realistic ceiling for your use case.

**[Decision]** If `max_tokens` is omitted, the logical model's configured default is used for the
reservation. `GET /v1/models/{id}` reports that default so you can plan against it.

`x-agentgate-ratelimit-remaining-tokens` reflects post-settle state and is the correct signal to
pace against.

---

## 10. Model discovery

`GET /v1/models` returns the logical models this token is entitled to.

```json
{
  "object": "list",
  "data": [
    {
      "id": "general-chat",
      "object": "model",
      "owned_by": "agentgate",
      "agentgate": {
        "pool": "general-chat",
        "tier": "interactive",
        "context_window": 128000,
        "default_max_tokens": 1024,
        "supports": ["chat", "tools", "json_mode", "streaming"],
        "data_classification_max": "confidential",
        "cache": {"exact": true, "semantic": false}
      }
    },
    {
      "id": "long-context",
      "object": "model",
      "owned_by": "agentgate",
      "agentgate": {
        "pool": "long-context",
        "tier": "batch",
        "context_window": 1000000,
        "default_max_tokens": 4096,
        "supports": ["chat", "streaming"],
        "data_classification_max": "confidential",
        "cache": {"exact": true, "semantic": false}
      }
    }
  ]
}
```

The `agentgate` sub-object is an **added** field, permitted by the compatibility rule. A strict
OpenAI client ignores it.

Concrete backend models are deliberately absent. Which hardware serves `general-chat` is a platform
decision that changes without notice, and depending on it would defeat the indirection.

---

## 11. Examples

### 11.1 curl — unary

```bash
curl -sS https://gateway.agentgate.internal/v1/chat/completions \
  -H "Authorization: Bearer ${AGENTGATE_TOKEN}" \
  -H "Content-Type: application/json" \
  -H "x-agentgate-session-id: sess_123" \
  -H "x-agentgate-request-priority: interactive" \
  -H "x-agentgate-idempotency-key: 7c9e6679-7425-40de-944b-e07fc1f90ae7" \
  -D /tmp/agentgate-headers.txt \
  -d '{
        "model": "general-chat",
        "messages": [{"role":"user","content":"Classify this dispute."}],
        "max_tokens": 256,
        "temperature": 0.0
      }'

# Always keep the correlation identifiers.
grep -i '^x-agentgate-\(request-id\|trace-id\|cost-usd\)' /tmp/agentgate-headers.txt
```

### 11.2 curl — streaming

```bash
curl -N -sS https://gateway.agentgate.internal/v1/chat/completions \
  -H "Authorization: Bearer ${AGENTGATE_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
        "model": "general-chat",
        "messages": [{"role":"user","content":"Summarise the dispute narrative."}],
        "max_tokens": 512,
        "stream": true,
        "stream_options": {"include_usage": true}
      }'
```

`-N` disables curl's buffering. Without it you will see the whole stream arrive at once and the
heartbeat will be invisible.

### 11.3 curl — token count and models

```bash
curl -sS https://gateway.agentgate.internal/v1/models \
  -H "Authorization: Bearer ${AGENTGATE_TOKEN}"

curl -sS https://gateway.agentgate.internal/v1/token-count \
  -H "Authorization: Bearer ${AGENTGATE_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"model":"general-chat","messages":[{"role":"user","content":"..."}]}'
```

### 11.4 Python — the `openai` SDK pointed at the gateway

```python
import os
import httpx
from openai import OpenAI

# The gateway is wire-compatible. No AgentGate SDK is required.
client = OpenAI(
    base_url="https://gateway.agentgate.internal/v1",
    api_key=os.environ["AGENTGATE_TOKEN"],   # sent as Authorization: Bearer
    timeout=httpx.Timeout(connect=5.0, read=60.0, write=10.0, pool=5.0),
    max_retries=0,   # let the gateway own retries; client-side retries stack badly
    default_headers={
        "x-agentgate-session-id": "sess_123",
        "x-agentgate-request-priority": "interactive",
    },
)

# Unary, with the response headers captured.
resp = client.chat.completions.with_raw_response.create(
    model="general-chat",
    messages=[{"role": "user", "content": "Classify this dispute."}],
    max_tokens=256,
    temperature=0.0,
    extra_headers={"x-agentgate-idempotency-key": "7c9e6679-7425-40de-944b-e07fc1f90ae7"},
    extra_body={"metadata": {"session_id": "sess_123", "step": "classify",
                             "tags": ["dispute", "tier2"]}},
)

print(resp.headers["x-agentgate-request-id"],
      resp.headers["x-agentgate-trace-id"],
      resp.headers["x-agentgate-provider"],
      resp.headers["x-agentgate-cost-usd"])

completion = resp.parse()
print(completion.choices[0].message.content)
```

**Read timeout must exceed 15 seconds** or the heartbeat interval will not save you on a long
generation. `max_retries=0` is deliberate: the gateway already retries, fails over and enforces a
fleet-wide retry budget. Client-side retries on top of that multiply load during exactly the
incidents where load is the problem.

Streaming:

```python
stream = client.chat.completions.create(
    model="general-chat",
    messages=[{"role": "user", "content": "Summarise the dispute narrative."}],
    max_tokens=512,
    stream=True,
    stream_options={"include_usage": True},
)

saw_terminator = False
for chunk in stream:
    # The SDK surfaces standard chunks. AgentGate's custom `event:` frames
    # are not surfaced by the OpenAI SDK; use the raw client if you need them.
    if chunk.usage is not None:
        print("usage", chunk.usage)
    for choice in chunk.choices:
        if choice.delta.content:
            print(choice.delta.content, end="")
    saw_terminator = True

if not saw_terminator:
    raise RuntimeError("stream ended without content or terminator")
```

Handling errors by `code`, not by message:

```python
from openai import APIStatusError

RETRYABLE = {"rate_limited", "quota_exceeded", "no_healthy_backend",
             "provider_error", "provider_timeout", "client_timeout"}

try:
    resp = client.chat.completions.create(model="general-chat", messages=msgs, max_tokens=256)
except APIStatusError as e:
    body = e.response.json()
    code = body.get("code")
    request_id = body.get("request_id")
    if code in RETRYABLE:
        wait = body.get("retry_after_seconds", 1)
        # back off for `wait` seconds; do not tight-loop
    elif code == "guardrail_blocked":
        # never retry: identical content will be blocked identically
        pass
    else:
        raise
```

If you need the raw AgentGate SSE events (`agentgate.usage`, `agentgate.failover`, `error`), read the
stream directly rather than through the SDK:

```python
import json, httpx

with httpx.stream("POST", "https://gateway.agentgate.internal/v1/chat/completions",
                  headers={"Authorization": f"Bearer {os.environ['AGENTGATE_TOKEN']}"},
                  json={"model": "general-chat", "messages": msgs,
                        "stream": True, "stream_options": {"include_usage": True}},
                  timeout=httpx.Timeout(connect=5.0, read=60.0, write=10.0, pool=5.0)) as r:
    r.raise_for_status()
    event = None
    done = False
    for line in r.iter_lines():
        if not line or line.startswith(":"):      # heartbeat comment
            continue
        if line.startswith("event: "):
            event = line[7:]
            continue
        if line.startswith("data: "):
            payload = line[6:]
            if payload == "[DONE]":
                done = True
                break
            data = json.loads(payload)
            if event == "agentgate.usage":
                print("final usage", data["usage"], data["cost_usd"])
            elif event == "agentgate.failover":
                print("failed over", data["from"], "->", data["to"])
            elif event == "error":
                raise RuntimeError(f"stream error {data['code']} req={data['request_id']}")
            else:
                pass  # standard content chunk
            event = None
    if not done:
        raise RuntimeError("stream ended without [DONE]")
```

### 11.5 Go — plain `net/http`

```go
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const baseURL = "https://gateway.agentgate.internal/v1"

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []message       `json:"messages"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// problemJSON is the RFC 9457 error body. Branch on Code, never on Detail.
type problemJSON struct {
	Type              string `json:"type"`
	Title             string `json:"title"`
	Status            int    `json:"status"`
	Detail            string `json:"detail"`
	Code              string `json:"code"`
	RequestID         string `json:"request_id"`
	TraceID           string `json:"trace_id"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

func (p *problemJSON) Error() string {
	return fmt.Sprintf("agentgate: %s (status=%d request_id=%s trace_id=%s): %s",
		p.Code, p.Status, p.RequestID, p.TraceID, p.Detail)
}

var client = &http.Client{
	// Above the 15s heartbeat interval. A shorter timeout fails long generations.
	Timeout: 120 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 64,
		MaxConnsPerHost:     128,
		IdleConnTimeout:     60 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

func Chat(ctx context.Context, token string, req chatRequest) (map[string]any, http.Header, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-agentgate-request-priority", "interactive")
	// otelhttp or your propagator injects traceparent here so the gateway continues your trace.

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header, err
	}

	if resp.StatusCode >= 400 {
		var p problemJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, resp.Header, fmt.Errorf("agentgate: status %d, unparseable body", resp.StatusCode)
		}
		return nil, resp.Header, &p
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil { // tolerant: unknown fields are expected
		return nil, resp.Header, err
	}
	return out, resp.Header, nil
}

// ChatStream reads the SSE stream and surfaces AgentGate's own event frames.
func ChatStream(ctx context.Context, token string, req chatRequest,
	onContent func(json.RawMessage), onUsage func(json.RawMessage)) error {

	req.Stream = true
	req.StreamOptions = &streamOptions{IncludeUsage: true}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var p problemJSON
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &p)
		return &p
	}

	fmt.Fprintf(os.Stderr, "request_id=%s trace_id=%s provider=%s\n",
		resp.Header.Get("x-agentgate-request-id"),
		resp.Header.Get("x-agentgate-trace-id"),
		resp.Header.Get("x-agentgate-provider"))

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var event string
	sawDone := false

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			continue
		case line[0] == ':': // heartbeat comment
			continue
		case len(line) > 7 && line[:7] == "event: ":
			event = line[7:]
		case len(line) > 6 && line[:6] == "data: ":
			payload := line[6:]
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			switch event {
			case "agentgate.usage":
				onUsage(json.RawMessage(payload))
			case "agentgate.failover":
				// informational: the stream was restarted before any content byte
			case "error":
				var p problemJSON
				_ = json.Unmarshal([]byte(payload), &p)
				return &p
			default:
				onContent(json.RawMessage(payload))
			}
			event = ""
		}
		if sawDone {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// Absence of [DONE] is a failure even if content arrived.
	if !sawDone {
		return fmt.Errorf("agentgate: stream ended without [DONE]")
	}
	return nil
}
```

---

## 12. Client checklist

Before you ship an integration:

- [ ] Base URL points at `https://gateway.agentgate.internal/v1`.
- [ ] Bearer token is obtained from the control plane and refreshed before expiry.
- [ ] `traceparent` is propagated. Without it your agent spans become orphans and your telemetry
      completeness — a promotion-gate input — will not pass.
- [ ] Read timeout is above 15 seconds.
- [ ] Response buffering is disabled for streaming calls.
- [ ] Client-side retries are disabled or strictly bounded; the gateway owns retries.
- [ ] Errors are branched on `code`, never on `detail`.
- [ ] Unknown response fields, headers, and SSE events are ignored, not rejected.
- [ ] `[DONE]` is required for a stream to count as successful.
- [ ] `max_tokens` is a realistic ceiling, not a maximum-possible value.
- [ ] `x-agentgate-request-priority` is `batch` for bulk work.
- [ ] `x-agentgate-request-id` and `x-agentgate-trace-id` are logged on every response.
- [ ] `metadata.step` uses a bounded vocabulary and contains no identifiers or personal data.
