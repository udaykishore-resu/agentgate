# Network, private connectivity and egress control (SPEC §8).
#
# The shape: one VNet, no public IP on anything that serves traffic, every PaaS
# dependency reached through a Private Endpoint with a matching Private DNS
# zone, and all outbound traffic forced through an Azure Firewall that enforces
# an FQDN allowlist. The firewall is the only path to the internet, and the
# route table is what makes that true rather than aspirational.

resource "azurerm_virtual_network" "this" {
  name                = "vnet-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  address_space       = [var.vnet_cidr]
  tags                = local.common_tags
}

# DNS servers are set in a second step rather than inline on the VNet. Inline
# would create a dependency cycle: the VNet must exist before the firewall
# subnet, which must exist before the firewall, whose private IP is the DNS
# server. This resource breaks the cycle without a hardcoded IP.
resource "azurerm_virtual_network_dns_servers" "this" {
  virtual_network_id = azurerm_virtual_network.this.id
  # The firewall's DNS proxy. FQDN-based network rules only work when clients
  # resolve through the firewall, and it keeps private endpoint resolution
  # consistent across every subnet.
  dns_servers = [azurerm_firewall.this.ip_configuration[0].private_ip_address]
}

resource "azurerm_subnet" "aks" {
  name                 = "snet-aks"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.aks]

  # Nodes reach storage and Key Vault over the Microsoft backbone rather than
  # through the firewall — lower latency and it keeps image-pull traffic out of
  # the firewall's rule evaluation.
  service_endpoints = ["Microsoft.Storage", "Microsoft.KeyVault", "Microsoft.ContainerRegistry"]
}

resource "azurerm_subnet" "private_endpoints" {
  name                 = "snet-private-endpoints"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.private_endpoints]

  private_endpoint_network_policies = "Enabled"
}

resource "azurerm_subnet" "postgres" {
  name                 = "snet-postgres"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.postgres]

  # Flexible Server uses VNet injection with a delegated subnet rather than a
  # private endpoint. The subnet may hold nothing else.
  delegation {
    name = "postgres-flexible-server"

    service_delegation {
      name    = "Microsoft.DBforPostgreSQL/flexibleServers"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}

resource "azurerm_subnet" "apim" {
  name                 = "snet-apim"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.apim]
}

# Both firewall subnets must carry these exact names; Azure rejects anything
# else.
resource "azurerm_subnet" "firewall" {
  name                 = "AzureFirewallSubnet"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.firewall]
}

resource "azurerm_subnet" "firewall_management" {
  name                 = "AzureFirewallManagementSubnet"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.firewall_mgmt]
}

# ---------------------------------------------------------------------------
# Network security groups
# ---------------------------------------------------------------------------

resource "azurerm_network_security_group" "aks" {
  name                = "nsg-${local.base_name}-aks"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tags                = local.common_tags

  # Only APIM may reach the workloads' ingress path. Pod-to-pod rules are
  # NetworkPolicies (deploy/k8s/base/networkpolicy.yaml) — an NSG cannot see
  # inside the overlay network, so trying to express pod paths here produces a
  # false sense of control.
  security_rule {
    name                       = "allow-apim-inbound-https"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "443"
    source_address_prefix      = var.subnet_cidrs.apim
    destination_address_prefix = var.subnet_cidrs.aks
  }

  security_rule {
    name                       = "allow-vnet-inbound"
    priority                   = 200
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "VirtualNetwork"
    destination_address_prefix = "VirtualNetwork"
  }

  security_rule {
    name                       = "deny-internet-inbound"
    priority                   = 4000
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "Internet"
    destination_address_prefix = "*"
  }
}

resource "azurerm_subnet_network_security_group_association" "aks" {
  subnet_id                 = azurerm_subnet.aks.id
  network_security_group_id = azurerm_network_security_group.aks.id
}

resource "azurerm_network_security_group" "apim" {
  name                = "nsg-${local.base_name}-apim"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tags                = local.common_tags

  # APIM in internal VNet mode has a fixed set of required inbound rules;
  # omitting the management endpoint rule puts the instance into a permanently
  # "Updating" state that is unpleasant to diagnose.
  security_rule {
    name                       = "allow-apim-management"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "3443"
    source_address_prefix      = "ApiManagement"
    destination_address_prefix = "VirtualNetwork"
  }

  security_rule {
    name                       = "allow-azure-lb"
    priority                   = 110
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "6390"
    source_address_prefix      = "AzureLoadBalancer"
    destination_address_prefix = "VirtualNetwork"
  }

  security_rule {
    name                       = "allow-client-https"
    priority                   = 120
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "443"
    source_address_prefix      = "VirtualNetwork"
    destination_address_prefix = "VirtualNetwork"
  }
}

resource "azurerm_subnet_network_security_group_association" "apim" {
  subnet_id                 = azurerm_subnet.apim.id
  network_security_group_id = azurerm_network_security_group.apim.id
}

