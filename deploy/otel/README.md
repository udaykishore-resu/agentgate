# OpenTelemetry pipeline

Two tiers, exactly as drawn in SPEC §4.4.

```
agent SDK ────OTLP/gRPC──┐
gateway   ────OTLP/gRPC──┼──►  collector-agent   (DaemonSet, one per node)
controlplane ─OTLP───────┘         │ memory_limiter, k8sattributes,
fleetview ───OTLP────────┘         │ resourcedetection, batch
                                   │ loadbalancing(routing_key: traceID)
                                   ▼
                        collector-gateway  (Deployment, HA pool)
                        ├─ attributes(stamp) → transform(attribution) → redaction
                        ├─ routing connector ─┬─► traces/main  → tail_sampling → OTLP traces store
                        │                     └─► traces/content → OTLP content store (no sampling)
                        ├─ metrics            ──► Prometheus exporter :8889
                        └─ logs               ──► Loki (OTLP)
```

## Files

| File | Where it runs |
|---|---|
| `collector-agent.yaml` | Kubernetes DaemonSet (`deploy/k8s/base/otel-collector-daemonset.yaml`) |
| `collector-agent.compose.yaml` | `docker compose` — identical minus `k8sattributes`, which needs a Kubernetes API to start |
| `collector-gateway.yaml` | Kubernetes Deployment (`deploy/k8s/base/otel-collector-deployment.yaml`) and compose |

Environment variables consumed by the configs:

| Variable | Tier | Meaning |
|---|---|---|
| `AGENTGATE_ENV` | both | Stamped as `deployment.environment.name` when the SDK did not set it |
| `K8S_NODE_NAME` | agent | Downward API; scopes the `k8sattributes` informer to this node |
| `OTEL_GATEWAY_HOST` | agent | Hostname (no port) of the gateway-tier headless Service |
| `TRACES_OTLP_ENDPOINT` | gateway | Trace store (Langfuse / Tempo / Jaeger) |
| `CONTENT_OTLP_ENDPOINT` | gateway | Separate, access-controlled content store |
| `LOKI_ENDPOINT` | gateway | Loki OTLP ingest, e.g. `http://loki:3100/otlp` |
| `TAIL_SAMPLING_BASELINE_PERCENT` | gateway | Baseline keep rate: `100` dev, `20` staging, `5` prod |

## Why two tiers at all

A single tier cannot do both jobs. The first hop has to be node-local and
always-available so that an emitting process is never blocked by the telemetry
plane and so that pod metadata can be attached while the pod still exists. The
second hop has to see *whole traces from the whole fleet* to sample on their
outcome, and has to be the only place that touches content, so that content
handling is one reviewable configuration rather than one per node.

---

## Agent tier — processor by processor

### `memory_limiter` (first, always)
Refuses new data when heap crosses 80% (20% spike headroom) so the process
applies back-pressure instead of being OOMKilled.

**Remove it and:** the collector dies under a traffic spike, taking its
in-memory queue with it. Telemetry loss is silent and correlates exactly with
the incidents you most need telemetry for.

### `k8sattributes`
Adds `k8s.namespace.name`, `k8s.pod.name`, `k8s.deployment.name`, node and
image metadata, plus ownership *hints* from pod labels/annotations
(`agentgate.io/team`, `agentgate.io/cost-center`, `agentgate.io/owner-email`).
Scoped to the local node via `filter.node_from_env_var`.

**Remove it and:** spans arrive with no idea which pod or deployment produced
them. You can still say "the fleet is slow"; you can no longer say which
replica, which image tag, or which rollout made it slow. The annotation hints
also disappear, so the attribution-mismatch signal in the gateway tier has
nothing to compare against.

**Note the scoping:** without `node_from_env_var` every DaemonSet pod watches
every pod in the cluster. On a 400-node cluster that is 400 full-cluster
informers and a very unhappy API server.

### `resourcedetection`
Fills in host/cloud identity (`host.name`, `cloud.*`) with `override: false`,
so an SDK that already knows its resource wins.

