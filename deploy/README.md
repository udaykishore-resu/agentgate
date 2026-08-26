# AgentGate — deployment and infrastructure

Everything needed to run AgentGate on a laptop, in a cluster, or in either
cloud. The core is cloud-neutral; the Azure and AWS directories are two
mappings of the same design, and the table at the bottom of this file is the
translation between them.

```
deploy/
├── docker/        Multi-stage Dockerfile (one file, parameterised by SERVICE)
├── compose/       Local-stack config: gateway config, postgres init, dev keys
├── otel/          OpenTelemetry Collector, agent tier and gateway tier
├── prometheus/    Scrape config, SLO recording + burn-rate rules, operational
│                  alerts, Alertmanager routing, notification templates
├── grafana/       Datasource + dashboard provisioning, three dashboards
├── k8s/           Base manifests and dev/staging/prod overlays
└── terraform/
    ├── azure/     AKS, PostgreSQL, Redis, Key Vault, Azure OpenAI, APIM,
    │              Private Endpoints, Log Analytics, workload identity
    └── aws/       EKS, RDS, ElastiCache, Secrets Manager, Bedrock, ALB,
                   PrivateLink, CloudWatch/X-Ray, IRSA
```

---

## Running locally

### Prerequisites

* Docker ≥ 24 with Compose v2, `curl`, `jq`, `openssl`
* ~6 GB of RAM free — the stack is fourteen containers
* Go 1.24 only if you want to run `make load` or the binaries outside Docker

### Start it

```bash
make run-stack          # builds every image and waits for health
make smoke              # end-to-end request, checks every contract header
```

`make run-stack` generates a local token-signing key on first use
(`deploy/compose/keys/signing.pem`, git-ignored) and brings up:

| Service | Address | Notes |
|---|---|---|
| gateway | http://localhost:8080 | `/v1`, `/healthz`, `/readyz` |
| gateway metrics | http://localhost:9090/metrics | |
| controlplane | http://localhost:8081 | token exchange, registry, promotion |
| fleetview | http://localhost:8082 | fleet UI, SLO, chargeback |
| guardrails | http://localhost:8083 | `builtin` mode — offline, no external calls |
| mockprovider ×3 | 8090 / 8091 / 8092 | azure-openai, bedrock, on-prem profiles |
| postgres | localhost:5432 | `agentgate` / `agentgate` |
| redis | localhost:6379 | |
| otel-collector (agent) | localhost:4317, 4318 | point your own agent SDK here |
| otel-collector (gateway) | localhost:14317 | tail sampling, routing, cost |
| prometheus | http://localhost:9099 | |
| grafana | http://localhost:3000 | anonymous viewer, admin/admin |
| jaeger | http://localhost:16686 | |
| loki | http://localhost:3100 | |

Optional profiles:

```bash
docker compose --profile alerting up -d    # Alertmanager on :9093
docker compose --profile langfuse up -d    # Langfuse on :3001
```

### What the local stack is actually for

The three mockprovider instances have deliberately different latency and
failure profiles (180ms/1%, 420ms/8%, 900ms/0%), matching the three-backend
pool in SPEC §3.1. That means retry, failover, the circuit breaker and the
failover tier are all reachable on a laptop:

```bash
make load DURATION=120s RATE=100     # drive it hard enough to trip a breaker
open http://localhost:3000           # watch it on the golden-signals dashboard
```

### Useful commands

```bash
make logs SVC=gateway     # follow one service
make validate             # compose, collector, promtool, kustomize — no cluster needed
make stop-stack           # stop, keep data
make reset-stack          # stop, delete volumes (fresh database)
```

---

## Deploying to a cluster

Terraform builds the platform; kustomize deploys the workloads. They are
separate on purpose: a cluster must be fixable during an incident without a
successful `terraform apply` standing in the way.

### 1. Platform

```bash
make tf-azure-plan ENV=dev && make tf-azure-apply ENV=dev
# or
make tf-aws-plan   ENV=dev && make tf-aws-apply   ENV=dev
```

Each cloud directory has its own README with prerequisites, the state-bucket
bootstrap, and the quota requests that must be raised in advance (Azure OpenAI
capacity; Bedrock model access).

### 2. Cluster prerequisites

The manifests assume these are already installed. They are not in this
repository because they are cluster-wide concerns shared with other teams:

| Component | Why the manifests need it |
|---|---|
| external-secrets operator | `ExternalSecret` / `SecretStore` in `k8s/base/externalsecrets.yaml` |
| cert-manager + `enterprise-pki` ClusterIssuer | `Certificate` in `k8s/base/httproute.yaml` |
| A Gateway API controller with GatewayClass `agentgate-internal` | `Gateway` / `HTTPRoute` (use `ingress-alternative.yaml` if unavailable) |
| Cilium or Calico | NetworkPolicy enforcement — **the AWS VPC CNI does not enforce it** |
| prometheus-adapter or KEDA | The HPA's `agentgate_gateway_inflight` custom metric |

