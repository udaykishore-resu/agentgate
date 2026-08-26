# VPC, private connectivity and egress control (SPEC §8).
#
# The shape mirrors the Azure configuration deliberately, because the platform
# is cloud-neutral and the security review should not have to learn two mental
# models: private subnets with no direct internet route, PrivateLink for every
# AWS dependency, a Route 53 private hosted zone for internal names, and all
# residual egress forced through AWS Network Firewall with an FQDN allowlist.

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.common_tags, {
    Name = "vpc-${local.base_name}"
  })
}

# ---------------------------------------------------------------------------
# Subnets
# ---------------------------------------------------------------------------

resource "aws_subnet" "app" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.app_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = merge(local.common_tags, {
    Name = "snet-${local.base_name}-app-${var.availability_zones[count.index]}"
    # Required by the AWS Load Balancer Controller to place internal load
    # balancers; without it the controller silently picks nothing.
    "kubernetes.io/role/internal-elb" = "1"
  })
}

resource "aws_subnet" "data" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.data_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = merge(local.common_tags, {
    Name = "snet-${local.base_name}-data-${var.availability_zones[count.index]}"
  })
}

resource "aws_subnet" "public" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.public_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]
  # No public IPs by default. The only things here are NAT gateways, which get
  # an EIP explicitly.
  map_public_ip_on_launch = false

  tags = merge(local.common_tags, {
    Name = "snet-${local.base_name}-public-${var.availability_zones[count.index]}"
  })
}

resource "aws_subnet" "firewall" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.firewall_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = merge(local.common_tags, {
    Name = "snet-${local.base_name}-fw-${var.availability_zones[count.index]}"
  })
}

# ---------------------------------------------------------------------------
# Internet path: IGW -> firewall -> NAT -> private subnets
# ---------------------------------------------------------------------------
# Traffic order matters. Egress goes private subnet -> NAT -> firewall -> IGW,
# so the firewall sees the pre-NAT source and can attribute a denied request to
# a subnet rather than to one shared NAT address.

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(local.common_tags, {
    Name = "igw-${local.base_name}"
  })
}

resource "aws_eip" "nat" {
  count = local.az_count

  domain = "vpc"

  tags = merge(local.common_tags, {
    Name = "eip-${local.base_name}-nat-${var.availability_zones[count.index]}"
  })
}

# One NAT gateway per AZ. A single shared NAT is cheaper and turns one AZ
# failure into a cross-AZ data charge plus a single point of failure for all
# egress.
resource "aws_nat_gateway" "this" {
  count = local.az_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = merge(local.common_tags, {
    Name = "nat-${local.base_name}-${var.availability_zones[count.index]}"
  })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_networkfirewall_rule_group" "fqdn_allowlist" {
  name     = "rg-${local.base_name}-fqdn-allow"
  type     = "STATEFUL"
  capacity = 200

  rule_group {
    rule_variables {
      ip_sets {
        key = "HOME_NET"

        ip_set {
          definition = [var.vpc_cidr]
        }
      }
    }

    rules_source {
      rules_source_list {
        generated_rules_type = "ALLOWLIST"
        # Both, because a client that speaks plain HTTP and one that speaks
        # TLS present the destination name differently (Host header vs SNI)
        # and an allowlist that only covers one is trivially bypassable.
        target_types = ["HTTP_HOST", "TLS_SNI"]
        targets      = var.egress_allowed_fqdns
      }
    }
  }

  tags = merge(local.common_tags, {
    Name = "rg-${local.base_name}-fqdn-allow"
  })
}

resource "aws_networkfirewall_firewall_policy" "this" {
  name = "fwp-${local.base_name}"

  firewall_policy {
    stateless_default_actions          = ["aws:forward_to_sfe"]
    stateless_fragment_default_actions = ["aws:forward_to_sfe"]

    # Strict order: rules are evaluated in the order given and unmatched
    # traffic is dropped. The permissive default (`aws:pass`) would make the
    # allowlist decorative.
    stateful_engine_options {
      rule_order = "STRICT_ORDER"
    }

    stateful_default_actions = ["aws:drop_established", "aws:alert_established"]

    stateful_rule_group_reference {
      resource_arn = aws_networkfirewall_rule_group.fqdn_allowlist.arn
      priority     = 100
    }
  }

  tags = local.common_tags
}