**Remove it and:** telemetry from VM and Container Apps runtimes (SPEC §4.1
lists `vm`, `aca`, `lambda`) loses host and region attributes entirely, and
data-residency questions become unanswerable from the data.

**Overlays:** the AKS overlay appends `aks, azure` to `detectors`; the EKS
overlay appends `eks, ec2`.

### `resource/environment`
Guarantees `deployment.environment.name` exists.

**Remove it and:** a single agent that forgets to set it publishes prod spans
that land in the non-prod retention class, or worse, prod content lands where
non-prod access rules apply.

### `batch` (last, always)
Amortises compression and network syscalls.

**Remove it and:** one gRPC export per span. CPU and connection count rise by
an order of magnitude and the gateway tier spends its time on framing.

### `loadbalancing` exporter with `routing_key: traceID`
The most load-bearing line in the agent config.

**Remove it (use a plain `otlp` exporter) and:** spans of one trace are spread
across gateway-tier replicas. Each replica sees a fragment, `tail_sampling`
decides on fragments, and you get partial traces where the error span was kept
and its parent was dropped. The failure is not an outage; it is a permanent,
quiet degradation of trace quality that looks like an instrumentation bug.

---

## Gateway tier — processor by processor

### `attributes/ownership`
Unconditional stamps: `agentgate.telemetry.tier=gateway`, and
`deployment.environment.name` if still missing.

**Remove it and:** you cannot tell whether a span traversed this tier, so a
replayed or side-loaded payload is indistinguishable from a verified one.

### `transform/attribution`
The "an agent cannot lie about who pays" rule from SPEC §4.1, implemented:

* declared cost centre ≠ verified cost centre → `agentgate.attribution.corrected=true`
* no verified cost centre → `agentgate.cost_center=UNATTRIBUTED` and corrected
* declared hint attributes are dropped afterwards

**Remove it and:** chargeback silently mixes verified and self-declared values.
Teams can move spend to another cost centre by editing a pod annotation, and
unattributed traffic disappears from the data instead of showing up as a
countable bucket. The `UNATTRIBUTED` bucket is deliberately a value, not a
missing label — a missing label is invisible in a `sum by (cost_center)`.

### `redaction/pii`
Value-level masking of card numbers, IBANs, SSNs, emails in free text, and
JWT-shaped strings, applied to span and log attributes.
`ignored_keys` exempts the structured ownership fields, which are *meant* to
hold an email or an identity URI.

**Remove it and:** the first provider error message containing a customer
identifier puts regulated data into a telemetry store whose retention and
access model was never approved for it. This is the control that makes the
data-flow review in SPEC §8 passable; it is not a nice-to-have.

**Note:** it runs on the ingest pipeline, before the routing fan-out, so no
downstream path — including the content path — can receive unmasked
attributes.

### `filter/drop-content-events`
Strips `gen_ai.content.prompt` / `gen_ai.content.completion` span events from
the general trace path.

**Remove it and:** prompts and completions land in the ordinary trace backend
that the whole platform team can read, defeating the separate content pipeline
entirely. This is the single processor whose removal turns a compliant
deployment into a reportable incident.

### `filter/content-only`
On the content path, keeps only `gen_ai.chat` / `gen_ai.embeddings` spans.

**Remove it and:** the content store duplicates the entire trace graph,
multiplying the storage that carries the highest-sensitivity data and widening
what the restricted access grant exposes.

### `tail_sampling`
Policies (OR-ed) exactly per SPEC §4.4: errors, guardrail blocks/redactions,
failovers, requests slower than the p99 threshold, and a 5% baseline.

**Remove it and:** either you store 100% of traces — at prod volume that is the
single largest line in the observability bill — or you head-sample, which
throws away errors at the same rate as successes and makes incident
investigation a matter of luck.

**Two settings that are easy to get wrong:**
* `decision_wait: 12s` must exceed p99 end-to-end trace duration. Set it too
  low and slow traces are judged while incomplete — so the "keep everything
  over p99" policy never fires for exactly the traces it exists to keep.
