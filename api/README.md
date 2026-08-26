# AgentGate API specifications

This directory holds the machine-readable interface contracts for the three AgentGate services.
They are generated from nothing: they are hand-maintained descriptions of what the Go code actually
does, and where the code and `SPEC.md` disagreed, **the code won and the divergence is recorded at
the bottom of this file**.

| File | Service | Base path | Audience |
|---|---|---|---|
| [`openapi/gateway.v1.yaml`](openapi/gateway.v1.yaml) | `gateway` | `/v1` | **External.** Published to every consuming engineering team. Frozen. |
| [`openapi/controlplane.v1.yaml`](openapi/controlplane.v1.yaml) | `controlplane` | `/oauth2`, `/api/v1` | CI pipelines, platform engineers, approvers. |
| [`openapi/fleet.v1.yaml`](openapi/fleet.v1.yaml) | `fleetview` | `/api/v1`, `/v1/traces` | Platform operators, FinOps, the OTel collector. |

---

## 1. What each specification is

### `gateway.v1.yaml` — the frozen contract

This is the document the platform is judged against. It describes the OpenAI-wire-compatible model
API: chat completions (unary and streaming), embeddings, a pre-flight token estimate, the entitled
model list, the liveness and readiness probes, and the network-restricted operator surface.

It carries the full `x-agentgate-*` header vocabulary in both directions, the complete
`application/problem+json` error contract, and a worked example of the SSE frame sequence including
the `agentgate.usage` and `error` named events, the `:` heartbeat comment, and the `data: [DONE]`
sentinel.

### `controlplane.v1.yaml` — the trust plane

OIDC discovery and JWKS publication, the two token grants, agent registration and inventory,
credential issuance and rotation, quarantine, and the promotion gate with its evidence and its
two-party approval flow.

Note that this specification describes **two different error dialects**. `/oauth2/token` returns
OAuth2-shaped `{"error", "error_description"}` bodies, because that is what an OAuth2 client
library expects. Everything under `/api/v1` returns RFC 9457 problem+json, the same document the
gateway returns.

### `fleet.v1.yaml` — the observability plane

The fleet inventory, per-agent telemetry trust, SLO status with exact error-budget arithmetic,
chargeback rollups, the promotion signals the control plane's gate consumes, and the OTLP/HTTP JSON
receiver that feeds the trust tracker.

---

## 2. How the contract is frozen

The freeze applies to `gateway.v1.yaml` in the strong sense and to the other two by convention. It
means precisely five things:

1. **Fields may be added.** New response fields and new response headers may appear at any time. A
   client must ignore what it does not recognise.
2. **No field is removed and no field is retyped.** A string stays a string. An optional field
   never becomes required.
3. **No error `code` is repurposed** and **no `code` changes its HTTP status.** The mapping in
   `internal/httpx/problem.go` is the single source of truth; the table in §5 is a copy of it.
4. **New error codes may be added.** A client must handle an unrecognised `code` by falling back to
   the HTTP status class it arrived with.
5. **Anything that breaks rules 2 or 3 is a `/v2`** — a separate base path, run side by side with
   `/v1`, with a published deprecation window. It is never shipped as an edit to this document.

The freeze is enforced by the compatibility suite in `test/`: a golden corpus of request and
response pairs replayed against the implementation, diffed at byte level on body, headers and error
mapping. A change that breaks the corpus fails CI. The corpus is the contract's teeth; this
document is its statement.

---

## 3. Proposing and reviewing a change

An interface change is a two-artefact change: the specification and the code move together, in one
pull request, or the specification is fiction within a sprint.

1. **Classify it.** Additive (a new optional field, a new error code, a new endpoint) or breaking
   (anything in §2 rules 2 and 3). If you are unsure, it is breaking.
2. **Additive change.** Open a pull request containing: the specification edit, the implementation,
   a test that exercises the new field or code, and a line in the release note. Reviewers are the
   platform lead and one engineer from a consuming team — the consumer reviewer exists because the
   people who will have to live with a field name should get to object to it before it is frozen.
