# Workload identity federation (SPEC §1.1 mode 1, SPEC §10).
#
# One user-assigned managed identity per Kubernetes ServiceAccount, each with a
# federated credential whose subject is that exact ServiceAccount. The chain:
#
#   pod -> projected SA token (issued by the cluster's OIDC issuer)
#       -> Entra ID validates issuer + subject + audience
#       -> Entra ID issues an access token for the managed identity
#       -> Azure resource authorises via RBAC on that identity
#
# No secret exists at any step. Rotation is automatic because the projected
# token has a one-hour lifetime and is renewed by the kubelet.
#
# The grants below are deliberately asymmetric. The gateway can call models and
# read its own secrets; only the control plane can touch the signing key. A
# gateway compromise is bad; a gateway compromise that could mint agent
# identities would be unrecoverable.

resource "azurerm_user_assigned_identity" "workload" {
  for_each = local.workload_identities

  name                = "id-${local.base_name}-${each.key}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tags = merge(local.common_tags, {
    purpose = each.value.description
  })
}

resource "azurerm_federated_identity_credential" "workload" {
  for_each = local.workload_identities

  name                = "fic-${each.key}"
  resource_group_name = azurerm_resource_group.this.name
  parent_id           = azurerm_user_assigned_identity.workload[each.key].id
  # Fixed by the OIDC spec for Kubernetes SA token exchange.
  audience = ["api://AzureADTokenExchange"]
  issuer   = data.azurerm_kubernetes_cluster.this.oidc_issuer_url
  # The subject is what binds this cloud identity to one specific
  # ServiceAccount in one specific namespace. A pod in another namespace
  # presenting its own token fails here, not later.
  subject = each.value.sa
}

# ---------------------------------------------------------------------------
# Key Vault grants
# ---------------------------------------------------------------------------

# The control plane reads the token signing key. This is the only identity with
# access to it.
resource "azurerm_role_assignment" "controlplane_kv_crypto" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Crypto User"
  principal_id         = azurerm_user_assigned_identity.workload["controlplane"].principal_id
}

resource "azurerm_role_assignment" "controlplane_kv_secrets" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.workload["controlplane"].principal_id
}

# The gateway reads only its own secrets (Redis auth, on-prem CA bundle, usage
# stream DSN). Note the absence of Crypto User.
resource "azurerm_role_assignment" "gateway_kv_secrets" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.workload["gateway"].principal_id
}

resource "azurerm_role_assignment" "fleetview_kv_secrets" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.workload["fleetview"].principal_id
}

# ---------------------------------------------------------------------------
# Azure OpenAI grants
# ---------------------------------------------------------------------------

# `Cognitive Services OpenAI User` allows inference and nothing else — it
# cannot create or modify deployments. The gateway calls models; it does not
# manage them.
resource "azurerm_role_assignment" "gateway_openai" {
  scope                = azurerm_cognitive_account.openai.id
  role_definition_name = "Cognitive Services OpenAI User"
  principal_id         = azurerm_user_assigned_identity.workload["gateway"].principal_id
}

# The guardrail service calls the content-safety endpoint on the same account
# when running in `callout` mode.
resource "azurerm_role_assignment" "guardrails_cognitive" {
  scope                = azurerm_cognitive_account.openai.id
  role_definition_name = "Cognitive Services User"
  principal_id         = azurerm_user_assigned_identity.workload["guardrails"].principal_id
}

# ---------------------------------------------------------------------------
# Redis grants
# ---------------------------------------------------------------------------

resource "azurerm_redis_cache_access_policy_assignment" "gateway" {
  name               = "gateway"
  redis_cache_id     = azurerm_redis_cache.this.id
  access_policy_name = "Data Contributor"
  object_id          = azurerm_user_assigned_identity.workload["gateway"].principal_id
  object_id_alias    = "gateway"
}

# ---------------------------------------------------------------------------
# PostgreSQL grants
# ---------------------------------------------------------------------------
# Entra administrators on the server. Table-level grants (controlplane writes,
# fleetview reads) are applied by the schema migration, because Terraform has
# no business holding a connection to the application database.

resource "azurerm_postgresql_flexible_server_active_directory_administrator" "controlplane" {
  server_name         = azurerm_postgresql_flexible_server.this.name
  resource_group_name = azurerm_resource_group.this.name
  tenant_id           = var.tenant_id
  object_id           = azurerm_user_assigned_identity.workload["controlplane"].principal_id
  principal_name      = azurerm_user_assigned_identity.workload["controlplane"].name
  principal_type      = "ServicePrincipal"
}

resource "azurerm_postgresql_flexible_server_active_directory_administrator" "fleetview" {
  server_name         = azurerm_postgresql_flexible_server.this.name
  resource_group_name = azurerm_resource_group.this.name
  tenant_id           = var.tenant_id
  object_id           = azurerm_user_assigned_identity.workload["fleetview"].principal_id
  principal_name      = azurerm_user_assigned_identity.workload["fleetview"].name
  principal_type      = "ServicePrincipal"
}

# ---------------------------------------------------------------------------
# Usage stream and telemetry grants
# ---------------------------------------------------------------------------

# The gateway writes UsageRecords (SPEC §6). Send-only: it must not be able to
# read back or delete what it has written, because the stream is the immutable
# input to chargeback.
resource "azurerm_role_assignment" "gateway_eventhub_send" {
  scope                = azurerm_eventhub_namespace.usage.id
  role_definition_name = "Azure Event Hubs Data Sender"
  principal_id         = azurerm_user_assigned_identity.workload["gateway"].principal_id
}

# fleetview consumes the stream to build hourly and daily rollups.
resource "azurerm_role_assignment" "fleetview_eventhub_receive" {
  scope                = azurerm_eventhub_namespace.usage.id
  role_definition_name = "Azure Event Hubs Data Receiver"
  principal_id         = azurerm_user_assigned_identity.workload["fleetview"].principal_id
}

# The collector publishes to Azure Monitor.
resource "azurerm_role_assignment" "collector_monitoring_publisher" {
  scope                = azurerm_resource_group.this.id
  role_definition_name = "Monitoring Metrics Publisher"
  principal_id         = azurerm_user_assigned_identity.workload["otel-collector"].principal_id
}

# fleetview queries the managed Prometheus workspace for SLI evaluation.
resource "azurerm_role_assignment" "fleetview_monitoring_reader" {
  scope                = azurerm_monitor_workspace.this.id
  role_definition_name = "Monitoring Data Reader"
  principal_id         = azurerm_user_assigned_identity.workload["fleetview"].principal_id
}

# ---------------------------------------------------------------------------
# Cluster-level grants
# ---------------------------------------------------------------------------

# The kubelet identity pulls images. Scoped to the registry, not the
# subscription.
resource "azurerm_role_assignment" "kubelet_acr_pull" {
  scope                = azurerm_resource_group.this.id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_kubernetes_cluster.this.kubelet_identity[0].object_id
}

# The cluster's own identity manages the route table and the private DNS zone
# so that the Gateway API controller and external-dns can program them.
resource "azurerm_role_assignment" "cluster_network_contributor" {
  scope                = azurerm_route_table.aks.id
  role_definition_name = "Network Contributor"
  principal_id         = azurerm_kubernetes_cluster.this.identity[0].principal_id
}

resource "azurerm_role_assignment" "cluster_dns_contributor" {
  scope                = azurerm_private_dns_zone.agentgate_internal.id
  role_definition_name = "Private DNS Zone Contributor"
  principal_id         = azurerm_kubernetes_cluster.this.identity[0].principal_id
}