# ---------------------------------------------------------------------------
# Egress: firewall with an FQDN allowlist (SPEC §8 row 4)
# ---------------------------------------------------------------------------

resource "azurerm_public_ip" "firewall" {
  name                = "pip-${local.base_name}-fw"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  allocation_method   = "Static"
  sku                 = "Standard"
  zones               = ["1", "2", "3"]
  tags                = local.common_tags
}

resource "azurerm_public_ip" "firewall_management" {
  name                = "pip-${local.base_name}-fw-mgmt"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  allocation_method   = "Static"
  sku                 = "Standard"
  zones               = ["1", "2", "3"]
  tags                = local.common_tags
}

resource "azurerm_firewall_policy" "this" {
  name                = "afwp-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  # Premium: TLS inspection and IDPS. The third-party model path requires
  # inspecting egress (SPEC §8 row 4), which Standard cannot do.
  sku  = "Premium"
  tags = local.common_tags

  dns {
    proxy_enabled = true
  }

  threat_intelligence_mode = "Deny"

  intrusion_detection {
    mode = "Deny"
  }
}

resource "azurerm_firewall_policy_rule_collection_group" "egress" {
  name               = "rcg-egress"
  firewall_policy_id = azurerm_firewall_policy.this.id
  priority           = 500

  application_rule_collection {
    name     = "arc-allowed-fqdns"
    priority = 100
    action   = "Allow"

    rule {
      name              = "allowlisted-fqdns"
      source_addresses  = [var.subnet_cidrs.aks]
      destination_fqdns = var.egress_allowed_fqdns

      protocols {
        type = "Https"
        port = 443
      }
    }

    # AKS itself needs a documented set of FQDNs to function. Expressed as an
    # FQDN tag so Microsoft maintains the list rather than the platform team
    # discovering an addition during an incident.
    rule {
      name                  = "aks-required-endpoints"
      source_addresses      = [var.subnet_cidrs.aks]
      destination_fqdn_tags = ["AzureKubernetesService"]

      protocols {
        type = "Https"
        port = 443
      }
    }
  }

  network_rule_collection {
    name     = "nrc-infrastructure"
    priority = 200
    action   = "Allow"

    # NTP. Clock skew is a first-class telemetry-trust signal (SPEC §4.5) and
    # a source of spurious JWT `exp` failures, so time sync is not optional.
    rule {
      name                  = "ntp"
      source_addresses      = [var.subnet_cidrs.aks]
      destination_addresses = ["*"]
      destination_ports     = ["123"]
      protocols             = ["UDP"]
    }

    # On-prem inference over ExpressRoute (SPEC §8 row 3). Routed, not
    # proxied: this path carries restricted-classification traffic and must not
    # be TLS-inspected by a device outside the on-prem trust boundary.
    rule {
      name                  = "onprem-inference"
      source_addresses      = [var.subnet_cidrs.aks]
      destination_addresses = [var.onprem_inference_cidr]
      destination_ports     = ["443"]
      protocols             = ["TCP"]
    }
  }
}

resource "azurerm_firewall" "this" {
  name                = "afw-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  sku_name            = "AZFW_VNet"
  sku_tier            = "Premium"
  firewall_policy_id  = azurerm_firewall_policy.this.id
  zones               = ["1", "2", "3"]
  tags                = local.common_tags

  ip_configuration {
    name                 = "primary"
    subnet_id            = azurerm_subnet.firewall.id
    public_ip_address_id = azurerm_public_ip.firewall.id
  }

  # A dedicated management NIC is what allows forced tunnelling of the data
  # path without cutting the firewall off from its own control plane.
  management_ip_configuration {
    name                 = "management"
    subnet_id            = azurerm_subnet.firewall_management.id
    public_ip_address_id = azurerm_public_ip.firewall_management.id
  }
}

# The route table is the control. Without a default route to the firewall,
# every allowlist above is advice: nodes would still egress directly through
# Azure's default internet path.
resource "azurerm_route_table" "aks" {
  name                = "rt-${local.base_name}-aks"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  tags                = local.common_tags

  route {
    name                   = "default-via-firewall"
    address_prefix         = "0.0.0.0/0"
    next_hop_type          = "VirtualAppliance"
    next_hop_in_ip_address = azurerm_firewall.this.ip_configuration[0].private_ip_address
  }

  route {
    name                   = "onprem-via-firewall"
    address_prefix         = var.onprem_inference_cidr
    next_hop_type          = "VirtualAppliance"
    next_hop_in_ip_address = azurerm_firewall.this.ip_configuration[0].private_ip_address
  }
}

resource "azurerm_subnet_route_table_association" "aks" {
  subnet_id      = azurerm_subnet.aks.id
  route_table_id = azurerm_route_table.aks.id
}

# ---------------------------------------------------------------------------
# Private DNS zones
# ---------------------------------------------------------------------------
# One zone per PaaS service, each linked to the VNet. A private endpoint
# without its zone resolves to the public IP and the "no public egress"
# guarantee quietly stops being true.

