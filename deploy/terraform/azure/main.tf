# AgentGate — Azure platform.
#
# COMPUTE CHOICE: AKS, not Container Apps.
#
# Container Apps is the cheaper and simpler option and it was the first choice.
# It was rejected for three reasons, all of which come from the spec rather
# than from preference:
#
#   1. The telemetry design (SPEC §4.4) needs a two-tier collector where the
#      first tier is node-local and the second is a trace-ID-sharded pool. A
#      DaemonSet is the shape of tier one; Container Apps has no per-node
#      placement and no way to address a node-local sidecarless collector.
#   2. SPEC §8 requires default-deny east-west with explicit per-path allows.
#      Container Apps environments give ingress/egress controls at the app
#      level, not the pod-to-pod granularity the network policy table needs.
#   3. Workload identity federation is per-ServiceAccount here (five distinct
#      identities with different vault grants). Container Apps binds managed
#      identity per app, which works, but the promotion story — one identity
#      per workload, subject-scoped, auditable — is materially weaker.
#
# Container Apps remains the right answer for the *agents* themselves, which is
# why `agentgate.runtime` includes `aca` in SPEC §4.1. The control plane is the
# part that needs Kubernetes.

resource "azurerm_resource_group" "this" {
  name     = "rg-${local.base_name}"
  location = var.location
  tags     = local.common_tags

  lifecycle {
    # Renaming the resource group means recreating everything in it.
    ignore_changes = [tags["created_at"]]
  }
}

# ---------------------------------------------------------------------------
# AKS
# ---------------------------------------------------------------------------

resource "azurerm_kubernetes_cluster" "this" {
  name                = "aks-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  dns_prefix          = "aks-${local.base_name}"
  kubernetes_version  = var.kubernetes_version
  sku_tier            = var.environment == "prod" ? "Standard" : "Free"
  tags                = local.common_tags

  # The two settings that make federated workload identity possible. Without
  # the OIDC issuer there is nothing for Entra ID to trust; without the
  # workload identity add-on the projected token is never mounted.
  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  # Private control plane: the API server has no public endpoint. Access is via
  # the platform team's jump path or GitHub Actions self-hosted runners inside
  # the VNet.
  private_cluster_enabled             = true
  private_cluster_public_fqdn_enabled = false

  # Local (certificate) admin accounts disabled: every human and pipeline
  # authenticates through Entra ID, so access is revocable centrally and shows
  # up in the audit log with a name attached.
  local_account_disabled = true

  azure_active_directory_role_based_access_control {
    azure_rbac_enabled = true
    tenant_id          = var.tenant_id
  }

  default_node_pool {
    name                         = "system"
    vm_size                      = var.system_node_pool.vm_size
    node_count                   = var.system_node_pool.node_count
    zones                        = var.system_node_pool.zones
    vnet_subnet_id               = azurerm_subnet.aks.id
    only_critical_addons_enabled = true
    os_sku                       = "AzureLinux"
    max_pods                     = 110

    upgrade_settings {
      max_surge = "33%"
    }
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin = "azure"
    # Overlay mode: pod IPs come from a private CIDR that is not routable in
    # the VNet, so pod density is not bounded by VNet address space. The
    # NetworkPolicy address plan assumes 10.244.0.0/16 for pods.
    network_plugin_mode = "overlay"
    pod_cidr            = "10.244.0.0/16"
    service_cidr        = "10.0.0.0/16"
    dns_service_ip      = "10.0.0.10"
    # Cilium enforces the NetworkPolicies in deploy/k8s/base/networkpolicy.yaml.
    # Azure's own policy engine does not support all the selectors used there.
    network_policy     = "cilium"
    network_data_plane = "cilium"
    load_balancer_sku  = "standard"
    # All egress leaves through the firewall via the route table, so the
    # cluster must not provision its own outbound SNAT path.
    outbound_type = "userDefinedRouting"
  }

  oms_agent {
    log_analytics_workspace_id      = azurerm_log_analytics_workspace.this.id
    msi_auth_for_monitoring_enabled = true
  }

  monitor_metrics {
    # Managed Prometheus scrapes the same endpoints Prometheus does locally,
    # so the recording rules in deploy/prometheus work unchanged.
    annotations_allowed = "prometheus.io/scrape,prometheus.io/port"
    labels_allowed      = "app.kubernetes.io/name,app.kubernetes.io/component,app.kubernetes.io/environment"
  }

  key_vault_secrets_provider {
    secret_rotation_enabled  = true
    secret_rotation_interval = "5m"
  }

  auto_scaler_profile {
    # Aggressive scale-down is wrong here: removing a gateway node cancels the
    # streams its pods are carrying.
    scale_down_delay_after_add    = "15m"
    scale_down_unneeded           = "15m"
    skip_nodes_with_local_storage = true
    skip_nodes_with_system_pods   = true
  }

  maintenance_window_auto_upgrade {
    frequency   = "Weekly"
    interval    = 1
    duration    = 4
    day_of_week = "Tuesday"
    start_time  = "02:00"
    utc_offset  = "+00:00"
  }

  lifecycle {
    ignore_changes = [
      # The cluster autoscaler owns this after creation.
      default_node_pool[0].node_count,
    ]
  }
}