resource "aws_networkfirewall_firewall" "this" {
  name                = "fw-${local.base_name}"
  firewall_policy_arn = aws_networkfirewall_firewall_policy.this.arn
  vpc_id              = aws_vpc.this.id

  # Protects against a `terraform destroy` in the wrong directory removing the
  # only control on what may leave the network.
  delete_protection                 = true
  firewall_policy_change_protection = var.environment == "prod"
  subnet_change_protection          = true

  dynamic "subnet_mapping" {
    for_each = aws_subnet.firewall

    content {
      subnet_id = subnet_mapping.value.id
    }
  }

  tags = local.common_tags
}

# ---------------------------------------------------------------------------
# Route tables
# ---------------------------------------------------------------------------

resource "aws_route_table" "app" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = merge(local.common_tags, {
    Name = "rt-${local.base_name}-app-${var.availability_zones[count.index]}"
  })
}

resource "aws_route" "app_default" {
  count = local.az_count

  route_table_id         = aws_route_table.app[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[count.index].id
}

resource "aws_route_table_association" "app" {
  count = local.az_count

  subnet_id      = aws_subnet.app[count.index].id
  route_table_id = aws_route_table.app[count.index].id
}

resource "aws_route_table" "public" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = merge(local.common_tags, {
    Name = "rt-${local.base_name}-public-${var.availability_zones[count.index]}"
  })
}

# Public subnets route out through the firewall endpoint, not straight to the
# IGW. This is what puts the firewall on the egress path rather than beside it.
resource "aws_route" "public_default_via_firewall" {
  count = local.az_count

  route_table_id         = aws_route_table.public[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  vpc_endpoint_id = [
    for sync in tolist(aws_networkfirewall_firewall.this.firewall_status[0].sync_states) :
    sync.attachment[0].endpoint_id
    if sync.availability_zone == var.availability_zones[count.index]
  ][0]
}

resource "aws_route_table_association" "public" {
  count = local.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public[count.index].id
}

resource "aws_route_table" "firewall" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  tags = merge(local.common_tags, {
    Name = "rt-${local.base_name}-fw-${var.availability_zones[count.index]}"
  })
}

resource "aws_route" "firewall_default" {
  count = local.az_count

  route_table_id         = aws_route_table.firewall[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

resource "aws_route_table_association" "firewall" {
  count = local.az_count

  subnet_id      = aws_subnet.firewall[count.index].id
  route_table_id = aws_route_table.firewall[count.index].id
}

# Data subnets have no route to anything outside the VPC at all. RDS and
# ElastiCache have no reason to reach the internet, and an isolated route table
# is a stronger statement of that than a security group rule.
resource "aws_route_table" "data" {
  vpc_id = aws_vpc.this.id

  tags = merge(local.common_tags, {
    Name = "rt-${local.base_name}-data"
  })
}

resource "aws_route_table_association" "data" {
  count = local.az_count

  subnet_id      = aws_subnet.data[count.index].id
  route_table_id = aws_route_table.data.id
}

# ---------------------------------------------------------------------------
# PrivateLink endpoints (SPEC §8 rows 2, 5, 6)
# ---------------------------------------------------------------------------

resource "aws_security_group" "vpc_endpoints" {
  name        = "sg-${local.base_name}-vpce"
  description = "Ingress to interface endpoints from the app subnets only."
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "HTTPS from application subnets"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = local.app_subnet_cidrs
  }

  tags = merge(local.common_tags, {
    Name = "sg-${local.base_name}-vpce"
  })
}

resource "aws_vpc_endpoint" "interface" {
  for_each = toset(local.interface_endpoints)

  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${var.region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.app[*].id
  security_group_ids  = [aws_security_group.vpc_endpoints.id]
  # Private DNS is the half people forget: without it the AWS SDK still
  # resolves the public endpoint name to a public IP and the traffic leaves
  # through the NAT gateway, so "PrivateLink" appears in the diagram and not on
  # the wire.
  private_dns_enabled = true

  tags = merge(local.common_tags, {
    Name = "vpce-${local.base_name}-${replace(each.value, ".", "-")}"
  })
}

# S3 as a gateway endpoint (free, route-table based) rather than an interface
# endpoint. Used for ECR layer pulls, which are S3-backed.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = concat(aws_route_table.app[*].id, [aws_route_table.data.id])

  tags = merge(local.common_tags, {
    Name = "vpce-${local.base_name}-s3"
  })
}

# ---------------------------------------------------------------------------
# Route 53 private hosted zone (SPEC §8 row 1)
# ---------------------------------------------------------------------------

resource "aws_route53_zone" "internal" {
  name    = var.internal_domain
  comment = "AgentGate internal service names. Resolves only inside the VPC."

  vpc {
    vpc_id = aws_vpc.this.id
  }

  tags = local.common_tags

  lifecycle {
    # external-dns manages records inside the zone; Terraform owns only the
    # zone itself.
    ignore_changes = [vpc]
  }
}

# ---------------------------------------------------------------------------
# Security groups for the data tier
# ---------------------------------------------------------------------------

resource "aws_security_group" "rds" {
  name        = "sg-${local.base_name}-rds"
  description = "PostgreSQL, reachable only from the EKS node subnets."
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "PostgreSQL from application subnets"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = local.app_subnet_cidrs
  }

  # No egress rules at all. A database that can open outbound connections is a
  # database that can exfiltrate.

  tags = merge(local.common_tags, {
    Name = "sg-${local.base_name}-rds"
  })
}