* `num_traces` bounds in-flight traces. Too low and traces are evicted before
  a decision, which looks like random sampling of long traces.

### `routing` connector
Splits by `resource.attributes["agentgate.signal"] == "content"`.

**Remove it and:** there is only one trace pipeline, so content inherits the
general path's retention, backend and access control — see
`filter/drop-content-events` above.

### Cost is not aggregated in this tier — and must not be

An earlier revision summed the `agentgate.cost.usd` span attribute into
`agentgate_gateway_cost_usd_total` with a `sum` connector on the ingest
pipeline. That has been removed, for two reasons, and it should not be added
back without addressing both.

**It double-counted.** The gateway already exports a counter of that exact
name from its own `/metrics` endpoint, and Prometheus scrapes both `gateway:9090`
and the collector's exporter on `otel-gateway:8889`. Two producers writing one
metric name do not give you a figure to reconcile against — they give you one
series that reads roughly twice actual spend, in a dashboard where nothing looks
wrong until the finance team asks.

**It does not exist in the pinned distribution.** `sum` is not a component in
`otel/opentelemetry-collector-contrib:0.115.1`; the collector refuses to start.

The property the connector was reaching for — cost measured before sampling —
is already guaranteed at the source. The gateway meters every request in
`finish()`, which runs on the request path where no sampling decision exists,
so the counter is complete by construction rather than by pipeline ordering.
The authoritative ledger is the per-request `UsageRecord` written by the
chargeback sink; the Prometheus counter is the queryable view of it.

If a genuinely independent second figure is ever wanted for invoice disputes,
it needs a **different metric name** and an explicit reconciliation query, not
a second writer to the same series.

### `batch` / `batch/content`
Separate batchers because the payload sizes differ by two orders of magnitude.
Content batches are kept small so a burst of long completions cannot produce a
single export larger than the content backend's limit.

### `prometheus` exporter with `resource_to_telemetry_conversion: false`
**Turn it on and:** every resource attribute becomes a label — including
`k8s.pod.name` and `container.image.tag`. Every deploy then creates a fresh
set of series for every metric, and the TSDB's memory grows with deploy
frequency rather than with fleet size. The gateway already emits the ownership
labels defined in SPEC §4.3 as proper metric attributes; nothing is lost.

### `otlp/content` sending queue is small on purpose
`queue_size: 512`. If the content backend is unavailable, content is dropped
rather than accumulating megabytes of prompts and completions in the memory of
a pod whose heap can be captured. Availability of the content path is worth
less than not holding that data.

---

## Changing the sampling rate per environment

`TAIL_SAMPLING_BASELINE_PERCENT` is the only knob:

| Environment | Value | Why |
|---|---|---|
| dev | `100` | Volume is trivial; developers need to find their own trace |
| staging | `20` | Enough to characterise behaviour before promotion |
| prod | `5` | SPEC §4.4 baseline; errors/blocks/failovers/slow are still 100% |

Set in `deploy/k8s/overlays/<env>/`. Never lower the *policy* set to save
money — lower the baseline. The policies are what make sampled data usable.

## Verifying a change

```bash
# Config parses and every component resolves (no cluster required):
docker run --rm -v "$PWD/deploy/otel:/etc/otel:ro" \
  -e AGENTGATE_ENV=dev -e OTEL_GATEWAY_HOST=localhost \
  -e TRACES_OTLP_ENDPOINT=localhost:4317 \
  -e CONTENT_OTLP_ENDPOINT=localhost:4317 \
  -e LOKI_ENDPOINT=http://localhost:3100/otlp \
  -e TAIL_SAMPLING_BASELINE_PERCENT=5 \
  otel/opentelemetry-collector-contrib:0.115.1 \
  validate --config=/etc/otel/collector-gateway.yaml

# Live: zpages at :55679/debug/tracez, self-metrics at :8888/metrics.
# `otelcol_processor_tail_sampling_sampling_trace_dropped_too_early` > 0 means
# decision_wait is too short or num_traces is too small.
```
