# Observability plane (SPEC §4.4, §6) and the usage stream that feeds
# chargeback.
#
# AgentGate's own pipeline (OTel collector -> Prometheus/Loki/trace store) is
# the primary path and is deployed in-cluster. What is created here is the
# Azure-native side of it: managed Prometheus for metrics that must outlive the
# cluster, Log Analytics and App Insights for platform-level diagnostics, and
# Event Hubs for the immutable UsageRecord stream.

resource "azurerm_log_analytics_workspace" "this" {
  name                = "log-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  sku                 = "PerGB2018"
  retention_in_days   = var.log_retention_days
  tags                = local.common_tags

  # Ingestion only over the private link scope in network.tf.
  internet_ingestion_enabled = false
  internet_query_enabled     = false
}

resource "azurerm_application_insights" "this" {
  name                = "appi-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  application_type    = "other"
  workspace_id        = azurerm_log_analytics_workspace.this.id
  tags                = local.common_tags

  internet_ingestion_enabled = false
  internet_query_enabled     = false

  # AgentGate does its own sampling in the collector's tail_sampling processor,
  # where the decision can be made on the trace's outcome. App Insights'
  # ingestion sampling is head-based and would discard errors at the same rate
  # as successes, undoing the policy set in deploy/otel/collector-gateway.yaml.
  sampling_percentage = 100
}

# Managed Prometheus. The recording and alerting rules in deploy/prometheus
# are the source of truth and are loaded here as rule groups, so an
# environment running managed Prometheus and one running self-hosted evaluate
# identical expressions.
resource "azurerm_monitor_workspace" "this" {
  name                = "amw-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tags                = local.common_tags

  public_network_access_enabled = false
}

resource "azurerm_monitor_data_collection_endpoint" "prometheus" {
  name                = "dce-${local.base_name}-prom"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  kind                = "Linux"
  tags                = local.common_tags

  public_network_access_enabled = false
}

resource "azurerm_monitor_data_collection_rule" "prometheus" {
  name                        = "dcr-${local.base_name}-prom"
  resource_group_name         = azurerm_resource_group.this.name
  location                    = azurerm_resource_group.this.location
  data_collection_endpoint_id = azurerm_monitor_data_collection_endpoint.prometheus.id
  kind                        = "Linux"
  tags                        = local.common_tags

  destinations {
    monitor_account {
      monitor_account_id = azurerm_monitor_workspace.this.id
      name               = "MonitoringAccount"
    }
  }

  data_flow {
    streams      = ["Microsoft-PrometheusMetrics"]
    destinations = ["MonitoringAccount"]
  }

  data_sources {
    prometheus_forwarder {
      streams = ["Microsoft-PrometheusMetrics"]
      name    = "PrometheusDataSource"
    }
  }
}

resource "azurerm_monitor_data_collection_rule_association" "prometheus" {
  name                    = "dcra-${local.base_name}-prom"
  target_resource_id      = azurerm_kubernetes_cluster.this.id
  data_collection_rule_id = azurerm_monitor_data_collection_rule.prometheus.id
}

# ---------------------------------------------------------------------------
# Usage stream (SPEC §6)
# ---------------------------------------------------------------------------

resource "azurerm_eventhub_namespace" "usage" {
  name                = "evhns-${local.base_name}-${local.suffix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  sku                 = var.environment == "prod" ? "Premium" : "Standard"
  capacity            = 1
  tags                = local.common_tags

  public_network_access_enabled = false
  minimum_tls_version           = "1.2"
  local_authentication_enabled  = false

  identity {
    type = "SystemAssigned"
  }
}

resource "azurerm_eventhub" "usage" {
  name              = "usage-records"
  namespace_id      = azurerm_eventhub_namespace.usage.id
  partition_count   = var.usage_stream_partitions
  # Seven days of replay. Long enough to rebuild a month's rollups after a
  # fleetview bug without going back to cold storage, short enough that the
  # stream is not a second copy of the warehouse.
  message_retention = 7
}

resource "azurerm_eventhub_consumer_group" "fleetview" {
  name                = "fleetview-rollup"
  namespace_name      = azurerm_eventhub_namespace.usage.name
  eventhub_name       = azurerm_eventhub.usage.name
  resource_group_name = azurerm_resource_group.this.name
}

resource "azurerm_eventhub_consumer_group" "warehouse" {
  # A separate consumer group so a slow warehouse loader cannot delay the
  # hourly chargeback rollups, and a rollup replay cannot re-deliver to the
  # warehouse.
  name                = "warehouse-export"
  namespace_name      = azurerm_eventhub_namespace.usage.name
  eventhub_name       = azurerm_eventhub.usage.name
  resource_group_name = azurerm_resource_group.this.name
}

# ---------------------------------------------------------------------------
# Diagnostic settings
# ---------------------------------------------------------------------------
# Every component that can produce an audit trail sends it to Log Analytics.
# These are the logs a security review asks for by name.

