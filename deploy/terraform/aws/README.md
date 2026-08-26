# AgentGate on AWS

Terraform for one AgentGate environment on AWS. One directory, three
environments, selected by `-var-file` and a backend config — no workspaces, no
copied directories.

The file split matches `deploy/terraform/azure/` exactly, so the same concern
lives in the same filename in both clouds. That includes `apim.tf`, which on
AWS contains an ALB rather than an API Management instance.

## What this creates

| File | Contents |
|---|---|
| `main.tf` | KMS keys, EKS cluster and node groups, RDS PostgreSQL, ElastiCache Redis, Secrets Manager containers |
| `network.tf` | VPC, four subnet tiers, NAT gateways, AWS Network Firewall + FQDN allowlist, route tables, PrivateLink endpoints, Route 53 private hosted zone, security groups, flow logs |
| `identity.tf` | OIDC provider, IRSA roles and policies, cluster/node roles, EKS access entries |
| `observability.tf` | CloudWatch log groups, Amazon Managed Prometheus (+ the repo's rule files), X-Ray, Kinesis usage stream, SNS topics, platform alarms |
| `apim.tf` | Internal ALB, target groups (including the migration weight), ACM private certificate, DNS records, access logs |
| `data.tf` | Data sources, locals, subnet arithmetic, the workload-identity map |
| `variables.tf` / `outputs.tf` / `versions.tf` | Inputs, outputs, provider pins |

## Why EKS and not ECS Fargate

Same reasoning as the Azure side and it lands the same way. SPEC §4.4 needs a
node-local collector feeding a trace-ID-sharded pool; Fargate has no node to
put a DaemonSet on, and the sidecar equivalent multiplies collector instances
by task count and destroys the batching that makes the first tier worth having.
SPEC §8 needs default-deny east-west with a per-path allow, which is a
NetworkPolicy, which needs a CNI.

Fargate remains a good host for the *agents*.

## Why an ALB and not API Gateway

API Gateway is the closer analogue to Azure API Management, and it breaks the
product. Both REST and HTTP APIs cap integration timeouts at 29 seconds and
buffer responses. SPEC §2.5 requires SSE with a 15-second heartbeat and
generations that run for minutes; SPEC §5 puts an SLO on time-to-first-token.
API Gateway would truncate long completions and make TTFT a function of its
buffer size.

The functions API Gateway would have provided are placed where they can be done
without breaking streaming:

| Function | Where it lives instead |
|---|---|
| Subscription / consumer management | Control plane registration (SPEC §1.3) |
| Token and request limiting | Gateway quota system (SPEC §3.4) — token-aware, not request-aware |
| Canary weighting | ALB weighted target groups (SPEC §7) |

The one thing genuinely lost is a managed developer portal. That is a
documentation problem, not a traffic problem.

## Prerequisites

* Terraform ≥ 1.9, AWS CLI ≥ 2.15
* An IAM principal that can create IAM roles and KMS keys — the IRSA roles in
  `identity.tf` cannot be created by a principal without `iam:CreateRole`
* State bucket, created out of band (S3 native locking is used, so no DynamoDB
  table is needed):

```bash
aws s3api create-bucket --bucket agentgate-tfstate --region eu-west-1 \
  --create-bucket-configuration LocationConstraint=eu-west-1
aws s3api put-bucket-versioning --bucket agentgate-tfstate \
  --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption --bucket agentgate-tfstate \
  --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
aws s3api put-public-access-block --bucket agentgate-tfstate \
  --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
```

* **Bedrock model access must be requested and granted before the first apply.**
  It is per account, per region, per model, and it is not instant. The IAM
  policy in `identity.tf` grants `bedrock:InvokeModel` on the model ARNs, but
  IAM permission does not imply model access — an ungranted model returns
  `AccessDeniedException` with a message that reads like an IAM problem and is
  not one.
* An ACM Private CA, with its ARN in `acm_private_ca_arn`. The internal names
  never resolve publicly, so no public CA can issue for them.
* An IAM role named `PlatformAIAdmin`, which receives cluster admin through the
  EKS access entry in `identity.tf`.

## Deploying

```bash
cd deploy/terraform/aws

cat > backends/prod.hcl <<'EOF'
bucket = "agentgate-tfstate"
key    = "aws/prod.tfstate"
region = "eu-west-1"
EOF

terraform init -backend-config=backends/prod.hcl
terraform plan  -var-file=../../../env/prod.aws.tfvars -out=prod.tfplan
terraform apply prod.tfplan
```

Or from the repository root: `make tf-aws-plan ENV=prod`.

Expect 25–40 minutes: EKS control plane ~10, RDS multi-AZ ~15, Network Firewall
endpoints ~10, mostly in parallel.

## After the apply

```bash
# 1. Credentials (API server is private — run from inside the VPC).
aws eks update-kubeconfig --region eu-west-1 --name eks-agentgate-prod

# 2. Patch the IRSA role ARNs into the ServiceAccounts.
terraform output -json irsa_role_arns

# 3. Platform prerequisites the manifests assume are present:
#    - external-secrets operator, configured for the aws-secretsmanager store
#    - cert-manager + enterprise-pki ClusterIssuer, or the ACM certificate on
#      the ALB and no in-cluster Certificate at all
#    - AWS Load Balancer Controller, or a Gateway API controller with
#      GatewayClass agentgate-internal
#    - Cilium or Calico: the VPC CNI alone does not enforce the
#      NetworkPolicies in deploy/k8s/base/networkpolicy.yaml
#    - prometheus-adapter or KEDA, publishing agentgate_gateway_inflight

# 4. Apply the overlay, with the AWS annotations enabled.
make k8s-prod
```

Note the CNI point: the AWS VPC CNI does **not** enforce NetworkPolicy on its
own. Without Cilium or Calico, every policy in `deploy/k8s/base/networkpolicy.yaml`
is accepted by the API server and enforced by nothing — which looks exactly
like a working default-deny right up until someone tests it.

## Things that are deliberate

**Private DNS on every interface endpoint.** Without `private_dns_enabled`, the
AWS SDK resolves the public service name to a public IP and the traffic leaves
through the NAT gateway. PrivateLink then exists in the diagram and not on the
wire — the same failure mode as an Azure private endpoint without its DNS zone.

**Data subnets have no route out.** RDS and ElastiCache sit in a route table
with no default route at all. A security group rule is a filter; an absent
route is a statement.

**Firewall on the egress path, not beside it.** Public subnets route `0.0.0.0/0`
to the Network Firewall endpoint, and only the firewall subnets route to the
IGW. This ordering is what makes the FQDN allowlist a control rather than
advice.

**`STRICT_ORDER` with `aws:drop_established`.** The permissive default
(`aws:pass`) would let unmatched traffic through and make the allowlist
decorative.

**No database password anywhere.** `manage_master_user_password` puts the
master credential in Secrets Manager under a key this account owns, and the
applications connect with `rds-db:connect` IAM auth against per-service
database users.

**Bedrock access is an explicit model allowlist.** `bedrock:InvokeModel` is
granted on named model ARNs. A pool referencing a model outside
`bedrock_model_ids` fails at the IAM boundary rather than being quietly
billable.

**`prevent_destroy` on RDS, `delete_protection` on the firewall.** Removing
either is a reviewed commit. The friction is the feature.

## Destroying a non-production environment

```bash
terraform destroy -var-file=../../../env/dev.aws.tfvars
```

It will refuse on RDS (`prevent_destroy`) and on the Network Firewall
(`delete_protection`). Both are intentional. Remove the guards in a reviewed
commit first.

## Cost notes

Roughly in order, for a production environment:

1. **Bedrock invocation** — dominates once real traffic arrives, which is why
   the chargeback plane exists.
2. **NAT gateways** — three of them, charged hourly plus per GB. The
   PrivateLink endpoints exist partly to keep AWS API traffic off them.
3. **AWS Network Firewall** — charged per endpoint-hour plus per GB, three
   endpoints for AZ redundancy.
4. **RDS multi-AZ** — doubles the instance cost for a standby that is idle
   until the day it is not.
5. **EKS node groups** — usually the first place someone tries to save money
   and rarely the largest line.
