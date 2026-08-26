# AgentGate on Azure

Terraform for one AgentGate environment on Azure. One directory, three
environments, selected by `-var-file` and a backend config — not by copied
directories and not by workspaces, because a workspace-per-environment hides
which state a plan is about to touch.

## What this creates

| File | Contents |
|---|---|
| `main.tf` | Resource group, AKS, PostgreSQL flexible server, Redis, Key Vault, Azure OpenAI + deployments |
| `network.tf` | VNet, subnets, NSGs, Azure Firewall + FQDN allowlist, route table, private DNS zones, private endpoints |
| `identity.tf` | User-assigned managed identities, federated credentials, RBAC grants |
| `observability.tf` | Log Analytics, Application Insights, managed Prometheus, Event Hubs usage stream, diagnostic settings, platform alerts |
| `apim.tf` | API Management (internal VNet mode), the gateway API, the AI policy set, product and subscription model |
| `data.tf` | Data sources, locals, the workload-identity map |
| `variables.tf` / `outputs.tf` / `versions.tf` | Inputs, outputs, provider pins |

## Why AKS and not Container Apps

Container Apps is cheaper and simpler and was the first choice. It was rejected
for three reasons, all of which come from the spec:

1. **Telemetry topology.** SPEC §4.4 needs a node-local collector tier feeding a
   trace-ID-sharded gateway tier. A DaemonSet is the shape of tier one.
   Container Apps has no per-node placement.
2. **Network policy granularity.** SPEC §8 requires default-deny east-west with
   an explicit allow per reviewed path. Container Apps gives app-level
   ingress/egress controls, not the pod-to-pod granularity that table needs.
3. **Identity granularity.** Five ServiceAccounts, five managed identities, five
   different vault grants, each bound to a subject. Container Apps binds
   identity per app — workable, but the "one identity per workload, auditable"
   story is materially weaker, and identity is the thing this platform exists
   to get right.

Container Apps remains the right host for the *agents* — which is why
`agentgate.runtime` includes `aca` in SPEC §4.1. It is the control plane that
needs Kubernetes.

## Prerequisites

* Terraform ≥ 1.9, Azure CLI ≥ 2.60
* An Entra ID account with `Owner` (or `Contributor` + `User Access
  Administrator`) on the target subscription — the RBAC assignments in
  `identity.tf` need the ability to grant roles
* A state storage account, created out of band:

```bash
az group create -n rg-agentgate-tfstate -l westeurope
az storage account create -n stagentgatetfstate -g rg-agentgate-tfstate \
  -l westeurope --sku Standard_ZRS --min-tls-version TLS1_2 \
  --allow-blob-public-access false
az storage container create -n tfstate --account-name stagentgatetfstate --auth-mode login
az storage account blob-service-properties update -n stagentgatetfstate \
  -g rg-agentgate-tfstate --enable-versioning true --enable-delete-retention true \
  --delete-retention-days 30
```

* Quota for the Azure OpenAI models in `openai_deployments`. Deployment
  capacity is the constraint that bites first in a new subscription and the
  request takes days, so raise it before you plan.
* Registered resource providers: `Microsoft.ContainerService`,
  `Microsoft.CognitiveServices`, `Microsoft.DBforPostgreSQL`, `Microsoft.Cache`,
  `Microsoft.ApiManagement`, `Microsoft.Monitor`, `Microsoft.EventHub`,
  `Microsoft.Network`, `Microsoft.KeyVault`.

## Deploying

```bash
cd deploy/terraform/azure

# One backend config per environment; state keys never collide.
cat > backends/prod.hcl <<'EOF'
resource_group_name  = "rg-agentgate-tfstate"
storage_account_name = "stagentgatetfstate"
container_name       = "tfstate"
key                  = "azure/prod.tfstate"
EOF

terraform init -backend-config=backends/prod.hcl
terraform plan  -var-file=../../../env/prod.azure.tfvars -out=prod.tfplan
terraform apply prod.tfplan
```

Or from the repository root: `make tf-azure-plan ENV=prod`.

**Expect the first apply to take about an hour.** API Management alone is
~45 minutes. It is not stuck.

## After the apply

Terraform stops at the cluster boundary. The Kubernetes layer is applied
separately so that a cluster can be fixed without a successful `terraform
apply` standing in the way.

```bash
# 1. Credentials (API server is private — run this from inside the VNet).
az aks get-credentials -g rg-agentgate-prod -n aks-agentgate-prod

# 2. Patch the workload identity client ids into the ServiceAccounts.
terraform output -json workload_identity_client_ids

# 3. Platform prerequisites the manifests assume are present:
#    - external-secrets operator          (ExternalSecret / SecretStore)
#    - cert-manager + enterprise-pki ClusterIssuer (Certificate)
#    - a Gateway API controller with GatewayClass agentgate-internal
#    - prometheus-adapter or KEDA, publishing agentgate_gateway_inflight
#      as a custom metric for the HPA

# 4. Apply the overlay.
make k8s-prod
```

## Things that are deliberate

**No public IP anywhere except the firewall.** Every PaaS dependency is reached
through a private endpoint with a matching private DNS zone. The zone is the
part people forget: a private endpoint without its zone still resolves to the
public name, and the deployment is "private" only in the diagram.

**`outbound_type = "userDefinedRouting"`.** The cluster does not provision its
own egress. Combined with the route table, this makes the firewall the only
path out — which is what turns the FQDN allowlist from advice into a control.

**No password on PostgreSQL.** `password_auth_enabled = false`. The control
plane connects with its workload identity. There is no credential to rotate,
leak, or find in a connection string.

**`local_auth_enabled = false` on Azure OpenAI.** API keys are disabled
outright. The gateway authenticates with its managed identity, so a leaked
manifest or an exfiltrated environment variable yields nothing.

**`prevent_destroy` on Key Vault, PostgreSQL, and APIM.** A destroyed Key Vault
takes the token signing key with it and every agent identity becomes
unverifiable. Removing these guards is a deliberate, reviewed act.

**Diagnostic settings on Key Vault are not optional.** `AuditEvent` is the
record of who read the signing key. It is the evidence that answers "could
anyone have minted an agent identity", and it must exist in every environment,
including dev.

## Destroying a non-production environment

```bash
terraform destroy -var-file=../../../env/dev.azure.tfvars
```

`prevent_destroy` will refuse on Key Vault, PostgreSQL and APIM. That is
working as intended. Remove the lifecycle blocks in a commit, get it reviewed,
then destroy — the friction is the feature.

## Cost notes

The expensive line items in a production environment, roughly in order:

1. **API Management Premium** — required for internal VNet mode. Developer SKU
   is one tenth the cost, has no SLA, and cannot be zone-redundant. Acceptable
   in dev only.
2. **Azure Firewall Premium** — required for TLS inspection on the third-party
   egress path (SPEC §8 row 4). Standard is cheaper and cannot inspect.
3. **Azure OpenAI provisioned capacity** — dominates everything above it once
   real traffic arrives, which is exactly why the chargeback plane exists.
4. **AKS node pools** — the cheapest thing on this list, and usually the first
   place someone tries to save money.
