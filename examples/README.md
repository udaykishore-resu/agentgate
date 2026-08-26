# Examples

A complete walkthrough of the agent lifecycle: **register → get a token → call the gateway →
promote → see it in the fleet view**, using commands that exist and flags that are real.

| File | What it is |
|---|---|
| [`agent.yaml`](agent.yaml) | The registration manifest a team commits to its own repository. Heavily commented. |
| [`go/main.go`](go/main.go) | A dependency-free Go agent: token, unary call, streaming call, trace propagation, error handling. Copy it out; it does not import anything from this repository. |
| [`python/agent_example.py`](python/agent_example.py) | The same walkthrough with the stock `openai` SDK repointed at the gateway. |

Every command below has a `curl` equivalent, so nothing here depends on `agentctl` being available
— it is the same set of calls a CI pipeline makes, in a form an engineer can run by hand.

---

## 0. Start the local stack

```sh
make run-stack          # gateway :8080, control plane :8081, fleet view :8082
make smoke              # end-to-end request asserting the frozen contract headers
```

Two things about the local stack are deliberately unlike production, and both matter for what
follows:

* **The control plane accepts an actor header instead of a bearer token.**
  `AGENTGATE_ALLOW_ANONYMOUS_ADMIN=true` makes the management API take the audit actor from
  `x-agentgate-actor`, so `agentctl` works without a bootstrap identity. The control plane refuses
  this outside development. Set `AGENTGATE_ACTOR` to something that identifies you — it lands in
  the audit trail, and `anonymous@localdev` in an approval record helps nobody.
* **The gateway accepts unverified tokens.** `config/gateway.yaml` sets
  `identity.allow_unverified: true`, so a local gateway synthesises a developer identity rather
  than verifying a token. Config validation refuses it in staging and production. This is why the
  local `curl` examples below work even before you have a token — but get a real token anyway, or
  you will not find out that your token plumbing is broken until staging.

Set these once:

```sh
export AGENTGATE_CONTROLPLANE_URL=http://localhost:8081
export AGENTGATE_GATEWAY_URL=http://localhost:8080
export AGENTGATE_FLEET_URL=http://localhost:8082
export AGENTGATE_ACTOR="$(git config user.email)"
```

`agentctl` reads all four, plus `AGENTGATE_TOKEN` for authenticated calls.

---

## 1. Register

Registration is how an agent becomes known to the platform. It is a precondition for identity
issuance, not a nice-to-have a team fills in later: an unregistered agent cannot obtain a token, so
it cannot reach the gateway at all.

```sh
agentctl register -f examples/agent.yaml -env dev -credential
```

| Flag | Meaning |
|---|---|
| `-f` | Manifest path. JSON, or the YAML subset. Default `agent.yaml`. |
| `-env` | Environment to register into. **Only applied when the manifest omits `env`.** |
| `-version` | Overrides the manifest's `version`. This is what CI passes. |
| `-commit` | Overrides `commit_sha`. |
| `-image` | Overrides `image`. |
| `-credential` | Issue a client secret. Omit it wherever federated workload identity is available. |

Expected output:

```
registered agent://fsclient/payments-risk/dispute-triage version 2.4.1 in dev (state active, agent_id agt_01j8z9x2qk4m7p3r5t)

client_id:     cli_01j8z9x2qk4m7p3r5t9v
client_secret: ags_Q2xpZW50U2VjcmV0RXhhbXBsZVZhbHVl
This secret is shown once. Store it in the vault now.
```

Note `state active`. A version registered into **dev** is active immediately — an engineer must be
able to run their agent against the gateway the same day they write it, or they will build
something that bypasses it. A version registered into staging or production is `registered` and
must be promoted.

Registration is **idempotent on (identity, env, version)**. Re-running your pipeline does not
create a second agent or a second credential, so it is safe on every build.

Read the warnings. They are problems that do not block registration but *will* block promotion, and
they are printed one deployment early on purpose:

```
  warning: no security review reference is linked; the production promotion gate will block
  warning: a client secret was issued; prefer federated workload identity, which the production promotion gate requires
```