3. **Breaking change.** Do not edit `gateway.v1.yaml`. Write an ADR under `docs/adr/` proposing
   `/v2`, following the template in `docs/adr/0000-template.md`, and name in it: what breaks, which
   consumers are affected, the migration path, and the deprecation window. ADRs sit for a minimum
   48-hour asynchronous review window so every time zone gets a full working day to object.
4. **Validate before you push.** The documents are plain YAML with no anchors, no aliases and no
   external references, so any OpenAPI 3.1-aware validator will do:

   ```sh
   npx @redocly/cli lint api/openapi/*.yaml
   # or
   docker run --rm -v "$PWD:/w" -w /w redocly/cli lint api/openapi/gateway.v1.yaml
   ```

   At minimum the document must parse as YAML and every `$ref` must resolve within the file. There
   is no `make` target for this yet; if you add one, hang it off `make validate` alongside the
   compose, collector, Prometheus and kustomize checks that already live there.
5. **Regenerate nothing.** There are no generated clients checked into this repository. Consumers
   generate their own, pinned to a specification revision, which is what keeps a client library
   from becoming a second contract that has to be kept in step.

---

## 4. Generating a client

The documents are standard OpenAPI 3.1 with no vendor extensions that affect code generation
(`x-agentgate-sse-frames` is documentation only and every generator ignores it).

```sh
# TypeScript types
npx openapi-typescript api/openapi/gateway.v1.yaml -o src/generated/agentgate.d.ts

# Python client
openapi-python-client generate --path api/openapi/gateway.v1.yaml

# Java / Kotlin / C# / anything else
openapi-generator-cli generate \
  -i api/openapi/gateway.v1.yaml \
  -g java \
  -o ./generated/agentgate-java
```

**You probably do not need to.** The gateway is wire-compatible with the OpenAI Chat Completions
API, so the shortest path is to keep the OpenAI SDK your team already uses and repoint it:

```python
from openai import OpenAI
client = OpenAI(base_url="https://gateway.agentgate.internal/v1", api_key=agentgate_token)
```

Generate a client when you want typed access to the AgentGate-specific parts — the `Problem`
document, the `x-agentgate-*` headers, `/v1/token-count`, or the control plane. See
[`../examples/`](../examples/) for worked Go and Python walkthroughs of both approaches.

Streaming is the one place a generator will not help you: SSE is a framed text protocol and
generated clients model it as an opaque string. Read the frame sequence documented on the `200`
response of `POST /v1/chat/completions` and parse it yourself, or use the OpenAI SDK, which already
does.

---

## 5. Error codes

Every error is an RFC 9457 `application/problem+json` document. **Branch on `code`, never on
`title` or `detail`** — the code is frozen, the prose is not.

