# Day-2 Operations

Routine changes to a running AgentGate. Each procedure states what it changes, who may run it, the
exact commands, how to verify, and how to roll back.

Three rules apply to everything in this document:

1. **Git is the source of truth.** `agentctl` and `kubectl` apply changes; the committed
   configuration under `deploy/k8s/overlays/prod/` and `registry/` is what the change must
   eventually match. Reconcile before you finish:

   ```bash
   agentctl --context "$CTX" pool diff --against deploy/k8s/overlays/prod/pools.yaml
   ```

2. **Every mutation carries `--reason`**, and every mutation is audited. A change with no reason is
   indistinguishable from a mistake three weeks later.

   ```bash
   agentctl --context "$CTX" audit list --since 7d --kind mutation \
     -o json | jq -r '.[] | "\(.ts)\t\(.actor)\t\(.command)\t\(.reason)"'
   ```

3. **Verify with a metric, not with the absence of an error.** A command that returns zero has
   changed configuration; it has not proven behaviour.

Common environment, assumed throughout — see [README §6](README.md#6-environment-conventions-used-throughout).

```bash
export CTX=prod-eastus NS=agentgate PROM=https://prometheus.internal
export GW=https://gateway.agentgate.internal CP=https://controlplane.agentgate.internal
```

---

## 1. Adding a backend to a pool

**Changes:** routing and capacity. **Who:** platform engineer. **Change class:** standard, needs a
change reference in prod. **Risk:** a new backend that misbehaves takes real traffic immediately, so
introduce it at low weight.

### Pre-flight

```bash
# Reachability from where the gateway actually runs
POD=$(kubectl --context "$CTX" -n "$NS" get pod -l app=gateway -o name | head -1)
kubectl --context "$CTX" -n "$NS" exec "$POD" -- \
  curl -sS -o /dev/null -w 'code=%{http_code} dns=%{time_namelookup} tls=%{time_appconnect} total=%{time_total}\n' \
  --max-time 10 https://fsclient-westus.openai.azure.internal/openai/deployments/gpt-4o-mini/chat/completions

# Classification and residency labels must be right before it takes traffic, not after
agentctl --context "$CTX" backend validate \
  --provider azure-openai --model gpt-4o-mini --endpoint https://fsclient-westus.openai.azure.internal \
  --classification confidential --residency eu-west
```

### Apply

Add at **weight 1** first. One in a hundred requests is enough to learn whether it works and few
enough that it does not matter if it does not.

```bash
agentctl --context "$CTX" pool add-backend --pool general-chat \
  --backend azure-openai/gpt-4o-mini-westus \
  --provider azure-openai --model gpt-4o-mini \
  --endpoint https://fsclient-westus.openai.azure.internal \
  --weight 1 --priority 1 \
  --concurrency-cap 200 --timeout 20s \
  --cost-input-per-1m 0.15 --cost-output-per-1m 0.60 \
  --classification confidential --residency eu-west \
  --reason "CHG0045530 adding westus capacity to general-chat"
```

### Verify before ramping

```bash
promtool query instant "$PROM" \
  'sum by (status) (rate(agentgate_gateway_requests_total{backend="azure-openai/gpt-4o-mini-westus"}[5m]))'
promtool query instant "$PROM" \
  'histogram_quantile(0.95, sum by (le) (rate(agentgate_gateway_ttft_seconds_bucket{backend="azure-openai/gpt-4o-mini-westus"}[5m])))'
promtool query instant "$PROM" 'agentgate_breaker_state{backend="azure-openai/gpt-4o-mini-westus"}'
```

Hold at weight 1 for 30 minutes. Then ramp 1 → 10 → 30 → target, 15 minutes at each step, checking
error ratio and TTFT at every step. Commit the final weights to git and re-apply from there.

### Rollback

```bash
agentctl --context "$CTX" pool remove-backend --pool general-chat \
  --backend azure-openai/gpt-4o-mini-westus --reason "CHG0045530 rollback"
```

---

## 2. Changing pool weights

**Changes:** traffic distribution and cost per request. **Who:** platform engineer; on-call may do
it during an incident. **Risk:** low, immediate effect, trivially reversible.

```bash
agentctl --context "$CTX" pool show --pool general-chat -o json | jq '.backends[] | {backend, weight, priority}'

agentctl --context "$CTX" pool set-weights --pool general-chat \
  --weight azure-openai/gpt-4o-mini=50 \
  --weight bedrock/claude-haiku=50 \
  --reason "CHG0045531 rebalancing after westus addition"
```

Weights are relative within a priority tier, not percentages — 50/50 and 30/30 distribute
identically. Verify the actual split rather than assuming it:

```bash
promtool query instant "$PROM" '
sum by (backend) (rate(agentgate_gateway_requests_total{pool="general-chat"}[5m]))
  / ignoring(backend) group_left sum(rate(agentgate_gateway_requests_total{pool="general-chat"}[5m]))'
```

Then check the two things a weight change moves:

```bash
promtool query instant "$PROM" '
sum(rate(agentgate_gateway_cost_usd_total{pool="general-chat"}[5m]))
  / sum(rate(agentgate_gateway_requests_total{pool="general-chat"}[5m]))'
promtool query instant "$PROM" 'agentgate:gateway_ttft:p95_5m{pool="general-chat"}'
```

**Never restore a weight from memory.** Restore from `deploy/k8s/overlays/prod/pools.yaml`.

**During an incident**, ramp back rather than restoring in one step: a recovering provider
re-degrades under a step load. 10 → 30 → 60, ten minutes at each step.

---

## 3. Onboarding a tenant

**Changes:** a new isolation boundary. **Who:** platform engineer plus the tenant's own owner.
**Risk:** the tenant boundary is a security boundary — verify isolation before handing over.

### Create

```bash
agentctl --context "$CTX" tenant create --id fsclient-markets \
  --display-name "FS Client Markets Division" \
  --default-classification confidential \
  --residency eu-west \
  --owner-email markets-platform@client.example \
  --reason "CHG0045532 markets division onboarding"
```

### Allocate pools and envelopes

```bash
agentctl --context "$CTX" tenant grant-pool --tenant fsclient-markets --pool general-chat
agentctl --context "$CTX" tenant grant-pool --tenant fsclient-markets --pool long-context

agentctl --context "$CTX" team create --tenant fsclient-markets --team rates-analytics \
  --envelope-tokens-per-minute 400000 --envelope-requests-per-minute 2000 \
  --monthly-token-budget 3000000000 --cost-center CC-5512 \
  --reason "CHG0045532"
```

The envelope is the ceiling for the sum of that team's agents. Individual agent quotas are allocated
from it and the `quota_declared` promotion gate enforces the arithmetic.

### Verify isolation — do this before telling them it is ready

```bash
# Cache boundary: same prompt, two tenants, distinct entries
for t in fsclient fsclient-markets; do
  TOKEN=$(agentctl --context "$CTX" token mint --tenant "$t" --agent probe --env staging)
  echo -n "$t: "
  curl -sS -D- -o /dev/null -X POST "$GW/v1/chat/completions" \
    -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' -H 'x-agentgate-cache: on' \
    -d '{"model":"general-chat","messages":[{"role":"user","content":"tenant boundary probe"}],"max_tokens":16,"temperature":0}' \
    | grep -i 'x-agentgate-cache:'
done
# Both must report miss on the first run for each tenant.

# Quota bucket separation
redis-cli -u "$REDIS_URL" --no-auth-warning --scan --pattern 'ag:{fsclient-markets:*' --count 100 | head

# Entitlement: a pool not granted must be refused
TOKEN=$(agentctl --context "$CTX" token mint --tenant fsclient-markets --agent probe --env staging)
curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -H 'x-agentgate-pool: regulated-chat' \
  -d '{"model":"general-chat","messages":[{"role":"user","content":"x"}],"max_tokens":8}'
# Expect 403 forbidden_pool.
```

### Hand over

Give the tenant: their pool entitlements, their envelope, the registration manifest format, the
promotion gate list from [operational-readiness-review.md](operational-readiness-review.md) Part B,
and the consuming-team channel. Set expectations about `x-agentgate-request-priority` and realistic
`max_tokens` at this point, not after their first quota incident.

### Rollback

```bash
agentctl --context "$CTX" tenant suspend --tenant fsclient-markets --reason "CHG0045532 rollback"
```

Suspension is reversible. Deletion is not, and is never done during business hours or without the
tenant owner's written confirmation.

---

## 4. Raising a quota — temporary

**Changes:** one agent's limits, with an expiry. **Who:** platform engineer or on-call.
**Risk:** cost.

```bash
agentctl --context "$CTX" quota set \
  --agent agent://fsclient/payments-risk/dispute-triage --env prod \
  --tokens-per-minute 200000 --requests-per-minute 900 \
  --reason "CHG0045533 month-end backfill, agreed with owning team" --ttl 8h
```

The TTL is mandatory. Verify the limit took effect and that the agent stops being refused:

```bash
promtool query instant "$PROM" '
sum by (decision) (rate(agentgate_ratelimit_decisions_total{agent="agent://fsclient/payments-risk/dispute-triage"}[2m]))'
agentctl --context "$CTX" quota show --agent agent://fsclient/payments-risk/dispute-triage --env prod
```

Rollback: `agentctl --context "$CTX" quota reset --agent agent://... --env prod`, or let the TTL
expire — but **write the expiry into the on-call handover**, because a quota reverting at 06:30
while the backfill is still running produces a page.

---

## 5. Raising a quota — permanent

**Changes:** the registry. **Who:** platform engineer, with the team's envelope confirmed and the
cost owner informed. **Not an on-call action.**

### Check the envelope has room

```bash
agentctl --context "$CTX" team envelope --tenant fsclient --team payments-risk -o json \
  | jq '{envelope_tpm, allocated_tpm, headroom_tpm, envelope_rpm, allocated_rpm, monthly_budget, projected_spend}'
```

If the requested increase exceeds headroom, it needs either a reallocation within the team or an
envelope increase approved by the budget owner. It does not get granted quietly.

### Apply through git

```bash
# Edit registry/agents/dispute-triage.yaml:
#   quota:
#     tokens_per_minute: 200000
#     requests_per_minute: 900
#     monthly_token_budget: 1400000000

agentctl --context "$CTX" registry apply -f registry/agents/dispute-triage.yaml --dry-run
agentctl --context "$CTX" registry apply -f registry/agents/dispute-triage.yaml
psql "$AGENTGATE_PG_URL" -tAc \
  "select quota from agents where identity='agent://fsclient/payments-risk/dispute-triage';"
```

### Sanity check the sizing

A quota set at 1.05x observed peak will page again next week; 2x is healthy. Check what they
actually use:

```bash
promtool query instant "$PROM" '
max_over_time((sum(rate(agentgate_gateway_tokens_total{agent="agent://fsclient/payments-risk/dispute-triage",env="prod"}[1m])) * 60)[7d:1m])'
```

Rollback: `git revert`, then re-apply.

---

## 6. Rotating signing keys

**Changes:** the identity plane. **Who:** platform engineer with security notified in advance.
**Risk:** high if the ordering is wrong. Read
[jwks-rotation-failure.md](jwks-rotation-failure.md) before starting.

**The ordering is the whole procedure.** Publish, wait, switch signing, wait, retire. Each wait must
exceed the maximum token lifetime plus the JWKS cache TTL.

```bash
kubectl --context "$CTX" -n "$NS" get cm gateway-config \
  -o jsonpath='{.data.authn\.jwks_cache_ttl}{"\n"}'
agentctl --context "$CTX" identity config show | jq '{token_ttl, jwks_cache_ttl}'
# WAIT = token_ttl + jwks_cache_ttl, rounded up generously. Typically 15m + 10m -> wait 45m.
```

### Step 1 — generate and publish, do not sign

```bash
agentctl --context "$CTX" identity keys create --kid ag-signing-2026-09 --alg ES256 \
  --reason "CHG0045534 quarterly rotation"
agentctl --context "$CTX" identity keys publish --kid ag-signing-2026-09
curl -sS "$CP/.well-known/jwks.json" | jq '[.keys[].kid]'
```

### Step 2 — wait, and prove every validator has it

```bash
sleep 2700
for p in $(kubectl --context "$CTX" -n "$NS" get pod -l app=gateway -o name); do
  echo -n "$p "
  kubectl --context "$CTX" -n "$NS" exec "$p" -- curl -sS localhost:9090/debug/jwks 2>/dev/null | jq -c '[.keys[].kid]'
done
```

Every pod must list the new kid. Do not proceed on a partial result.

### Step 3 — switch signing

```bash
agentctl --context "$CTX" identity keys set-signing --kid ag-signing-2026-09 \
  --reason "CHG0045534"

SA_TOKEN=$(kubectl --context "$CTX" -n "$NS" create token agentgate-probe --audience=api://agentgate)
AT=$(curl -sS -X POST "$CP/oauth2/token" \
  -H 'content-type: application/x-www-form-urlencoded' \
  -d 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
  -d 'subject_token_type=urn:ietf:params:oauth:token-type:jwt' \
  --data-urlencode "subject_token=$SA_TOKEN" \
  -d 'audience=https://gateway.agentgate.internal' | jq -r .access_token)
echo "$AT" | cut -d. -f1 | base64 -d 2>/dev/null | jq '{kid}'

curl -sS -o /dev/null -w '%{http_code}\n' -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $AT" -H 'content-type: application/json' \
  -d '{"model":"general-chat","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'
# Expect 200.
```

### Step 4 — wait again, watching for rejections

```bash
promtool query instant "$PROM" \
  'sum by (reason) (rate(agentgate_identity_token_validations_total{result="fail"}[5m]))'
# unknown_kid must remain zero throughout. If it is not, publish the missing key immediately.
```

### Step 5 — retire the old key only after the wait

```bash
agentctl --context "$CTX" identity keys retire --kid ag-signing-2026-06 \
  --reason "CHG0045534 rotation complete"
curl -sS "$CP/.well-known/jwks.json" | jq '[.keys[].kid]'
```

Rollback at any step is `agentctl identity keys set-signing --kid <previous>` plus re-publishing any
key you retired. Because publishing is always safe and retiring is not, a rotation that goes wrong is
recoverable as long as you have not retired anything.

---

## 7. Upgrading the gateway with zero downtime

**Changes:** the deployed build. **Who:** platform engineer. **Risk:** managed by rollout strategy
and by watching the right metrics.

### Pre-flight

```bash
kubectl --context "$CTX" -n "$NS" get pdb gateway -o jsonpath='{.spec.minAvailable}{"\n"}'   # expect 75%
kubectl --context "$CTX" -n "$NS" get deploy gateway -o jsonpath='{.spec.strategy}{"\n"}' | jq
# maxSurge >= 25%, maxUnavailable 0. Never allow maxUnavailable above 0 for the gateway.

kubectl --context "$CTX" -n "$NS" get cm gateway-config \
  -o jsonpath='{.data.server\.shutdown_grace}{"\n"}'
# Must exceed the longest expected stream. 300s is the committed value.
```

The termination sequence that makes this zero-downtime: `preStop` marks `/readyz` unhealthy, the
load balancer stops sending new requests, in-flight unary requests complete, streams run to
completion or to the grace deadline, then the process exits. Confirm the hook exists:

```bash
kubectl --context "$CTX" -n "$NS" get deploy gateway -o jsonpath='{.spec.template.spec.containers[0].lifecycle}{"\n"}' | jq
```

### Roll

```bash
kubectl --context "$CTX" -n "$NS" set image deploy/gateway gateway=registry.internal/agentgate/gateway:1.14.2
kubectl --context "$CTX" -n "$NS" rollout status deploy/gateway --timeout=900s
```

### Watch during, not after

```bash
watch -n 10 '
promtool query instant '"$PROM"' "sum by (status) (rate(agentgate_gateway_requests_total{env=\"prod\"}[1m]))"
promtool query instant '"$PROM"' "agentgate:gateway_overhead:p95_1m{env=\"prod\"}"
promtool query instant '"$PROM"' "sum(rate(agentgate_gateway_requests_total{code=\"client_closed_request\"}[1m]))"
'
```

`client_closed_request` rising during a rollout means streams are being cut at pod termination —
either the grace period is too short or the `preStop` hook is not draining. Stop the rollout:

```bash
kubectl --context "$CTX" -n "$NS" rollout pause deploy/gateway
```

### After

```bash
kubectl --context "$CTX" -n "$NS" rollout status deploy/gateway
curl -sS -D- -o /dev/null -X POST "$GW/v1/chat/completions" \
  -H "Authorization: Bearer $AGENTGATE_PROBE_TOKEN" -H 'content-type: application/json' \
  -d '{"model":"general-chat","messages":[{"role":"user","content":"post-upgrade probe"}],"max_tokens":8}' \
  | grep -Ei '^HTTP|x-agentgate-'
```

Check the full response header set against SPEC §2.3 — a missing header is a contract regression that
no error rate will reveal.

Rollback: `kubectl --context "$CTX" -n "$NS" rollout undo deploy/gateway`.

---

## 8. Draining a backend

**Changes:** removes one backend from selection. **Who:** platform engineer or on-call.
**Risk:** low if the pool has headroom; check first.

```bash
# Confirm survivors exist before you drain
promtool query instant "$PROM" 'count by (pool) (agentgate_breaker_state{state="closed"} == 1)'
agentctl --context "$CTX" pool show --pool general-chat -o json \
  | jq '[.backends[] | select(.drained == false and .breaker_state == "closed")] | length'

agentctl --context "$CTX" backend drain --pool general-chat \
  --backend azure-openai/gpt-4o-mini --reason "CHG0045535 provider maintenance window" --ttl 4h
```

Draining is graceful: no new requests, in-flight requests finish, streams run to completion.

```bash
promtool query instant "$PROM" \
  'sum by (backend) (rate(agentgate_gateway_requests_total{pool="general-chat"}[1m]))'
# The drained backend falls to zero within ~60s.
promtool query instant "$PROM" 'sum by (pool) (agentgate_gateway_inflight)'
# Confirm the survivors have absorbed it without approaching their caps.
```

Undrain, ramping rather than restoring in one step:

```bash
agentctl --context "$CTX" backend undrain --pool general-chat --backend azure-openai/gpt-4o-mini
agentctl --context "$CTX" pool set-weights --pool general-chat --weight azure-openai/gpt-4o-mini=10 ...
# then 30, then 60, ten minutes apart, watching error ratio and TTFT
```

**Always set a TTL.** A forgotten drain is a leading cause of `NoHealthyBackend` — see that
runbook's decision tree, where "drain intentional?" is the first branch.

---

## 9. Emergency disable of semantic cache

**Changes:** removes semantic matching; exact-match caching is unaffected. **Who:** on-call, no
approval needed to disable. **Risk:** cost and latency rise; correctness improves.

```bash
agentctl --context "$CTX" cache semantic disable --pool general-chat \
  --reason "INC-#### latency or correctness concern"
# All pools at once:
agentctl --context "$CTX" cache semantic disable --all-pools --reason "INC-####"
```

Verify:

```bash
promtool query instant "$PROM" 'sum by (pool, result) (rate(agentgate_cache_lookups_total[2m]))'
promtool query instant "$PROM" '
sum(rate(agentgate_gateway_cost_usd_total[5m])) / sum(rate(agentgate_gateway_requests_total[5m]))'
```

Expect the hit ratio to fall and cost per request to rise — that movement is your proof it took
effect.

If you suspect the cache has served a response across a boundary, **do not** stop at disabling
semantic matching. Disable the cache entirely and go to
[cache-poisoning-suspected.md](cache-poisoning-suspected.md):

```bash
agentctl --context "$CTX" cache disable --all-pools --reason "INC-#### suspected contamination"
```

Re-enable requires the classification allowance and a verified threshold:

```bash
agentctl --context "$CTX" cache semantic enable --pool general-chat --threshold 0.97 \
  --reason "INC-#### resolved"
promtool query instant "$PROM" '
histogram_quantile(0.01, sum by (le, pool) (rate(agentgate_cache_semantic_similarity_bucket{result="hit"}[10m])))'
# Must be at or above the configured threshold.
```

---

## 10. Switching guardrail failure mode

**Changes:** whether a pool serves unscanned traffic or refuses it when the guardrail service is
unavailable. **Who:** requires risk-and-compliance approval in the direction of `fail_open`.
**Risk:** control coverage.

`fail_closed` is the default for `data_classification=restricted` (SPEC §3.6) and is not a value an
on-call engineer changes alone.

### Look before you touch

```bash
agentctl --context "$CTX" guardrail policy list -o json \
  | jq -r '.[] | "\(.pool)\t\(.failure_mode)\t\(.provider)\t\(.classification)\t\(.window_tokens)"'
promtool query instant "$PROM" 'agentgate_guardrail_failmode == 1'
```

### To `fail_open` — approval required, TTL mandatory

```bash
agentctl --context "$CTX" guardrail set-failmode --pool regulated-chat --mode fail_open \
  --reason "INC-#### guardrail provider outage, restricted pool down 40 min" \
  --approver risk-duty@client.example --change-ref CHG0045536 --ttl 2h

echo "FAIL-OPEN START $(date -u +%FT%TZ) pool=regulated-chat approver=<name> ref=CHG0045536" \
  | tee -a /tmp/failopen-record.txt
promtool query instant "$PROM" 'sum(rate(agentgate_gateway_requests_total{pool="regulated-chat"}[1m])) * 60'
```

Record the request rate: you will need the count of requests processed unscanned for the compliance
record.

### Back to `fail_closed` — no approval needed, do it as soon as you can

```bash
agentctl --context "$CTX" guardrail set-failmode --pool regulated-chat --mode fail_closed \
  --reason "INC-#### guardrail service recovered"
echo "FAIL-OPEN END $(date -u +%FT%TZ)" | tee -a /tmp/failopen-record.txt
promtool query instant "$PROM" 'sum by (pool, action) (rate(agentgate_guardrail_decisions_total[2m]))'
```

Decisions resuming is the verification. Configuration alone is not.

### Prefer switching provider over switching failure mode

If the managed content-safety service is down, `builtin` still scans — weaker detection, but the
pool neither goes down nor goes unscanned. This is usually the better trade:

```bash
agentctl --context "$CTX" guardrail set-provider --pool general-chat --provider builtin \
  --reason "INC-#### managed provider outage" --ttl 4h
```

For a `restricted` pool this substitution also needs risk-and-compliance sign-off, because it changes
what is detected. Ask.

Every fail-open window must appear in the incident's compliance record with start, end, pool, request
count, classification, approver and change reference — the table in
[guardrail-service-down.md §9](guardrail-service-down.md#9-post-incident).

---

## 11. Quick reference

| Operation | Command | Reversible | Approval |
|---|---|---|---|
| Add backend | `agentctl pool add-backend` | yes | change ref |
| Change weights | `agentctl pool set-weights` | yes | none in incident |
| Drain backend | `agentctl backend drain --ttl` | yes | none |
| Onboard tenant | `agentctl tenant create` | suspend, yes; delete, no | change ref |
| Temporary quota | `agentctl quota set --ttl` | yes | none |
| Permanent quota | `agentctl registry apply` | yes, via git | envelope owner |
| Rotate signing key | `agentctl identity keys …` | yes if not retired | security notified |
| Gateway upgrade | `kubectl set image` | yes | change ref |
| Disable semantic cache | `agentctl cache semantic disable` | yes | none |
| Disable cache entirely | `agentctl cache disable` | yes | none |
| Purge cache | `agentctl cache purge` | **no** | export evidence first |
| Guardrail to fail_open | `agentctl guardrail set-failmode` | yes | risk and compliance |
| Suspend agent | `agentctl agent suspend` | yes | IC |
| Block version | `agentctl version block` | yes | notify owning team |
| Regional failover | `scripts/frontdoor-weight.sh` | yes | L4 |

---

## Related

- [oncall-guide.md](oncall-guide.md) — what to write in the handover about time-boxed changes
- [incident-response.md](incident-response.md)
- [operational-readiness-review.md](operational-readiness-review.md)
- [no-healthy-backend.md](no-healthy-backend.md) — forgotten drains
- [jwks-rotation-failure.md](jwks-rotation-failure.md) — when a rotation goes wrong
- [cache-poisoning-suspected.md](cache-poisoning-suspected.md)
- [guardrail-service-down.md](guardrail-service-down.md)