<details>
<summary><code>curl</code> equivalent</summary>

The management API takes JSON, so the manifest has to be converted. `agentctl` does this for you;
by hand, write the manifest as JSON, or convert it with any YAML tool:

```sh
curl -sS -X POST "$AGENTGATE_CONTROLPLANE_URL/api/v1/agents" \
  -H 'content-type: application/json' \
  -H "x-agentgate-actor: $AGENTGATE_ACTOR" \
  -d '{
    "identity": "agent://fsclient/payments-risk/dispute-triage",
    "display_name": "Dispute Triage Agent",
    "owner": {
      "team": "payments-risk",
      "email": "payments-risk@client.example",
      "oncall": "PD-PAYRISK",
      "cost_center": "CC-4471"
    },
    "runtime": "aks",
    "framework": "langgraph",
    "data_classification": "confidential",
    "requested_pools": ["general-chat", "long-context"],
    "quota": {
      "tokens_per_minute": 120000,
      "requests_per_minute": 600,
      "monthly_token_budget": 900000000
    },
    "version": "2.4.1",
    "env": "dev",
    "issue_credential": true
  }' | jq
```

Outside development, replace the actor header with `-H "authorization: Bearer $ADMIN_TOKEN"`. The
token needs the `agents:register` scope.

Unknown JSON fields are **rejected**, so a typo fails here rather than being silently dropped and
discovered on promotion day.
</details>

---

## 2. Get a token

Two grants. Use the first one wherever the runtime supports it.

### Token exchange — preferred, no secret

The agent's runtime has already proved who it is. The control plane converts that proof into an
AgentGate identity, and no shared secret ever exists. The resulting token carries
`attestation=workload-identity`, which is what the production promotion gate requires.

```sh
agentctl token \
  -subject-token "$(cat /var/run/secrets/tokens/agentgate)" \
  -agent agent://fsclient/payments-risk/dispute-triage \
  -env dev
```

| Flag | Meaning |
|---|---|
| `-subject-token` | The platform-issued workload token. Selects the token-exchange grant. |
| `-agent` | The agent identity URI being claimed. |
| `-env` | Environment. Default `dev`. |
| `-agent-version` | A specific version. Defaults to whatever is `active` in that environment. |
| `-q` | Print only the access token, for `$(...)` capture. |

Read the projected token at each use — the kubelet refreshes it in place, so a cached copy goes
stale.

### Client credentials — the fallback

```sh
agentctl token -client-id cli_01j8z9x2qk4m7p3r5t9v -client-secret ags_... -env dev
```

```
agent_id: agt_01j8z9x2qk4m7p3r5t
env:      dev
version:  2.4.1
expires:  900s

export AGENTGATE_TOKEN=eyJhbGciOiJSUzI1NiIs...
```

For scripting:

```sh
export AGENTGATE_TOKEN=$(agentctl token -client-id "$CLIENT_ID" -client-secret "$CLIENT_SECRET" -env dev -q)
```

**900 seconds is not a mistake.** Token lifetimes are short so that a quarantine takes effect
quickly. A long-running agent must refresh — at roughly half the lifetime, not at expiry. A token
that expires mid-request produces a 401 the caller cannot tell apart from a real authentication
failure.

<details>
<summary><code>curl</code> equivalent</summary>

The endpoint is form-encoded and returns OAuth2-shaped errors, not problem+json.

```sh
# Token exchange
curl -sS -X POST "$AGENTGATE_CONTROLPLANE_URL/oauth2/token" \
  -d grant_type=urn:ietf:params:oauth:grant-type:token-exchange \
  -d subject_token="$(cat /var/run/secrets/tokens/agentgate)" \
  -d subject_token_type=urn:ietf:params:oauth:token-type:jwt \
  -d agent_identity=agent://fsclient/payments-risk/dispute-triage \
  -d env=dev | jq

# Client credentials
curl -sS -X POST "$AGENTGATE_CONTROLPLANE_URL/oauth2/token" \
  -d grant_type=client_credentials \
  -d client_id="$CLIENT_ID" \
  -d client_secret="$CLIENT_SECRET" \
  -d env=dev | jq -r .access_token
```
</details>

