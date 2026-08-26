# Data sources and locals.

data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

data "aws_region" "current" {}

# The EKS cluster's OIDC thumbprint. Read from the issuer rather than
# hardcoded, because AWS rotates the root that signs it and a stale thumbprint
# silently breaks every IRSA assume-role at once.
data "tls_certificate" "eks_oidc" {
  url = aws_eks_cluster.this.identity[0].oidc[0].issuer
}

resource "random_string" "suffix" {
  length  = 6
  special = false
  upper   = false

  keepers = {
    environment = var.environment
    region      = var.region
    account     = var.account_id
  }
}

locals {
  base_name = "${var.name_prefix}-${var.environment}"
  suffix    = random_string.suffix.result

  az_count = length(var.availability_zones)

  # Subnet plan, derived rather than enumerated so adding a fourth AZ is a
  # one-line change instead of twelve CIDR calculations done by hand at
  # 23:00.
  #
  #   app       10.70.0.0/20,  10.70.16.0/20, 10.70.32.0/20   (EKS nodes, pods)
  #   data      10.70.48.0/22, 10.70.52.0/22, 10.70.56.0/22   (RDS, ElastiCache)
  #   public    10.70.60.0/24, 10.70.61.0/24, 10.70.62.0/24   (NAT gateways)
  #   firewall  10.70.63.0/26, 10.70.63.64/26, 10.70.63.128/26
  app_subnet_cidrs      = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, var.subnet_newbits.app, i)]
  data_subnet_cidrs     = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, var.subnet_newbits.data, i + 12)]
  public_subnet_cidrs   = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, var.subnet_newbits.public, i + 60)]
  firewall_subnet_cidrs = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, var.subnet_newbits.firewall, i + 252)]

  # Kubernetes ServiceAccounts that receive an IRSA role. Identical list to
  # deploy/terraform/azure/data.tf and to
  # deploy/k8s/base/serviceaccounts.yaml — if the three disagree, pods get
  # tokens nothing accepts.
  workload_identities = {
    gateway = {
      sa          = "gateway"
      description = "Model traffic plane. Invokes Bedrock and reads its own secrets."
    }
    controlplane = {
      sa          = "controlplane"
      description = "Trust plane. Reads the token signing key. Most privileged identity in the platform."
    }
    fleetview = {
      sa          = "fleetview"
      description = "Truth plane. Read-only against the database, metrics and the usage stream."
    }
    guardrails = {
      sa          = "guardrails"
      description = "Content safety callout."
    }
    "otel-collector" = {
      sa          = "otel-collector"
      description = "Telemetry pipeline. Writes to CloudWatch, X-Ray and AMP."
    }
  }

  k8s_namespace = "agentgate"

  # Interface endpoints (PrivateLink). Every one of these is a service the
  # workloads would otherwise reach over the internet through the NAT gateway;
  # with the endpoint in place, the traffic never leaves the VPC and the
  # firewall's allowlist does not have to carry an AWS API surface.
  interface_endpoints = [
    "bedrock-runtime",
    "secretsmanager",
    "kms",
    "sts",
    "logs",
    "monitoring",
    "xray",
    "ecr.api",
    "ecr.dkr",
    "kinesis-streams",
    "elasticloadbalancing",
    "ec2",
    "aps-workspaces",
  ]

  common_tags = merge(var.tags, {
    environment = var.environment
    region      = var.region
  })
}