resource "azurerm_kubernetes_cluster_node_pool" "workload" {
  name                  = "workload"
  kubernetes_cluster_id = azurerm_kubernetes_cluster.this.id
  vm_size               = var.workload_node_pool.vm_size
  zones                 = var.workload_node_pool.zones
  vnet_subnet_id        = azurerm_subnet.aks.id
  os_sku                = "AzureLinux"
  max_pods              = 110
  tags                  = local.common_tags

  auto_scaling_enabled = true
  min_count            = var.workload_node_pool.min_count
  max_count            = var.workload_node_pool.max_count

  node_labels = {
    "agentgate.io/pool" = "workload"
  }

  upgrade_settings {
    max_surge = "33%"
  }

  lifecycle {
    ignore_changes = [node_count]
  }
}

# ---------------------------------------------------------------------------
# PostgreSQL — registry, promotion records, fleet inventory
# ---------------------------------------------------------------------------

resource "azurerm_postgresql_flexible_server" "this" {
  name                = "psql-${local.base_name}-${local.suffix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  version             = var.postgres.version
  sku_name            = var.postgres.sku_name
  storage_mb          = var.postgres.storage_mb
  tags                = local.common_tags

  # VNet-injected: no public endpoint exists at all, so there is no firewall
  # rule to get wrong.
  delegated_subnet_id           = azurerm_subnet.postgres.id
  private_dns_zone_id           = azurerm_private_dns_zone.this["postgres"].id
  public_network_access_enabled = false

  # Entra-only authentication. Password auth is disabled outright: the
  # control plane connects with its workload identity, so there is no password
  # to rotate, leak, or find in a connection string.
  authentication {
    active_directory_auth_enabled = true
    password_auth_enabled         = false
    tenant_id                     = var.tenant_id
  }

  backup_retention_days        = var.postgres.backup_retention_days
  geo_redundant_backup_enabled = var.postgres.geo_redundant_backup

  dynamic "high_availability" {
    for_each = var.postgres.high_availability ? [1] : []

    content {
      mode                      = "ZoneRedundant"
      standby_availability_zone = "2"
    }
  }

  maintenance_window {
    day_of_week  = 2
    start_hour   = 2
    start_minute = 0
  }

  depends_on = [azurerm_private_dns_zone_virtual_network_link.this]

  lifecycle {
    prevent_destroy = true
  }
}

resource "azurerm_postgresql_flexible_server_database" "agentgate" {
  name      = "agentgate"
  server_id = azurerm_postgresql_flexible_server.this.id
  collation = "en_US.utf8"
  charset   = "utf8"

  lifecycle {
    prevent_destroy = true
  }
}

resource "azurerm_postgresql_flexible_server_configuration" "log_statement" {
  name      = "log_statement"
  server_id = azurerm_postgresql_flexible_server.this.id
  # DDL only. Logging every statement would put registration payloads —
  # including owner emails — into the server log, which has a different
  # retention and access model from the application's own telemetry.
  value = "ddl"
}