### Rotating a client secret

```sh
curl -sS -X POST \
  "$AGENTGATE_CONTROLPLANE_URL/api/v1/agents/$AGENT_ID/credentials/$CLIENT_ID/rotate?overlap=72h" \
  -H "x-agentgate-actor: $AGENTGATE_ACTOR" | jq
```

The overlap window is why the previous secret keeps working for 72 hours: a running agent picks up
the new one on its next restart rather than at the instant of rotation. A rotation with no overlap
is an outage scheduled for whenever the agent next needs a token.

---

## 3. Call the gateway

### What models may this token use?

```sh
agentctl models
```

A model that exists on the platform but that this agent has not been granted is **absent** from
this list, not forbidden. When a call fails with `unknown_model`, look here first.

### Unary

```sh
agentctl chat -model general-chat -p "Classify this dispute: contactless, 42.10 GBP, 3 March."
```

| Flag | Meaning |
|---|---|
| `-model` | Logical model. Default `general-chat`. |
| `-p` | The prompt. Reads stdin when omitted. |
| `-stream` | Stream the response. |
| `-max-tokens` | Output cap. Default 256. |
| `-temperature` | Sampling temperature. Default 0. |

`agentctl chat` prints the AgentGate response headers to **stderr** and the content to stdout, so
`agentctl chat -p "..." > answer.txt` keeps the two apart:

```
request req_2f9c0a1d4b8e4c7f9a11 trace 4bf92f3577b34da6a3ce929d0e0e4736 provider azure-openai model gpt-4o-mini attempts 1 cache miss cost 0.000462
```

Those headers are half the value of routing through a gateway. They tell you what the call cost,
which backend served it, whether it was cached and how many attempts it took — per call, without a
dashboard.

### Streaming

```sh
agentctl chat -model general-chat -stream -p "Summarise the chargeback reason codes."
```

`-stream` also sets `stream_options.include_usage`, so the final `agentgate.usage` frame arrives
with cost and token counts. `agentctl` prints named events to stderr prefixed with the event name.

<details>
<summary><code>curl</code> equivalent</summary>

```sh
# Unary, showing the response headers
curl -sS -D - -X POST "$AGENTGATE_GATEWAY_URL/v1/chat/completions" \
  -H "authorization: Bearer $AGENTGATE_TOKEN" \
  -H 'content-type: application/json' \
  -H 'x-agentgate-session-id: sess_8f21c4' \
  -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' \
  -d '{
    "model": "general-chat",
    "messages": [{"role": "user", "content": "Classify this dispute."}],
    "max_tokens": 256,
    "temperature": 0.2
  }'

# Streaming. -N disables curl's buffering, or you will see the whole stream at once
# and conclude that time-to-first-token is terrible.
curl -sS -N -X POST "$AGENTGATE_GATEWAY_URL/v1/chat/completions" \
  -H "authorization: Bearer $AGENTGATE_TOKEN" \
  -H 'content-type: application/json' \
  -H 'accept: text/event-stream' \
  -d '{
    "model": "general-chat",
    "messages": [{"role": "user", "content": "Summarise the chargeback reason codes."}],
    "max_tokens": 512,
    "stream": true,
    "stream_options": {"include_usage": true}
  }'

# Pre-flight: what will this cost me in context, before I spend a request?
curl -sS -X POST "$AGENTGATE_GATEWAY_URL/v1/token-count" \
  -H "authorization: Bearer $AGENTGATE_TOKEN" \
  -H 'content-type: application/json' \
  -d '{"model":"long-context","messages":[{"role":"user","content":"..."}],"max_tokens":2048}' | jq
```

The streaming output looks like this. Note the `:` heartbeat, the named `agentgate.usage` frame,
and the `[DONE]` sentinel:

```
data: {"id":"chatcmpl-9f2","object":"chat.completion.chunk","created":1774526521,"model":"general-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"The "}}]}

data: {"id":"chatcmpl-9f2","object":"chat.completion.chunk","created":1774526521,"model":"general-chat","choices":[{"index":0,"delta":{"content":"dispute qualifies."}}]}

: heartbeat

data: {"id":"chatcmpl-9f2","object":"chat.completion.chunk","created":1774526522,"model":"general-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

event: agentgate.usage
data: {"request_id":"req_2f9c...","trace_id":"4bf9...","provider":"azure-openai","backend":"azure-openai/gpt-4o-mini","model":"general-chat","attempts":1,"usage":{"prompt_tokens":1842,"completion_tokens":311,"total_tokens":2153},"cost_usd":0.000462,"estimated":false,"stalls":0}

data: [DONE]
```

**A stream that ends without `data: [DONE]` failed.** Once the first content byte is written the
HTTP status cannot be changed, so the contract carries the failure in band as an `event: error`
frame. Treat a truncated stream as an error, never as a short answer.
</details>

### From your own code

The Go and Python examples in this directory do everything above properly — token acquisition,
trace propagation, header reading, and error handling that honours `Retry-After` on 429 and never
retries a 400.

```sh
# Go: copy it out, because it is a template rather than a library
mkdir -p /tmp/agentgate-example && cp examples/go/main.go /tmp/agentgate-example/
cd /tmp/agentgate-example && go mod init example && go run main.go

# Python
pip install "openai>=1.0" requests
python3 examples/python/agent_example.py
```

For most teams the Python path is the short one: the gateway is wire-compatible with the OpenAI
Chat Completions API, so an existing agent becomes an AgentGate agent by changing two arguments.

```python
client = OpenAI(base_url="http://localhost:8080/v1", api_key=agentgate_token)
```

---

## 4. Promote

`dev → staging → prod`. Each transition runs the automated gate; production additionally requires
two human approvals.

```sh
agentctl promote -agent-id agt_01j8z9x2qk4m7p3r5t -version 2.4.1 -to staging
```

Every gate runs even after one fails. An engineer given one blocker at a time makes one fix at a
time, and each round trip costs a deployment cycle:

```
promotion prm_1f0c8b3a-7d21-4e55-9a6b-0c2e8f4d1a93: dev → staging, state applied

gate:
  [ok  ] registration_complete    owner, on-call, cost centre and data classification must be present; missing: []
         observed 0 missing, required 0 missing
  [ok  ] identity_attested        the agent must have authenticated with federated workload identity in the source environment
         observed client-secret, required workload-identity
  [FAIL] telemetry_healthy        traces must be complete and correctly attributed over the last 24 hours
         observed 0.000 completeness over 0 requests, required >= 0.95 completeness over >= 100 requests
  [ok  ] error_budget             the agent's own success rate must meet its objective in the source environment
  [ok  ] guardrail_clean          no unresolved critical guardrail violations in the last 7 days
  [ok  ] quota_declared           requested tokens per minute must fit inside the team's allocated envelope
  [ok  ] cost_projection          projected monthly spend must fit inside the team's budget
  [skip] security_review          security review is required for production promotions only
```

**You will hit `telemetry_healthy` on a fresh local stack, and that is correct.** The gate needs at
least 100 gateway requests with at least 95% trace completeness, and a stack you started ten
minutes ago has neither. The honest fixes are to generate traffic (`make load`) or to waive the
gate deliberately — see below. Note also `identity_attested` observing `client-secret`: that passes
for staging, where any attestation will do, and fails for production, where only
`workload-identity` does.

A staging promotion needs **no** approvals: if the gate passes, the state is `applied` and the
version is active immediately. A production promotion is `pending` and needs two.

### Approving a production promotion

```sh
agentctl promote -agent-id agt_01j8z9x2qk4m7p3r5t -version 2.4.1 -to prod
# ... prints prm_4c1e9a02-...

AGENTGATE_ACTOR=a.okafor@client.example \
  agentctl approve -id prm_4c1e9a02-... -role owning_team -comment "Gate evidence reviewed."

AGENTGATE_ACTOR=s.mensah@client.example \
  agentctl approve -id prm_4c1e9a02-... -role platform -comment "Telemetry completeness 0.994."
```