resource "azurerm_private_dns_zone" "this" {
  for_each = local.private_dns_zones

  name                = each.value
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.common_tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "this" {
  for_each = local.private_dns_zones

  name                  = "link-${each.key}"
  resource_group_name   = azurerm_resource_group.this.name
  private_dns_zone_name = azurerm_private_dns_zone.this[each.key].name
  virtual_network_id    = azurerm_virtual_network.this.id
  registration_enabled  = false
  tags                  = local.common_tags
}

# The internal service zone from SPEC §8 row 1. Records for
# gateway/controlplane/fleet are created by external-dns from the Gateway API
# listener, so no A records are declared here.
resource "azurerm_private_dns_zone" "agentgate_internal" {
  name                = "agentgate.internal"
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.common_tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "agentgate_internal" {
  name                  = "link-agentgate-internal"
  resource_group_name   = azurerm_resource_group.this.name
  private_dns_zone_name = azurerm_private_dns_zone.agentgate_internal.name
  virtual_network_id    = azurerm_virtual_network.this.id
  registration_enabled  = false
  tags                  = local.common_tags
}

# ---------------------------------------------------------------------------
# Private endpoints
# ---------------------------------------------------------------------------

resource "azurerm_private_endpoint" "redis" {
  name                = "pe-${local.base_name}-redis"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  subnet_id           = azurerm_subnet.private_endpoints.id
  tags                = local.common_tags

  private_service_connection {
    name                           = "psc-redis"
    private_connection_resource_id = azurerm_redis_cache.this.id
    subresource_names              = ["redisCache"]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name                 = "default"
    private_dns_zone_ids = [azurerm_private_dns_zone.this["redis"].id]
  }
}

resource "azurerm_private_endpoint" "keyvault" {
  name                = "pe-${local.base_name}-kv"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  subnet_id           = azurerm_subnet.private_endpoints.id
  tags                = local.common_tags

  private_service_connection {
    name                           = "psc-kv"
    private_connection_resource_id = azurerm_key_vault.this.id
    subresource_names              = ["vault"]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name                 = "default"
    private_dns_zone_ids = [azurerm_private_dns_zone.this["keyvault"].id]
  }
}

resource "azurerm_private_endpoint" "openai" {
  name                = "pe-${local.base_name}-openai"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  subnet_id           = azurerm_subnet.private_endpoints.id
  tags                = local.common_tags

  private_service_connection {
    name                           = "psc-openai"
    private_connection_resource_id = azurerm_cognitive_account.openai.id
    subresource_names              = ["account"]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name                 = "default"
    private_dns_zone_ids = [azurerm_private_dns_zone.this["openai"].id]
  }
}

resource "azurerm_private_endpoint" "eventhub" {
  name                = "pe-${local.base_name}-evhns"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  subnet_id           = azurerm_subnet.private_endpoints.id
  tags                = local.common_tags

  private_service_connection {
    name                           = "psc-evhns"
    private_connection_resource_id = azurerm_eventhub_namespace.usage.id
    subresource_names              = ["namespace"]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name                 = "default"
    private_dns_zone_ids = [azurerm_private_dns_zone.this["eventhub"].id]
  }
}

# Azure Monitor private link scope: makes Log Analytics and Application
# Insights ingestion take the private path too (SPEC §8 row 6). Without it,
# telemetry — including anything that leaked into an attribute — would egress
# to a public ingestion endpoint.
resource "azurerm_monitor_private_link_scope" "this" {
  name                = "ampls-${local.base_name}"
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.common_tags
}

resource "azurerm_monitor_private_link_scoped_service" "law" {
  name                = "amplss-law"
  resource_group_name = azurerm_resource_group.this.name
  scope_name          = azurerm_monitor_private_link_scope.this.name
  linked_resource_id  = azurerm_log_analytics_workspace.this.id
}

resource "azurerm_monitor_private_link_scoped_service" "appinsights" {
  name                = "amplss-appi"
  resource_group_name = azurerm_resource_group.this.name
  scope_name          = azurerm_monitor_private_link_scope.this.name
  linked_resource_id  = azurerm_application_insights.this.id
}

resource "azurerm_private_endpoint" "monitor" {
  name                = "pe-${local.base_name}-monitor"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  subnet_id           = azurerm_subnet.private_endpoints.id
  tags                = local.common_tags

  private_service_connection {
    name                           = "psc-monitor"
    private_connection_resource_id = azurerm_monitor_private_link_scope.this.id
    subresource_names              = ["azuremonitor"]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name = "default"
    private_dns_zone_ids = [
      azurerm_private_dns_zone.this["monitor"].id,
      azurerm_private_dns_zone.this["oms"].id,
      azurerm_private_dns_zone.this["ods"].id,
      azurerm_private_dns_zone.this["agentsvc"].id,
      azurerm_private_dns_zone.this["blob"].id,
    ]
  }
}
