variable "subscription_id" {
  description = "Azure subscription that hosts this AgentGate environment. One subscription per environment keeps blast radius, quota and cost reporting aligned."
  type        = string
}

variable "tenant_id" {
  description = "Entra ID tenant."
  type        = string
}

variable "environment" {
  description = "Environment name. Flows into resource names, tags, and deployment.environment.name on every signal."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of dev, staging, prod — these are the three the promotion gate knows about."
  }
}

variable "location" {
  description = "Primary Azure region. Data residency for the client is EU, so this must stay within the approved EU region list."
  type        = string
  default     = "westeurope"

  validation {
    condition     = contains(["westeurope", "northeurope", "swedencentral"], var.location)
    error_message = "location must be an approved EU region: westeurope, northeurope or swedencentral."
  }
}

variable "name_prefix" {
  description = "Prefix for every resource name. Kept short because several Azure resource types cap at 24 characters."
  type        = string
  default     = "agentgate"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{2,11}$", var.name_prefix))
    error_message = "name_prefix must be 3-12 lowercase alphanumeric characters starting with a letter."
  }
}

variable "tags" {
  description = "Tags applied to every resource. cost_center is mandatory: it is the same chargeback key the platform enforces on agents, applied to the platform itself."
  type        = map(string)
  default = {
    application = "agentgate"
    owner       = "platform-ai@client.example"
    cost_center = "CC-0001"
    managed_by  = "terraform"
  }

  validation {
    condition     = contains(keys(var.tags), "cost_center")
    error_message = "tags must include cost_center — the platform cannot require attribution of its tenants and exempt itself."
  }
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

variable "vnet_cidr" {
  description = "VNet address space. Must match the plan documented in deploy/k8s/base/networkpolicy.yaml, because the NetworkPolicies encode these ranges."
  type        = string
  default     = "10.60.0.0/16"
}

variable "subnet_cidrs" {
  description = "Subnet allocation inside the VNet."
  type = object({
    aks               = string
    private_endpoints = string
    apim              = string
    firewall          = string
    firewall_mgmt     = string
    postgres          = string
  })
  default = {
    aks               = "10.60.0.0/20"
    private_endpoints = "10.60.16.0/24"
    apim              = "10.60.17.0/24"
    firewall          = "10.60.18.0/26"
    firewall_mgmt     = "10.60.18.64/26"
    postgres          = "10.60.20.0/24"
  }
}

variable "onprem_inference_cidr" {
  description = "On-prem inference range reached over ExpressRoute (SPEC §8 row 3)."
  type        = string
  default     = "10.10.20.0/24"
}

variable "egress_allowed_fqdns" {
  description = <<-EOT
    FQDN allowlist for the egress firewall (SPEC §8 row 4). Everything not on
    this list is denied, including anything a future dependency decides to
    phone home to. Adding an entry is a third-party risk review, which is the
    point: the list is the control, not the paperwork about the list.
  EOT
  type        = list(string)
  default = [
    # Image pulls and signature verification.
    "mcr.microsoft.com",
    "*.data.mcr.microsoft.com",
    "ghcr.io",
    "*.pkg.github.com",
    # Entra ID token endpoints for workload identity federation.
    "login.microsoftonline.com",
    "*.identity.azure.net",
    # AKS control-plane and node bootstrap.
    "management.azure.com",
    "packages.microsoft.com",
    "acs-mirror.azureedge.net",
    # ServiceNow, for the promotion gate's change records.
    "client.service-now.com",
  ]
}

# ---------------------------------------------------------------------------
# Compute
# ---------------------------------------------------------------------------

variable "kubernetes_version" {
  description = "AKS control-plane version. Pinned rather than tracking latest, so a cluster upgrade is a reviewed change with its own window."
  type        = string
  default     = "1.31"
}

variable "system_node_pool" {
  description = "System node pool: CoreDNS, metrics-server, the CSI drivers. Kept separate from workloads so a tenant burst cannot starve cluster services."
  type = object({
    vm_size    = string
    node_count = number
    zones      = list(string)
  })
  default = {
    vm_size    = "Standard_D4ds_v5"
    node_count = 3
    zones      = ["1", "2", "3"]
  }
}

variable "workload_node_pool" {
  description = <<-EOT
    Workload node pool for the AgentGate services. Memory-heavy relative to
    CPU because the gateway is IO-bound (waiting on providers) while the
    gateway-tier collector holds up to 200k traces in memory for tail
    sampling.
  EOT
  type = object({
    vm_size   = string
    min_count = number
    max_count = number
    zones     = list(string)
  })
  default = {
    vm_size   = "Standard_E4ds_v5"
    min_count = 3
    max_count = 30
    zones     = ["1", "2", "3"]
  }
}

# ---------------------------------------------------------------------------
# State stores
# ---------------------------------------------------------------------------

variable "postgres" {
  description = "Azure Database for PostgreSQL flexible server sizing and retention."
  type = object({
    sku_name              = string
    storage_mb            = number
    version               = string
    backup_retention_days = number
    geo_redundant_backup  = bool
    high_availability     = bool
  })
  default = {
    sku_name              = "GP_Standard_D4ds_v5"
    storage_mb            = 262144
    version               = "16"
    backup_retention_days = 35
    geo_redundant_backup  = true
    high_availability     = true
  }
}

variable "redis" {
  description = "Azure Cache for Redis sizing. Premium is required, not preferred: only Premium supports private endpoints and zone redundancy, and the rate-limit buckets must not be reachable from anywhere public."
  type = object({
    sku_name = string
    family   = string
    capacity = number
  })
  default = {
    sku_name = "Premium"
    family   = "P"
    capacity = 1
  }
}

# ---------------------------------------------------------------------------
# Models
# ---------------------------------------------------------------------------

variable "openai_deployments" {
  description = "Azure OpenAI model deployments. Capacity is in thousands of tokens per minute and is the real quota the pool weights are balanced against."
  type = list(object({
    name          = string
    model_name    = string
    model_version = string
    sku_name      = string
    capacity      = number
  }))
  default = [
    {
      name          = "gpt-4o-mini"
      model_name    = "gpt-4o-mini"
      model_version = "2024-07-18"
      sku_name      = "DataZoneStandard"
      capacity      = 300
    },
    {
      name          = "gpt-4o"
      model_name    = "gpt-4o"
      model_version = "2024-11-20"
      sku_name      = "DataZoneStandard"
      capacity      = 100
    },
    {
      name          = "text-embedding-3-large"
      model_name    = "text-embedding-3-large"
      model_version = "1"
      sku_name      = "Standard"
      capacity      = 120
    },
  ]
}

# ---------------------------------------------------------------------------
# Edge
# ---------------------------------------------------------------------------

variable "apim_sku" {
  description = "API Management SKU. Premium is required for VNet injection in internal mode and for zone redundancy; Developer is acceptable in dev only and carries no SLA."
  type        = string
  default     = "Premium_1"

  validation {
    condition     = can(regex("^(Developer_1|Premium_[1-9][0-9]?)$", var.apim_sku))
    error_message = "apim_sku must be Developer_1 (dev only) or Premium_N — other SKUs cannot be VNet-injected in internal mode."
  }
}

variable "apim_publisher" {
  description = "Publisher identity shown on the developer portal and used for APIM notification email."
  type = object({
    name  = string
    email = string
  })
  default = {
    name  = "Client Platform AI"
    email = "platform-ai@client.example"
  }
}

variable "apim_token_limit_per_minute" {
  description = "Tokens-per-minute ceiling applied by the APIM AI policy set as a coarse outer guard. The gateway's per-agent quota (SPEC §3.4) is the real control; this only stops a runaway consumer from reaching the gateway at all."
  type        = number
  default     = 500000
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------

variable "log_retention_days" {
  description = "Log Analytics retention. 90 days is the client's operational minimum; regulated content has its own, longer retention on a separate workspace."
  type        = number
  default     = 90

  validation {
    condition     = var.log_retention_days >= 30 && var.log_retention_days <= 730
    error_message = "log_retention_days must be between 30 and 730."
  }
}

variable "usage_stream_partitions" {
  description = "Event Hubs partition count for the UsageRecord stream (SPEC §6). Partitions cannot be reduced later, so this is sized for peak, not for today."
  type        = number
  default     = 8
}