| Flag | Meaning |
|---|---|
| `-id` | Promotion id. |
| `-role` | `owning_team` or `platform`. Both are required for production. |
| `-reject` | Record a rejection instead. Moves the promotion straight to `rejected`. |
| `-comment` | Recorded in the audit trail. |

The two-party rule is enforced, not advisory. **The requester may not approve their own
promotion**, the same actor may not record two decisions, and a role that is already satisfied
cannot be satisfied again. Each of those returns 409 with the promotion record attached, because
they are decisions rather than faults. This is why `AGENTGATE_ACTOR` changes between the two
commands above: with anonymous admin, that header *is* the identity.

Once both approvals land, the promotion is applied in the same call: the version becomes `active`
in production and whatever was active there is `retired` rather than deleted — which is what makes
an emergency rollback a state change instead of a redeploy.

### Waiving a gate

```sh
agentctl waiver \
  -agent-id agt_01j8z9x2qk4m7p3r5t \
  -gate telemetry_healthy \
  -reason "local stack has no traffic history; tracked in PLAT-4471" \
  -days 7
```

`-days` is clamped to 1–90; anything outside becomes 30. An exception without an expiry is a policy
change made quietly.

A waived gate shows as `[waiv]` with the reason and waiver id attached — **the exception is visible
in the evidence rather than hidden by it**, which is the entire point.

### Reviewing and stopping

```sh
agentctl promotions -state pending          # what is awaiting a decision
agentctl promotions -agent-id agt_...       # one agent's promotion history
agentctl agent agt_...                      # the full record, including every gate snapshot
```

```sh
agentctl quarantine -agent-id agt_... -version 2.4.1 -env prod \
  -reason "unbounded tool loop consuming 40x its declared quota; INC0091422"
```

Quarantine is the emergency stop. `-reason` is required — a quarantine nobody can review afterwards
is not a control. It takes effect at the agent's next token refresh, which is why token lifetimes
are short.

<details>
<summary><code>curl</code> equivalents</summary>

```sh
# Request a promotion. 201 when pending or applied, 409 when the gate blocked -
# and a 409 body is the full promotion record with the failing checks, not an
# apology.
curl -sS -X POST "$AGENTGATE_CONTROLPLANE_URL/api/v1/promotions" \
  -H 'content-type: application/json' \
  -H "x-agentgate-actor: $AGENTGATE_ACTOR" \
  -d '{"agent_id":"agt_01j8z9x2qk4m7p3r5t","version":"2.4.1","to":"staging"}' | jq '.state, .gate.checks'

# Approve
curl -sS -X POST "$AGENTGATE_CONTROLPLANE_URL/api/v1/promotions/$PROMOTION_ID/approve" \
  -H 'content-type: application/json' \
  -H 'x-agentgate-actor: a.okafor@client.example' \
  -d '{"role":"owning_team","approved":true,"comment":"Gate evidence reviewed."}' | jq

# Waive
curl -sS -X POST "$AGENTGATE_CONTROLPLANE_URL/api/v1/waivers" \
  -H 'content-type: application/json' \
  -H "x-agentgate-actor: $AGENTGATE_ACTOR" \
  -d '{"agent_id":"agt_...","gate":"telemetry_healthy","reason":"no traffic history yet","days":7}' | jq

# Quarantine
curl -sS -X POST "$AGENTGATE_CONTROLPLANE_URL/api/v1/agents/$AGENT_ID/quarantine" \
  -H 'content-type: application/json' \
  -H "x-agentgate-actor: $AGENTGATE_ACTOR" \
  -d '{"version":"2.4.1","env":"prod","reason":"INC0091422"}' | jq
```
</details>

---

## 5. See it in the fleet view

```sh
agentctl fleet
```