| Status | `code` | Meaning | What the caller should do |
|---|---|---|---|
| 400 | `invalid_request` | Malformed body, out-of-range parameter, unsupported role, or the provider rejected the request as invalid. | **Do not retry.** Fix the request. `param` names the offending field when one can be identified. |
| 401 | `unauthenticated` | Token missing, expired, or unverifiable. | Obtain a fresh token and retry **once**. A second 401 with a fresh token is a configuration fault, not a transient one. |
| 403 | `forbidden_pool` | The pool is not in the token's `model_pools`, or the token lacks the required scope. On the control plane, the token lacks the required management scope. | **Do not retry with this token.** Ask the platform team for a pool grant, or request the missing scope. |
| 403 | `agent_not_promoted` | The token is scoped to a different environment than this gateway serves, or the version is not promoted here. | **Do not retry.** Promote the version, or point at the gateway for the environment the token names. |
| 403 | `guardrail_blocked` | Content safety denied the request or the response. Also produced when the upstream provider refuses under its own content policy. | **Do not retry the same content.** `x-agentgate-guardrail` carries `blocked:<category>`. If it is a false positive, raise it with the guardrail owner; the runbook is `docs/runbooks/guardrail-false-positive-spike.md`. |
| 404 | `unknown_model` | The logical model does not exist for this tenant, **or** the caller is not entitled to it. The two are deliberately indistinguishable. | **Do not retry.** Call `GET /v1/models` for the list this token may actually use. |
| 404 | `not_found` | The addressed object does not exist: an unknown pool or backend on the operator surface, an unknown agent, version, promotion or credential on the control plane. | **Do not retry.** Check the identifier. |
| 408 | `client_timeout` | The caller's deadline expired before the gateway could answer. | Retry with a longer deadline or a smaller request. Retrying with the same deadline under the same load times out again. |
| 409 | `idempotency_conflict` | The idempotency key was already used with a different request body. | **Do not retry with the same key.** Use a new key, or send the original body. |
| 409 | `conflict` | The operation contradicts existing state — an identity already owned by a different team, a credential that does not belong to the named agent. | **Do not retry.** This needs a platform action, not another attempt. |
| 409 | `promotion_gate_blocked` | The automated promotion gate refused. Returned by `POST /api/v1/promotions` in a purpose-built envelope, **not** a problem document: it carries this code plus `failed_gates` and the full `PromotionRequest` with the verbatim evaluation. | **Do not retry.** Read `failed_gates` for the short answer and `promotion.gate.checks` for what was observed versus required. Fix the underlying signal, or request a time-boxed waiver. |
| 413 | `context_too_large` | Estimated input plus requested output exceeds the model's context window, or the provider said the same. | **Do not retry unchanged.** Trim context, lower `max_tokens`, or move to a longer-context model. `POST /v1/token-count` answers this before a request is spent. |
| 429 | `rate_limited` | The agent's requests-per-minute limit, the gateway shedding `batch` traffic to protect interactive traffic, or the provider throttling. | **Honour `Retry-After`**, then retry with backoff and full jitter. Retrying sooner turns a limit into an outage. |
| 429 | `quota_exceeded` | The agent's tokens-per-minute limit or monthly token budget. | Honour `Retry-After`. If the monthly budget is exhausted, retrying will not help — the fix is a quota change through the registry. |
| 499 | `client_closed_request` | The caller disconnected mid-stream. | Nothing. **This is never written to the wire** — the connection is already gone. It exists so a cancelled agent run is recorded on the span, in metrics and in the usage record without burning the gateway's error budget. |
| 500 | `internal_error` | An unexpected fault inside the gateway. The underlying error text is deliberately not forwarded. | Retry **once** with backoff. If it persists, open a ticket quoting `x-agentgate-request-id`. |
| 502 | `provider_error` | The upstream provider returned an unrecoverable error, or the gateway could not authenticate to it. Retries and failover are already exhausted. | Honour `Retry-After` when present, then retry with backoff. Persisting means a provider incident; the runbook is `docs/runbooks/provider-degradation.md`. |
| 503 | `no_healthy_backend` | Every backend in the pool is open-circuit, drained, or excluded by data classification. | Honour `Retry-After`. This burns the gateway's availability error budget and will already have paged someone. Runbook: `docs/runbooks/no-healthy-backend.md`. |
| 504 | `provider_timeout` | The provider did not respond within the deadline, after retries. | Retry with backoff, or with a smaller request. |

Two response headers change how you should handle any of these:

* **`Retry-After`** — whole seconds. Always present on 429; present on 502 and 503 when the gateway
  has an estimate. The fleet-wide retry budget is capped at 10% of request volume, so ignoring it
  gets your excess retries shed rather than served.
* **`x-agentgate-degraded: true`** — the gateway served this call with a degraded dependency,
  typically an unreachable distributed rate limiter, meaning limits were not enforced for this
  call. It is not an error. It is there so a consuming team can decide whether to retry or fall
  back, instead of guessing.

---

