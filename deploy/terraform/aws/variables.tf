variable "account_id" {
  description = "AWS account for this environment. One account per environment: it is the only blast-radius boundary AWS enforces without effort."
  type        = string

  validation {
    condition     = can(regex("^[0-9]{12}$", var.account_id))
    error_message = "account_id must be a 12-digit AWS account id."
  }
}

variable "region" {
  description = "Primary region. Data residency for the client is EU, so this must stay within the approved EU region list."
  type        = string
  default     = "eu-west-1"

  validation {
    condition     = contains(["eu-west-1", "eu-central-1", "eu-north-1"], var.region)
    error_message = "region must be an approved EU region: eu-west-1, eu-central-1 or eu-north-1."
  }
}

variable "environment" {
  description = "Environment name. Flows into resource names, tags, and deployment.environment.name on every signal."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of dev, staging, prod."
  }
}

variable "name_prefix" {
  description = "Prefix for every resource name."
  type        = string
  default     = "agentgate"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.name_prefix))
    error_message = "name_prefix must be 3-16 lowercase alphanumeric characters or hyphens, starting with a letter."
  }
}

variable "tags" {
  description = "Tags applied to every resource. cost_center is mandatory for the same reason it is mandatory on an agent token."
  type        = map(string)
  default = {
    application = "agentgate"
    owner       = "platform-ai@client.example"
    cost_center = "CC-0001"
  }

  validation {
    condition     = contains(keys(var.tags), "cost_center")
    error_message = "tags must include cost_center."
  }
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

variable "vpc_cidr" {
  description = "VPC address space. Kept distinct from the Azure plan (10.60.0.0/16) so the two clouds can be peered to the same on-prem estate without renumbering."
  type        = string
  default     = "10.70.0.0/16"
}

variable "availability_zones" {
  description = "AZs to spread across. Three is the minimum for a quorum-shaped failure domain and matches the topology spread constraints in deploy/k8s."
  type        = list(string)
  default     = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]

  validation {
    condition     = length(var.availability_zones) >= 3
    error_message = "at least three availability zones are required."
  }
}

variable "subnet_newbits" {
  description = "Bits added to the VPC prefix when carving subnets. 4 gives /20 app subnets (4090 usable), 6 gives /22 data subnets, 8 gives /24 edge subnets."
  type = object({
    app      = number
    data     = number
    public   = number
    firewall = number
  })
  default = {
    app      = 4
    data     = 6
    public   = 8
    firewall = 10
  }
}

variable "onprem_inference_cidr" {
  description = "On-prem inference range reached over Direct Connect (SPEC §8 row 3)."
  type        = string
  default     = "10.10.20.0/24"
}

variable "egress_allowed_fqdns" {
  description = <<-EOT
    FQDN allowlist enforced by AWS Network Firewall (SPEC §8 row 4). Anything
    not listed is dropped. A leading dot means "and all subdomains", which is
    Suricata/Network Firewall syntax, not a typo.
  EOT
  type        = list(string)
  default = [
    ".amazonaws.com",
    "ghcr.io",
    ".pkg.github.com",
    "registry.k8s.io",
    ".pkg.dev",
    "client.service-now.com",
  ]
}

# ---------------------------------------------------------------------------
# Compute
# ---------------------------------------------------------------------------

variable "kubernetes_version" {
  description = "EKS control-plane version. Pinned so an upgrade is a reviewed change with its own window."
  type        = string
  default     = "1.31"
}

variable "node_groups" {
  description = <<-EOT
    Managed node groups. `system` carries CoreDNS and the CSI drivers;
    `workload` carries AgentGate. They are separate so a tenant burst cannot
    starve cluster services, and the workload group is memory-heavy relative
    to CPU because the gateway is IO-bound while the collector holds traces in
    memory for tail sampling.
  EOT
  type = map(object({
    instance_types = list(string)
    min_size       = number
    max_size       = number
    desired_size   = number
    capacity_type  = string
  }))
  default = {
    system = {
      instance_types = ["m6i.xlarge"]
      min_size       = 3
      max_size       = 6
      desired_size   = 3
      capacity_type  = "ON_DEMAND"
    }
    workload = {
      instance_types = ["r6i.xlarge"]
      min_size       = 3
      max_size       = 30
      desired_size   = 3
      # On-demand, not spot. A reclaimed node cancels every stream its gateway
      # pods are carrying, and the saving is small next to model spend.
      capacity_type = "ON_DEMAND"
    }
  }
}

# ---------------------------------------------------------------------------
# State stores
# ---------------------------------------------------------------------------

variable "rds" {
  description = "RDS PostgreSQL sizing and retention."
  type = object({
    instance_class          = string
    allocated_storage       = number
    max_allocated_storage   = number
    engine_version          = string
    multi_az                = bool
    backup_retention_period = number
    deletion_protection     = bool
  })
  default = {
    instance_class          = "db.m6g.xlarge"
    allocated_storage       = 256
    max_allocated_storage   = 1024
    engine_version          = "16.6"
    multi_az                = true
    backup_retention_period = 35
    deletion_protection     = true
  }
}

variable "elasticache" {
  description = "ElastiCache Redis sizing. Cluster mode is off: the rate-limit Lua script is atomic per key and does not need cross-slot transactions, so replication is simpler and failover is faster."
  type = object({
    node_type          = string
    num_cache_clusters = number
    engine_version     = string
  })
  default = {
    node_type          = "cache.m7g.large"
    num_cache_clusters = 2
    engine_version     = "7.1"
  }
}

# ---------------------------------------------------------------------------
# Models
# ---------------------------------------------------------------------------

variable "bedrock_model_ids" {
  description = "Bedrock models the gateway is permitted to invoke. This is an allowlist in IAM, not documentation: a model that is not listed cannot be called even if a pool references it."
  type        = list(string)
  default = [
    "anthropic.claude-3-5-haiku-20241022-v1:0",
    "anthropic.claude-3-5-sonnet-20241022-v2:0",
    "amazon.titan-embed-text-v2:0",
  ]
}

# ---------------------------------------------------------------------------
# Edge
# ---------------------------------------------------------------------------

variable "internal_domain" {
  description = "Private hosted zone for the internal service names (SPEC §8 row 1)."
  type        = string
  default     = "agentgate.internal"
}

variable "acm_private_ca_arn" {
  description = "ARN of the enterprise private CA. Certificates come from enterprise PKI (SPEC §8), not from a public issuer, because these names never resolve publicly."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------

variable "log_retention_days" {
  description = "CloudWatch log retention."
  type        = number
  default     = 90

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731], var.log_retention_days)
    error_message = "log_retention_days must be one of the values CloudWatch accepts."
  }
}

variable "usage_stream_shards" {
  description = "Kinesis shard count for the UsageRecord stream (SPEC §6). Each shard takes 1000 records/s; size for peak request rate, not average."
  type        = number
  default     = 4
}