```json
{
  "agents": [
    {
      "agent_id": "agt_01j8z9x2qk4m7p3r5t",
      "identity": "agent://fsclient/payments-risk/dispute-triage",
      "team": "payments-risk",
      "prod_version": "2.4.1",
      "requests_last_hour": 18422,
      "spend_usd_last_24h": 122.41,
      "error_rate": 0.0012,
      "telemetry": {
        "completeness": 0.994,
        "orphan_ratio": 0.0045,
        "unattributed_ratio": 0,
        "clock_skew_p99_seconds": 0.31
      },
      "health": "healthy"
    }
  ],
  "count": 1
}
```

The `health` verdict is deliberately opinionated. `unhealthy` is set by gateway traffic with no
traces arriving, an error rate above 5%, or a quarantined version. `degraded` is set by a missing
on-call rotation or cost centre, production without a linked security review, trace completeness
below the bar, more than 1% unattributed spans, or clock skew above five seconds. A fleet view that
reports everything as green until it is on fire is a decoration.

`http://localhost:8082/` serves the same data as an HTML dashboard.

### Error budgets

```sh
agentctl slo
```

Reports each objective's SLI, whether it is met, and how much error budget remains — in events and
in minutes. No traffic is **not** a breach: an idle window reports an SLI of 1 with a full budget,
because reporting 0% availability for an idle hour is how a dashboard trains people to ignore it.

### Chargeback

```sh
agentctl chargeback -period day
agentctl chargeback -period day -cost-center CC-4471
```

| Flag | Meaning |
|---|---|
| `-period` | `hour` or `day`. Default `day`. |
| `-cost-center` | Restrict to one cost centre. |

`savings_usd` is what cache hits avoided spending. A shared platform that can only report what it
cost, and never what it saved, is a shared platform that will eventually be asked to stop spending.

### Telemetry trust

```sh
curl -sS "$AGENTGATE_FLEET_URL/api/v1/telemetry/health" | jq
```

This is the metric the platform holds *itself* to, and it is a hard input to the promotion gate: an
agent whose telemetry cannot be believed cannot reach production. `missing_attributes` is the
actionable half — it names exactly which resource attributes the owning team has to add.

<details>
<summary><code>curl</code> equivalents</summary>

```sh
curl -sS "$AGENTGATE_FLEET_URL/api/v1/agents?team=payments-risk&health=degraded" | jq
curl -sS "$AGENTGATE_FLEET_URL/api/v1/agents/agt_01j8z9x2qk4m7p3r5t" | jq
curl -sS "$AGENTGATE_FLEET_URL/api/v1/slo" | jq '.objectives[] | {name, sli, met, budget_minutes_remaining}'
curl -sS "$AGENTGATE_FLEET_URL/api/v1/chargeback?period=day&cost_center=CC-4471" | jq
# The same signals the promotion gate reads, so you can see why a gate will
# pass or fail before you request the promotion.
curl -sS "$AGENTGATE_FLEET_URL/api/v1/signals/agt_01j8z9x2qk4m7p3r5t?env=staging" | jq
```
</details>

---

## Putting it in CI

The whole registration step is three lines, and it is the same three lines in every pipeline:

```sh
agentctl register -f agent.yaml \
  -env "$TARGET_ENV" \
  -version "$APP_VERSION" \
  -commit "$GIT_SHA" \
  -image "$IMAGE_REF"
```

Leave `env` out of the committed manifest so one file serves dev, staging and production. Do not
pass `-credential` from a pipeline that has workload identity available — a secret issued in CI is
a secret that has to be rotated, stored and eventually leaked.

Promotion is a separate, deliberate step, because it is a decision rather than a build artefact:

```sh
agentctl promote -agent-id "$AGENT_ID" -version "$APP_VERSION" -to staging
```

`agentctl promote` exits non-zero when the gate blocks, so a pipeline that calls it fails the way
you want it to.

---

## Reference

* [`../api/openapi/gateway.v1.yaml`](../api/openapi/gateway.v1.yaml) — the frozen wire contract.
* [`../api/README.md`](../api/README.md) — every error code, its status, and what to do about it.
* `agentctl help` — the full command list.
