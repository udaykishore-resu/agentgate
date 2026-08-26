# Data sources and derived locals.
#
# Everything that is *looked up* rather than created lives here, so that the
# boundary between "what this configuration owns" and "what it depends on" is
# a single file rather than something a reader has to infer.

data "azurerm_client_config" "current" {}

data "azurerm_subscription" "current" {}

# The AKS cluster's OIDC issuer is the trust anchor for every federated
# credential in identity.tf. It is read back from the created cluster rather
# than constructed, because the URL contains a per-cluster GUID that only
# exists after creation.
data "azurerm_kubernetes_cluster" "this" {
  name                = azurerm_kubernetes_cluster.this.name
  resource_group_name = azurerm_resource_group.this.name

  depends_on = [azurerm_kubernetes_cluster.this]
}

# Available AKS versions in this region. Used only by the validation below:
# pinning a version that the region has already retired produces a confusing
# failure several minutes into an apply.
data "azurerm_kubernetes_service_versions" "current" {
  location        = var.location
  version_prefix  = var.kubernetes_version
  include_preview = false
}

check "kubernetes_version_available" {
  assert {
    condition     = length(data.azurerm_kubernetes_service_versions.current.versions) > 0
    error_message = "kubernetes_version ${var.kubernetes_version} is not offered in ${var.location}. Check `az aks get-versions` before pinning."
  }
}

resource "random_string" "suffix" {
  # Globally-unique names (Key Vault, storage, Azure OpenAI) need a suffix that
  # is stable across applies. `keepers` deliberately references only values
  # that, if changed, genuinely mean a different environment.
  length  = 6
  special = false
  upper   = false

  keepers = {
    environment  = var.environment
    location     = var.location
    subscription = var.subscription_id
  }
}

locals {
  # `agentgate-prod` style base name; individual resources add their own type
  # prefix so a reader can tell what something is from its name alone.
  base_name = "${var.name_prefix}-${var.environment}"
  suffix    = random_string.suffix.result

  # Kubernetes ServiceAccounts that receive a federated identity. The map key
  # is the identity name; `sa` is the subject the federated credential trusts.
  # This is the exact list in deploy/k8s/base/serviceaccounts.yaml — the two
  # files must agree or the pod gets a token nothing accepts.
  workload_identities = {
    gateway = {
      sa          = "system:serviceaccount:agentgate:gateway"
      description = "Model traffic plane. Reads Key Vault secrets and calls Azure OpenAI."
    }
    controlplane = {
      sa          = "system:serviceaccount:agentgate:controlplane"
      description = "Trust plane. Reads the token signing key. Most privileged identity in the platform."
    }
    fleetview = {
      sa          = "system:serviceaccount:agentgate:fleetview"
      description = "Truth plane. Read-only against the database and metrics."
    }
    guardrails = {
      sa          = "system:serviceaccount:agentgate:guardrails"
      description = "Content safety callout. Calls the managed content-safety endpoint."
    }
    "otel-collector" = {
      sa          = "system:serviceaccount:agentgate:otel-collector"
      description = "Telemetry pipeline. Writes to Azure Monitor and the usage stream."
    }
  }

  # Private DNS zones required for the private-endpoint paths in SPEC §8. Every
  # PaaS dependency gets one; without the zone the private endpoint exists but
  # the name still resolves publicly, which is the single most common way a
  # "private" deployment turns out not to be.
  private_dns_zones = {
    postgres  = "privatelink.postgres.database.azure.com"
    redis     = "privatelink.redis.cache.windows.net"
    keyvault  = "privatelink.vaultcore.azure.net"
    openai    = "privatelink.openai.azure.com"
    eventhub  = "privatelink.servicebus.windows.net"
    monitor   = "privatelink.monitor.azure.com"
    oms       = "privatelink.oms.opinsights.azure.com"
    ods       = "privatelink.ods.opinsights.azure.com"
    agentsvc  = "privatelink.agentsvc.azure-automation.net"
    blob      = "privatelink.blob.core.windows.net"
    appconfig = "privatelink.azconfig.io"
  }

  # The frozen v1 contract, imported into APIM when present.
  openapi_contract_path = "${path.module}/../../../api/openapi/gateway.v1.yaml"

  common_tags = merge(var.tags, {
    environment = var.environment
    location    = var.location
    # Written by CI so an unexpected resource can be traced back to the run
    # that created it.
    terraform_module = "deploy/terraform/azure"
  })
}