### 3. Workloads

```bash
make k8s-render ENV=prod | less    # see exactly what will be applied
make k8s-diff   ENV=prod           # diff against the live cluster
make k8s-dev                       # apply
make k8s-staging
make k8s-prod                      # asks you to type the context name first
```

Patch the identity values from the Terraform outputs into the ServiceAccounts
before the first apply:

```bash
terraform -chdir=deploy/terraform/azure output -json workload_identity_client_ids
terraform -chdir=deploy/terraform/aws   output -json irsa_role_arns
```

### What differs per environment

| | dev | staging | prod |
|---|---|---|---|
| gateway replicas | 1 | 2 | 6 (HPA to 60) |
| control plane replicas | 1 | 2 | 3 |
| guardrails replicas | 1 | 2 | 6 |
| collector (gateway tier) | 1 | 2 | 4 |
| content capture | `redacted` | `redacted` | `off` |
| guardrail failure mode | `fail_open` | `fail_open` | `fail_closed` |
| tail sampling baseline | 100% | 20% | 5% |
| zone spread | ScheduleAnyway | ScheduleAnyway | DoNotSchedule |
| PDBs / HPAs | removed | reduced | full |
| image tag | floating `dev` | `1.0.0-rc.N` | `1.0.0` |
| ServiceNow integration | off (manual path) | on (staging instance) | on |

The two rows that matter most are content capture and guardrail failure mode,
because they invert between non-prod and prod. In dev, a guardrail outage must
not block a developer; in prod, serving unscanned content is worse than
returning 503. That inversion is deliberate and is the reason the setting is an
overlay value rather than a code default.

---

## Directory notes

### `docker/`

One `Dockerfile` for all five images, selected by `--build-arg SERVICE=`.
Multi-stage, `CGO_ENABLED=0`, `gcr.io/distroless/static-debian12:nonroot`,
uid 65532, version/commit/date stamped into
`internal/version.{Version,Commit,BuildDate}`.

The `RUNTIME_VARIANT` build arg is the one thing worth knowing: `plain` (the
default, used for cluster images) is pure distroless with no shell and no
network client; `healthcheck` adds a static busybox purely so `docker compose`,
which runs probes *inside* the container, can perform an HTTP healthcheck.
Kubernetes needs neither, because the kubelet does the probing.

### `otel/`

Two tiers, per SPEC §4.4, with `README.md` explaining every processor and what
breaks if it is removed. The three lines most worth understanding:

* the agent tier's `loadbalancing` exporter with `routing_key: traceID` — the
  only reason tail sampling produces whole traces rather than fragments;
* `filter/drop-content-events` — the only thing keeping prompts out of the
  general trace backend;
* the `sum` connector's position *before* `tail_sampling` — the only reason
  chargeback reports 100% of spend rather than 5%.

`collector-agent.compose.yaml` is the compose variant: identical minus
`k8sattributes`, which needs a Kubernetes API to start.

### `prometheus/`

`slo.rules.yml` implements SPEC §5 with multi-window multi-burn-rate alerts.
The burn rates are computed for a 28-day window, not the usual 30:
2%/1h → 13.44×, 5%/6h → 5.6×, 10%/3d → 0.933×. Every alert carries `summary`,
`description`, `runbook_url` and a severity plus a `notify` label that
Alertmanager routes on.

`operational.rules.yml` covers the conditions that are actionable in
themselves: breaker open, provider degradation, retry storm, quota saturation,
cost anomaly (3σ plus a hard daily ceiling), telemetry completeness,
certificate expiry, and whether the monitoring itself is alive.

### `grafana/`

Provisioned datasources with pinned UIDs (dashboard JSON references them by
UID, so an auto-generated UID would break every panel on recreation), plus
three dashboards: gateway golden signals, fleet overview, cost and chargeback.

### `k8s/`

`base/` is production-shaped; the overlays adjust it. Notable choices:

* **No CPU limit on the gateway.** A CFS quota on a latency-sensitive proxy
  produces throttling that shows up as p99 overhead with no other symptom — and
  the latency SLO is measured on exactly that. Memory is limited; the HPA and
  the ResourceQuota bound total consumption.
* **Default-deny NetworkPolicy** plus one explicit allow per row of the SPEC §8
  table. The CIDRs in it match the Terraform address plan; they are not
  decorative.
* **`automountServiceAccountToken: false`** everywhere except the collector,
  which genuinely calls the Kubernetes API.