resource "azurerm_monitor_diagnostic_setting" "aks" {
  name                       = "diag-aks"
  target_resource_id         = azurerm_kubernetes_cluster.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  enabled_log {
    category = "kube-audit-admin"
  }

  enabled_log {
    category = "kube-apiserver"
  }

  enabled_log {
    category = "guard"
  }

  enabled_log {
    category = "cluster-autoscaler"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

resource "azurerm_monitor_diagnostic_setting" "keyvault" {
  name                       = "diag-kv"
  target_resource_id         = azurerm_key_vault.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  # Every read of the token signing key is recorded. This is the evidence that
  # answers "who could have minted an agent identity" and it is the reason the
  # setting is not optional in any environment.
  enabled_log {
    category = "AuditEvent"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

resource "azurerm_monitor_diagnostic_setting" "openai" {
  name                       = "diag-openai"
  target_resource_id         = azurerm_cognitive_account.openai.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  enabled_log {
    category = "Audit"
  }

  enabled_log {
    category = "RequestResponse"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

resource "azurerm_monitor_diagnostic_setting" "firewall" {
  name                       = "diag-firewall"
  target_resource_id         = azurerm_firewall.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  # The record of what was allowed out and what was denied — the operational
  # counterpart to the FQDN allowlist, and the first thing to check when a
  # dependency "suddenly stopped working" after a review.
  enabled_log {
    category = "AZFWApplicationRule"
  }

  enabled_log {
    category = "AZFWNetworkRule"
  }

  enabled_log {
    category = "AZFWDnsQuery"
  }

  enabled_log {
    category = "AZFWIdpsSignature"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

resource "azurerm_monitor_diagnostic_setting" "postgres" {
  name                       = "diag-psql"
  target_resource_id         = azurerm_postgresql_flexible_server.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  enabled_log {
    category = "PostgreSQLLogs"
  }

  enabled_log {
    category = "PostgreSQLFlexSessions"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

resource "azurerm_monitor_diagnostic_setting" "redis" {
  name                       = "diag-redis"
  target_resource_id         = azurerm_redis_cache.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  enabled_log {
    category = "ConnectedClientList"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

resource "azurerm_monitor_diagnostic_setting" "apim" {
  name                       = "diag-apim"
  target_resource_id         = azurerm_api_management.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  enabled_log {
    category = "GatewayLogs"
  }

  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

# ---------------------------------------------------------------------------
# Platform-level alerts
# ---------------------------------------------------------------------------
# Application SLO alerts live in deploy/prometheus/rules, where they can be
# reviewed alongside the SLIs they measure. What is here is the small set of
# infrastructure conditions Prometheus cannot see, because they take Prometheus
# itself down.

resource "azurerm_monitor_action_group" "page" {
  name                = "ag-${local.base_name}-page"
  resource_group_name = azurerm_resource_group.this.name
  short_name          = "agpage"
  tags                = local.common_tags

  webhook_receiver {
    name                    = "pagerduty"
    service_uri             = "https://events.pagerduty.com/integration/azure/enqueue"
    use_common_alert_schema = true
  }
}

resource "azurerm_monitor_metric_alert" "openai_quota" {
  name                = "alert-${local.base_name}-openai-quota"
  resource_group_name = azurerm_resource_group.this.name
  scopes              = [azurerm_cognitive_account.openai.id]
  description         = "Azure OpenAI is rejecting requests for quota. The gateway will fail over to Bedrock and the on-prem tier, raising cost and TTFT."
  severity            = 2
  frequency           = "PT1M"
  window_size         = "PT5M"
  tags                = local.common_tags

  criteria {
    metric_namespace = "Microsoft.CognitiveServices/accounts"
    metric_name      = "AzureOpenAIProvisionedManagedUtilizationV2"
    aggregation      = "Average"
    operator         = "GreaterThan"
    threshold        = 90
  }

  action {
    action_group_id = azurerm_monitor_action_group.page.id
  }
}

resource "azurerm_monitor_metric_alert" "redis_memory" {
  name                = "alert-${local.base_name}-redis-memory"
  resource_group_name = azurerm_resource_group.this.name
  scopes              = [azurerm_redis_cache.this.id]
  description         = "Redis is near its memory ceiling. Eviction of rate-limit buckets makes quota enforcement approximate, which is worse than it sounds: it fails open."
  severity            = 2
  frequency           = "PT1M"
  window_size         = "PT5M"
  tags                = local.common_tags

  criteria {
    metric_namespace = "Microsoft.Cache/redis"
    metric_name      = "usedmemorypercentage"
    aggregation      = "Average"
    operator         = "GreaterThan"
    threshold        = 85
  }

  action {
    action_group_id = azurerm_monitor_action_group.page.id
  }
}

resource "azurerm_monitor_metric_alert" "postgres_storage" {
  name                = "alert-${local.base_name}-psql-storage"
  resource_group_name = azurerm_resource_group.this.name
  scopes              = [azurerm_postgresql_flexible_server.this.id]
  description         = "PostgreSQL storage above 85%. At 100% the server goes read-only and the promotion gate stops recording approvals."
  severity            = 2
  frequency           = "PT5M"
  window_size         = "PT15M"
  tags                = local.common_tags

  criteria {
    metric_namespace = "Microsoft.DBforPostgreSQL/flexibleServers"
    metric_name      = "storage_percent"
    aggregation      = "Average"
    operator         = "GreaterThan"
    threshold        = 85
  }

  action {
    action_group_id = azurerm_monitor_action_group.page.id
  }
}
