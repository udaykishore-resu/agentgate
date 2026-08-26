# Outputs.
#
# These are the values the Kubernetes layer needs. The k8s overlays are not
# generated from Terraform — a cluster that can only be configured by a
# successful apply is a cluster you cannot fix during an incident — so these
# are consumed by the deploy pipeline, which patches them into the overlay
# before applying:
#
#   terraform -chdir=deploy/terraform/azure output -json > /tmp/tf.json
#   scripts/render-overlay.sh prod /tmp/tf.json
#
# Nothing here is a secret. Connection strings and keys live in Key Vault and
# are read at runtime by workload identity; putting them in state would put
# them in the state file's blob container, which has a different access model.

output "resource_group_name" {
  description = "Resource group holding this environment."
  value       = azurerm_resource_group.this.name
}

output "location" {
  description = "Region this environment is deployed in."
  value       = azurerm_resource_group.this.location
}

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

output "aks_cluster_name" {
  description = "AKS cluster name. Use with `az aks get-credentials --admin=false`; local admin accounts are disabled."
  value       = azurerm_kubernetes_cluster.this.name
}

output "aks_oidc_issuer_url" {
  description = "Cluster OIDC issuer. The trust anchor for every federated credential; also what an auditor checks when asked how workload identity is bound."
  value       = azurerm_kubernetes_cluster.this.oidc_issuer_url
}

output "aks_node_resource_group" {
  description = "Auto-created resource group holding node VMSS and load balancers. Not managed by this configuration."
  value       = azurerm_kubernetes_cluster.this.node_resource_group
}

# ---------------------------------------------------------------------------
# Workload identity — patched into the ServiceAccount annotations
# ---------------------------------------------------------------------------

output "workload_identity_client_ids" {
  description = "Map of ServiceAccount name to managed identity client id. Goes into azure.workload.identity/client-id on each ServiceAccount in deploy/k8s/base/serviceaccounts.yaml."
  value       = { for k, v in azurerm_user_assigned_identity.workload : k => v.client_id }
}

output "workload_identity_principal_ids" {
  description = "Map of ServiceAccount name to managed identity principal (object) id, for RBAC grants made outside this configuration."
  value       = { for k, v in azurerm_user_assigned_identity.workload : k => v.principal_id }
}

output "tenant_id" {
  description = "Entra tenant, for azure.workload.identity/tenant-id."
  value       = var.tenant_id
}

# ---------------------------------------------------------------------------
# Dependencies — hostnames only, never credentials
# ---------------------------------------------------------------------------

output "postgres_fqdn" {
  description = "PostgreSQL private FQDN. Resolves only inside the VNet."
  value       = azurerm_postgresql_flexible_server.this.fqdn
}

output "postgres_database" {
  description = "Application database name."
  value       = azurerm_postgresql_flexible_server_database.agentgate.name
}

output "redis_hostname" {
  description = "Redis private hostname. Port 6380 (TLS) only; the non-TLS port is disabled."
  value       = azurerm_redis_cache.this.hostname
}

output "key_vault_uri" {
  description = "Key Vault URI for AGENTGATE_VAULT_ADDR and the External Secrets SecretStore."
  value       = azurerm_key_vault.this.vault_uri
}

output "openai_endpoint" {
  description = "Azure OpenAI endpoint. Resolves to the private endpoint address inside the VNet; there is no public route to it."
  value       = azurerm_cognitive_account.openai.endpoint
}

output "openai_deployment_names" {
  description = "Deployed model names, which must match the `model` values in the gateway's pool configuration."
  value       = [for d in azurerm_cognitive_deployment.this : d.name]
}

output "usage_stream_namespace" {
  description = "Event Hubs namespace hosting the UsageRecord stream (SPEC §6)."
  value       = azurerm_eventhub_namespace.usage.name
}

output "usage_stream_eventhub" {
  description = "Event hub name for UsageRecords."
  value       = azurerm_eventhub.usage.name
}

# ---------------------------------------------------------------------------
# Edge
# ---------------------------------------------------------------------------

output "apim_gateway_url" {
  description = "APIM gateway URL. Internal VNet mode, so this resolves only from inside the network or over ExpressRoute."
  value       = azurerm_api_management.this.gateway_url
}

output "apim_private_ip_addresses" {
  description = "APIM's private IPs, for the DNS records consumers resolve."
  value       = azurerm_api_management.this.private_ip_addresses
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------

output "log_analytics_workspace_id" {
  description = "Log Analytics workspace resource id."
  value       = azurerm_log_analytics_workspace.this.id
}

output "monitor_workspace_query_endpoint" {
  description = "Managed Prometheus query endpoint, for AGENTGATE_PROM_URL when running against Azure Monitor rather than in-cluster Prometheus."
  value       = azurerm_monitor_workspace.this.query_endpoint
}

output "application_insights_connection_string" {
  description = "App Insights connection string. Marked sensitive because it embeds the ingestion key."
  value       = azurerm_application_insights.this.connection_string
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

output "vnet_id" {
  description = "VNet resource id, for peering an agent-hosting VNet into this one."
  value       = azurerm_virtual_network.this.id
}

output "firewall_private_ip" {
  description = "Azure Firewall private IP — the next hop for all egress and the VNet's DNS server."
  value       = azurerm_firewall.this.ip_configuration[0].private_ip_address
}

output "firewall_public_ip" {
  description = "The single outbound IP the whole environment egresses from. Third parties allowlist this address."
  value       = azurerm_public_ip.firewall.ip_address
}

output "private_dns_zone_names" {
  description = "Private DNS zones created for the private-endpoint paths in SPEC §8."
  value       = [for z in azurerm_private_dns_zone.this : z.name]
}

# ---------------------------------------------------------------------------
# Convenience
# ---------------------------------------------------------------------------

output "kubectl_config_command" {
  description = "Command to obtain cluster credentials. The API server is private, so this must run from inside the VNet or over the VPN."
  value       = "az aks get-credentials --resource-group ${azurerm_resource_group.this.name} --name ${azurerm_kubernetes_cluster.this.name} --overwrite-existing"
}