* **Collector configs are generated from `deploy/otel/`** rather than
  duplicated, which is why `kustomize build` needs
  `--load-restrictor=LoadRestrictionsNone` (the Makefile passes it).

---

## Cloud mapping

Every row is the same concept expressed three ways. The middle two columns are
what Terraform creates; the right-hand column is what the Go code actually
knows about — which is deliberately never a cloud service name.

| Concern | Azure | AWS | Cloud-neutral concept in the code |
|---|---|---|---|
| Edge API management | API Management (Premium, internal VNet) + AI policy set | Internal ALB with weighted target groups | The published v1 contract; canary weights (SPEC §7) |
| Compute | AKS (Azure CNI overlay + Cilium) | EKS (VPC CNI + Cilium/Calico) | `agentgate.runtime` = `aks` \| `eks` |
| Workload identity | Managed identity + federated credential on the SA subject | IRSA role + OIDC provider, `sub` condition | `attestation=workload-identity`; RFC 8693 exchange (SPEC §1.1) |
| Secrets | Key Vault (Premium, HSM) | Secrets Manager + KMS CMK | `AGENTGATE_VAULT_ADDR`; `SIGNING_KEY_PATH` as a projected file |
| Models | Azure OpenAI account + deployments | Bedrock (IAM model allowlist) | `provider` adapter + `backend` in a pool (SPEC §3.1) |
| Content safety | Azure AI Content Safety (`callout`) | Bedrock Guardrails (`callout`) | `GuardrailProvider` (SPEC §3.6) |
| Private connectivity | Private Endpoint + Private DNS zone | PrivateLink interface endpoint + private DNS | "no public egress" (SPEC §8 rows 2, 5, 6) |
| Internal DNS | Private DNS zone `agentgate.internal` | Route 53 private hosted zone | `*.agentgate.internal` (SPEC §8 row 1) |
| Egress control | Azure Firewall Premium, FQDN allowlist, UDR | Network Firewall, TLS_SNI/HTTP_HOST allowlist, route order | Inspecting egress proxy (SPEC §8 row 4) |
| Certificates | cert-manager + enterprise PKI ClusterIssuer | ACM Private CA (+ cert-manager) | 90-day rotation, 21-day expiry alert (SPEC §8) |
| Relational state | PostgreSQL Flexible Server (VNet-injected, Entra auth) | RDS PostgreSQL (private subnets, IAM auth) | `AGENTGATE_DB_DSN` — registry, promotions, rollups |
| Cache / buckets | Azure Cache for Redis (Premium, private endpoint) | ElastiCache Redis (TLS, IAM auth) | `AGENTGATE_REDIS_ADDR` — distributed buckets (SPEC §3.4), cache (§3.5) |
| Usage stream | Event Hubs (7-day retention) | Kinesis Data Streams (7-day retention) | Immutable `UsageRecord` stream (SPEC §6) |
| Metrics backend | Azure Monitor workspace (managed Prometheus) | Amazon Managed Prometheus | `agentgate_*` metrics; the SLI rules are byte-identical |
| Traces backend | Application Insights + Langfuse | X-Ray + Langfuse | OTLP exporter target — the collector config changes an endpoint, nothing else |
| Logs backend | Log Analytics | CloudWatch Logs | OTLP → Loki locally, managed backend in cloud |
| Telemetry private path | Azure Monitor Private Link Scope | PrivateLink endpoints for logs/monitoring/xray | Data-residency review (SPEC §8 row 6) |
| Alert delivery | Action Group → PagerDuty webhook | SNS topics `page` / `ticket` | `notify=page` \| `notify=ticket` labels on every rule |
| Change records | ServiceNow via egress proxy | ServiceNow via egress proxy | `AGENTGATE_SERVICENOW_URL`; empty disables and falls back to the manual path |
| On-prem inference | ExpressRoute + internal CA pinning | Direct Connect + internal CA pinning | `onprem-vllm` provider, priority-2 tier |

The point of the last column: nothing in `internal/` names a cloud service. A
backend is a provider adapter and a URL; a secret is a path; an identity is a
token. That is what makes the same binaries deployable in both columns, and it
is the property to protect when adding anything here.

---

## Conventions worth keeping

**One source of truth per fact.** The collector configs exist once, in
`otel/`, and are generated into ConfigMaps. The Prometheus rules exist once and
are loaded into Azure Monitor and AMP as-is. Copies drift; the copy that drifts
is always the one being read during the incident.

**Environment differences are values, not forks.** Every difference between dev
and prod is a variable in an overlay or a `tfvars` file. If something can only
be true in production, it has never been tested.

**Comments explain why, not what.** `readOnlyRootFilesystem: true` needs no
comment. The absence of a CPU limit on the gateway does.