## Divergence from SPEC

`SPEC.md` is the platform specification. The specifications in this directory describe the
implementation. Where the two disagree, the implementation is documented and the disagreement is
recorded here so it is a decision rather than a discrepancy.

Four divergences found during the first specification pass — a specified-but-unemitted SSE failover
frame, a defined-but-unproduced error code, two version states that were declared but never
assigned, and a JSON field-naming inconsistency on the promotion-signals endpoint — were **fixed in
the code** rather than documented as permanent deviations. They no longer appear below. What
remains is the set where the implementation is deliberately right and `SPEC.md` is behind it.

**D1 — `GET /metrics` is not a gateway endpoint.**
`SPEC.md` §2.1 lists `/metrics` in the gateway's endpoint table. `internal/gateway/server.go`'s
`routes()` does not register it. Each binary exposes Prometheus exposition on a **separate operator
listener** (`cmd/gateway/main.go`, `cmd/controlplane/main.go`, `cmd/fleetview/main.go`), so that
scrape traffic and request traffic can be given different network policies. It is therefore absent
from all three specifications, and the absence is deliberate.

**D2 — `x-agentgate-degraded` is implemented but unspecified.**
The response header table in `SPEC.md` §2.3 does not list it. `internal/gateway/context.go` defines
it and `WriteHeaders` sets it to `true` when the request was served with a degraded dependency. It
is documented here as part of the contract.

**D3 — the `/admin/*` operator surface is implemented but unspecified.**
`SPEC.md` describes no operator routes. The gateway registers four: `GET /admin/backends`,
`POST /admin/backends/{pool}/{backend}/drain`, the matching `undrain`, and
`POST /admin/cache/purge`. They are not token-authenticated — they must work when the control plane
is the thing that is broken — and are therefore **network-restricted**, tagged `admin` in
`gateway.v1.yaml`, and must be unreachable from agent networks.

**D4 — the error set is larger than the specified table.**
`SPEC.md` §2.4 lists fifteen codes. `internal/httpx/problem.go` defines nineteen. The four
additions are `internal_error` (500), `not_found` (404), `conflict` (409) and
`promotion_gate_blocked` (409). All four are additive under §2 rule 4 and all four appear in the
table in §5 above.

**D5 — `x-agentgate-cache` has a fifth value.**
`SPEC.md` §2.3 lists `hit | miss | bypass | refresh`. `internal/cache/cache.go` also defines
`semantic_hit`, which appears on pools where semantic caching has been explicitly enabled (see ADR
0009 — it is off by default). Clients must treat the header as an open enumeration.

**D6 — `x-agentgate-request-id` is also a request header.**
`SPEC.md` §2.2 lists it only as a response header. `requestID()` in
`internal/gateway/server.go` reads it from the **request** and adopts a caller-supplied value up to
128 characters, generating `req_<32 hex>` only when it is absent. This lets a consumer join gateway
records to an id its own system already has, and it is documented as a request parameter in
`gateway.v1.yaml`.

**D7 — 499 is recorded, never returned.**
`SPEC.md` §2.4 lists `client_closed_request` in the HTTP status table. `httpx.WriteProblem` returns
early for status 499 without writing anything, because by definition the connection is gone. The
code exists so that a caller disconnect is attributed on the span, in metrics and in the usage
record without counting against the gateway's availability SLI. It is in the `ErrorCode` enum for
completeness but has no response object.

**D8 — the usage record is a superset of the specified one.**
`SPEC.md` §6 shows eighteen fields. `cost.Record` carries those plus `agent_identity`,
`agent_version`, `pool`, `operation`, `savings_usd`, `estimated`, `unpriced`, `status_code`,
`error_code`, `duration_ms` and `ttft_ms`. All are additive. `estimated` and `unpriced` matter for
chargeback correctness: an estimated token count is never charged as if it had been measured, and a
backend with no price entry produces a zero-cost record marked `unpriced` rather than a silently
free one.