resource "azurerm_postgresql_flexible_server_configuration" "pg_stat_statements" {
  name      = "pg_stat_statements.track"
  server_id = azurerm_postgresql_flexible_server.this.id
  value     = "top"
}

resource "azurerm_postgresql_flexible_server_configuration" "shared_preload_libraries" {
  name      = "shared_preload_libraries"
  server_id = azurerm_postgresql_flexible_server.this.id
  value     = "pg_stat_statements"
}

# ---------------------------------------------------------------------------
# Redis — distributed rate-limit buckets and response cache
# ---------------------------------------------------------------------------

resource "azurerm_redis_cache" "this" {
  name                = "redis-${local.base_name}-${local.suffix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  capacity            = var.redis.capacity
  family              = var.redis.family
  sku_name            = var.redis.sku_name
  tags                = local.common_tags

  # Reachable only through the private endpoint in network.tf.
  public_network_access_enabled = false
  minimum_tls_version           = "1.2"
  non_ssl_port_enabled          = false

  zones = var.environment == "prod" ? ["1", "2", "3"] : null

  redis_configuration {
    # Both rate-limit buckets and cache entries are reconstructible, so
    # persistence is off: an AOF fsync on the quota path would add latency to
    # every request to protect data we are happy to lose.
    maxmemory_policy = "allkeys-lru"
    # Entra ID auth instead of the access key, matching the "no static
    # credentials" rule everywhere else.
    active_directory_authentication_enabled = true
  }

  patch_schedule {
    day_of_week    = "Tuesday"
    start_hour_utc = 3
  }
}

# ---------------------------------------------------------------------------
# Key Vault — token signing key and connection strings
# ---------------------------------------------------------------------------

resource "azurerm_key_vault" "this" {
  name                = "kv-${var.name_prefix}-${var.environment}-${local.suffix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tenant_id           = var.tenant_id
  sku_name            = "premium"
  tags                = local.common_tags

  # Premium, because the token signing key is HSM-backed. A software key that
  # can be exported is a key that can be used to mint agent identities
  # offline.
  enable_rbac_authorization  = true
  purge_protection_enabled   = true
  soft_delete_retention_days = 90

  public_network_access_enabled = false

  network_acls {
    # Deny by default; the private endpoint is the only way in. AzureServices
    # is bypassed so that Key Vault-backed diagnostic settings and the CSI
    # driver work without a public path.
    default_action             = "Deny"
    bypass                     = "AzureServices"
    virtual_network_subnet_ids = [azurerm_subnet.aks.id]
  }

  lifecycle {
    prevent_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Azure OpenAI
# ---------------------------------------------------------------------------

resource "azurerm_cognitive_account" "openai" {
  name                = "oai-${local.base_name}-${local.suffix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  kind                = "OpenAI"
  sku_name            = "S0"
  tags                = local.common_tags

  # `custom_subdomain_name` is required for both private endpoints and Entra
  # ID token auth; without it the account can only be reached with an API key
  # over a public endpoint.
  custom_subdomain_name         = "oai-${local.base_name}-${local.suffix}"
  public_network_access_enabled = false
  local_auth_enabled            = false

  identity {
    type = "SystemAssigned"
  }

  network_acls {
    default_action = "Deny"
  }
}

resource "azurerm_cognitive_deployment" "this" {
  for_each = { for d in var.openai_deployments : d.name => d }

  name                 = each.value.name
  cognitive_account_id = azurerm_cognitive_account.openai.id

  model {
    format  = "OpenAI"
    name    = each.value.model_name
    version = each.value.model_version
  }

  sku {
    name     = each.value.sku_name
    capacity = each.value.capacity
  }

  # Version upgrades change tokenisation and output distribution, which moves
  # both cost and the golden-corpus diffs in the migration suite (SPEC §7).
  # They happen on a change ticket, not on Microsoft's schedule.
  version_upgrade_option = "NoAutoUpgrade"

  lifecycle {
    # Capacity is adjusted operationally in response to quota pressure; let
    # that happen without a Terraform round trip, and reconcile at review time.
    ignore_changes = [sku[0].capacity]
  }
}