resource "aws_security_group" "elasticache" {
  name        = "sg-${local.base_name}-redis"
  description = "Redis, reachable only from the EKS node subnets, TLS port only."
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "Redis from application subnets"
    from_port   = 6379
    to_port     = 6379
    protocol    = "tcp"
    cidr_blocks = local.app_subnet_cidrs
  }

  tags = merge(local.common_tags, {
    Name = "sg-${local.base_name}-redis"
  })
}

resource "aws_security_group" "eks_nodes" {
  name        = "sg-${local.base_name}-nodes"
  description = "EKS worker nodes."
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "Intra-VPC"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = [var.vpc_cidr]
  }

  ingress {
    description = "On-prem inference responses over Direct Connect"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.onprem_inference_cidr]
  }

  egress {
    description = "All egress, constrained by the route tables and Network Firewall rather than here. Pod-level rules are NetworkPolicies (deploy/k8s/base/networkpolicy.yaml); a node security group cannot see inside the CNI."
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, {
    Name = "sg-${local.base_name}-nodes"
  })
}

resource "aws_security_group" "alb" {
  name        = "sg-${local.base_name}-alb"
  description = "Internal ALB in front of the gateway."
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "HTTPS from within the VPC and from on-prem"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr, var.onprem_inference_cidr]
  }

  egress {
    description = "To the node subnets"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = local.app_subnet_cidrs
  }

  tags = merge(local.common_tags, {
    Name = "sg-${local.base_name}-alb"
  })
}

# ---------------------------------------------------------------------------
# Flow logs
# ---------------------------------------------------------------------------

resource "aws_flow_log" "vpc" {
  vpc_id               = aws_vpc.this.id
  traffic_type         = "ALL"
  log_destination_type = "cloud-watch-logs"
  log_destination      = aws_cloudwatch_log_group.vpc_flow.arn
  iam_role_arn         = aws_iam_role.flow_logs.arn

  # The default format omits the fields that make a flow log useful during a
  # data-flow review: which endpoint served the traffic, and whether it left
  # the VPC at all.
  log_format = "$${version} $${account-id} $${interface-id} $${srcaddr} $${dstaddr} $${srcport} $${dstport} $${protocol} $${packets} $${bytes} $${start} $${end} $${action} $${log-status} $${vpc-id} $${subnet-id} $${instance-id} $${tcp-flags} $${type} $${pkt-srcaddr} $${pkt-dstaddr} $${flow-direction} $${traffic-path}"

  tags = local.common_tags
}
