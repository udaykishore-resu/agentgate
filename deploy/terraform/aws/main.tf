# AgentGate — AWS platform.
#
# COMPUTE CHOICE: EKS, not ECS Fargate.
#
# Same reasoning as the Azure side, and it lands the same way: the telemetry
# design (SPEC §4.4) needs a node-local DaemonSet feeding a trace-ID-sharded
# collector pool, and SPEC §8 needs default-deny east-west with per-path
# allows. ECS Fargate has no node to run a DaemonSet on — the equivalent is a
# sidecar per task, which multiplies collector instances by task count and
# destroys the batching that makes the first tier worth having.
#
# ECS Fargate remains a good host for the *agents*, which is why
# `agentgate.runtime` in SPEC §4.1 is open-ended.

# ---------------------------------------------------------------------------
# KMS — one key per data domain
# ---------------------------------------------------------------------------
# Separate keys, not one platform key: a key is the smallest unit of "revoke
# access to this data without revoking access to that data", and the token
# signing key must be revocable independently of, say, log data.

resource "aws_kms_key" "secrets" {
  description             = "AgentGate secrets: token signing key, database credentials, on-prem CA bundle."
  enable_key_rotation     = true
  deletion_window_in_days = 30
  tags                    = local.common_tags
}

resource "aws_kms_alias" "secrets" {
  name          = "alias/${local.base_name}-secrets"
  target_key_id = aws_kms_key.secrets.key_id
}

resource "aws_kms_key" "data" {
  description             = "AgentGate data at rest: RDS, ElastiCache, Kinesis."
  enable_key_rotation     = true
  deletion_window_in_days = 30
  tags                    = local.common_tags
}

resource "aws_kms_alias" "data" {
  name          = "alias/${local.base_name}-data"
  target_key_id = aws_kms_key.data.key_id
}

resource "aws_kms_key" "logs" {
  description             = "AgentGate telemetry at rest: CloudWatch log groups."
  enable_key_rotation     = true
  deletion_window_in_days = 30
  tags                    = local.common_tags

  policy = data.aws_iam_policy_document.logs_kms.json
}

resource "aws_kms_alias" "logs" {
  name          = "alias/${local.base_name}-logs"
  target_key_id = aws_kms_key.logs.key_id
}

data "aws_iam_policy_document" "logs_kms" {
  statement {
    sid       = "AllowAccountAdministration"
    effect    = "Allow"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["arn:${data.aws_partition.current.partition}:iam::${var.account_id}:root"]
    }
  }

  statement {
    sid    = "AllowCloudWatchLogs"
    effect = "Allow"
    actions = [
      "kms:Encrypt*",
      "kms:Decrypt*",
      "kms:ReEncrypt*",
      "kms:GenerateDataKey*",
      "kms:Describe*",
    ]
    resources = ["*"]

    principals {
      type        = "Service"
      identifiers = ["logs.${var.region}.amazonaws.com"]
    }

    # Scope the grant to this account's log groups so the key cannot be used
    # by a log group in another account that happens to know its ARN.
    condition {
      test     = "ArnLike"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:${data.aws_partition.current.partition}:logs:${var.region}:${var.account_id}:log-group:*"]
    }
  }
}

# ---------------------------------------------------------------------------
# EKS
# ---------------------------------------------------------------------------

resource "aws_eks_cluster" "this" {
  name     = "eks-${local.base_name}"
  role_arn = aws_iam_role.eks_cluster.arn
  version  = var.kubernetes_version
  tags     = local.common_tags

  vpc_config {
    subnet_ids              = aws_subnet.app[*].id
    security_group_ids      = [aws_security_group.eks_nodes.id]
    endpoint_private_access = true
    # No public API server endpoint. Access is from inside the VPC or over
    # Direct Connect; CI runs on self-hosted runners in the app subnets.
    endpoint_public_access = false
  }

  access_config {
    # API-only: the aws-auth ConfigMap is not a security boundary anyone can
    # audit. Access entries are IAM objects with CloudTrail behind them.
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = false
  }

  # Envelope encryption for Kubernetes Secrets. Even though every real secret
  # comes from Secrets Manager at runtime, anything that does land in etcd is
  # encrypted with a key this account controls and can revoke.
  encryption_config {
    provider {
      key_arn = aws_kms_key.secrets.arn
    }
    resources = ["secrets"]
  }

  enabled_cluster_log_types = ["api", "audit", "authenticator", "controllerManager", "scheduler"]

  depends_on = [
    aws_iam_role_policy_attachment.eks_cluster,
    aws_cloudwatch_log_group.eks,
  ]
}

resource "aws_eks_node_group" "this" {
  for_each = var.node_groups

  cluster_name    = aws_eks_cluster.this.name
  node_group_name = each.key
  node_role_arn   = aws_iam_role.eks_nodes.arn
  subnet_ids      = aws_subnet.app[*].id
  instance_types  = each.value.instance_types
  capacity_type   = each.value.capacity_type
  ami_type        = "AL2023_x86_64_STANDARD"
  tags            = local.common_tags

  scaling_config {
    min_size     = each.value.min_size
    max_size     = each.value.max_size
    desired_size = each.value.desired_size
  }

  update_config {
    # One node at a time. Draining a gateway node cancels the streams its pods
    # are carrying, so a faster rollout is paid for in cancelled generations.
    max_unavailable = 1
  }

  labels = {
    "agentgate.io/pool" = each.key
  }

  dynamic "taint" {
    # The system group is tainted so only cluster add-ons tolerate it,
    # mirroring `only_critical_addons_enabled` on the AKS side.
    for_each = each.key == "system" ? [1] : []

    content {
      key    = "CriticalAddonsOnly"
      value  = "true"
      effect = "NO_SCHEDULE"
    }
  }

  lifecycle {
    # Owned by the cluster autoscaler after creation.
    ignore_changes = [scaling_config[0].desired_size]
  }

  depends_on = [aws_iam_role_policy_attachment.eks_nodes]
}

resource "aws_eks_addon" "this" {
  for_each = toset(["vpc-cni", "coredns", "kube-proxy", "eks-pod-identity-agent"])

  cluster_name = aws_eks_cluster.this.name
  addon_name   = each.value
  tags         = local.common_tags

  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  depends_on = [aws_eks_node_group.this]
}

# ---------------------------------------------------------------------------
# RDS PostgreSQL
# ---------------------------------------------------------------------------

resource "aws_db_subnet_group" "this" {
  name       = "dbsg-${local.base_name}"
  subnet_ids = aws_subnet.data[*].id
  tags       = local.common_tags
}

resource "aws_db_parameter_group" "this" {
  name   = "pg-${local.base_name}"
  family = "postgres16"
  tags   = local.common_tags

  parameter {
    name  = "shared_preload_libraries"
    value = "pg_stat_statements"
    # Static parameter: takes effect only on reboot, and Terraform must be
    # told or the apply reports success while the setting is not live.
    apply_method = "pending-reboot"
  }

  parameter {
    name  = "pg_stat_statements.track"
    value = "top"
  }

  parameter {
    # DDL only. Logging every statement would put registration payloads —
    # including owner emails — into a log with different retention and access
    # from the application's own telemetry.
    name  = "log_statement"
    value = "ddl"
  }

  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
}

resource "aws_db_instance" "this" {
  identifier     = "rds-${local.base_name}"
  engine         = "postgres"
  engine_version = var.rds.engine_version
  instance_class = var.rds.instance_class
  tags           = local.common_tags

  allocated_storage     = var.rds.allocated_storage
  max_allocated_storage = var.rds.max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = aws_kms_key.data.arn

  db_name  = "agentgate"
  username = "agentgate_admin"
  # No password stored anywhere in this configuration. RDS generates and
  # manages it in Secrets Manager, and the applications connect with IAM
  # authentication instead (see identity.tf).
  manage_master_user_password   = true
  master_user_secret_kms_key_id = aws_kms_key.secrets.arn

  iam_database_authentication_enabled = true

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  parameter_group_name   = aws_db_parameter_group.this.name
  publicly_accessible    = false
  multi_az               = var.rds.multi_az

  backup_retention_period = var.rds.backup_retention_period
  backup_window           = "02:00-03:00"
  maintenance_window      = "tue:03:30-tue:04:30"
  copy_tags_to_snapshot   = true

  performance_insights_enabled          = true
  performance_insights_kms_key_id       = aws_kms_key.data.arn
  performance_insights_retention_period = 31
  enabled_cloudwatch_logs_exports       = ["postgresql", "upgrade"]

  auto_minor_version_upgrade = true
  deletion_protection        = var.rds.deletion_protection
  # A final snapshot is the difference between "we lost the registry" and "we
  # lost twenty minutes".
  skip_final_snapshot       = false
  final_snapshot_identifier = "rds-${local.base_name}-final-${local.suffix}"

  lifecycle {
    prevent_destroy = true
  }
}

# ---------------------------------------------------------------------------
# ElastiCache Redis
# ---------------------------------------------------------------------------

resource "aws_elasticache_subnet_group" "this" {
  name       = "ecsg-${local.base_name}"
  subnet_ids = aws_subnet.data[*].id
  tags       = local.common_tags
}

resource "aws_elasticache_parameter_group" "this" {
  name   = "ecpg-${local.base_name}"
  family = "redis7"
  tags   = local.common_tags

  parameter {
    # Rate-limit buckets and cache entries are both reconstructible, so
    # eviction is preferable to refusing writes when memory fills.
    name  = "maxmemory-policy"
    value = "allkeys-lru"
  }
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "ec-${local.base_name}"
  description          = "AgentGate rate-limit buckets and response cache."
  engine               = "redis"
  engine_version       = var.elasticache.engine_version
  node_type            = var.elasticache.node_type
  num_cache_clusters   = var.elasticache.num_cache_clusters
  port                 = 6379
  tags                 = local.common_tags

  subnet_group_name    = aws_elasticache_subnet_group.this.name
  security_group_ids   = [aws_security_group.elasticache.id]
  parameter_group_name = aws_elasticache_parameter_group.this.name

  automatic_failover_enabled = var.elasticache.num_cache_clusters > 1
  multi_az_enabled           = var.elasticache.num_cache_clusters > 1

  at_rest_encryption_enabled = true
  kms_key_id                 = aws_kms_key.data.arn
  transit_encryption_enabled = true
  # IAM authentication rather than an auth token: same "no static credential"
  # rule as everywhere else, and it makes access revocable through IAM.
  transit_encryption_mode = "required"

  # Persistence off on purpose — see the parameter group. An AOF fsync on the
  # quota path would add latency to every request to protect data we are happy
  # to lose.
  snapshot_retention_limit = 0

  maintenance_window         = "tue:04:30-tue:05:30"
  auto_minor_version_upgrade = true

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.redis_slow.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "slow-log"
  }
}

# ---------------------------------------------------------------------------
# Secrets Manager
# ---------------------------------------------------------------------------
# Values are written out of band (by the PKI pipeline for the signing key, by
# the network team for the on-prem CA bundle). Terraform creates the container
# and the access policy; it never holds the value, because a value in
# Terraform is a value in state.

resource "aws_secretsmanager_secret" "token_signing_key" {
  name        = "${local.base_name}/token-signing-key"
  description = "Control plane token signing key (SPEC §1.1). Read by the controlplane IRSA role only."
  kms_key_id  = aws_kms_key.secrets.arn
  # 30 days: long enough to recover from a mistaken delete, and the key itself
  # is backed up by the PKI system independently.
  recovery_window_in_days = 30
  tags                    = local.common_tags
}

resource "aws_secretsmanager_secret" "onprem_ca_bundle" {
  name                    = "${local.base_name}/onprem-ca-bundle"
  description             = "Internal CA bundle for on-prem inference certificate pinning (SPEC §8 row 3)."
  kms_key_id              = aws_kms_key.secrets.arn
  recovery_window_in_days = 30
  tags                    = local.common_tags
}

resource "aws_secretsmanager_secret" "servicenow_token" {
  name                    = "${local.base_name}/servicenow-token"
  # Rotated on the 90-day clock in SPEC §1.1 by the platform's rotation job,
  # which writes a new version through the Secrets Manager API. There is no
  # rotation Lambda here because the credential is issued by ServiceNow, not
  # by AWS, and a rotation function that cannot mint the new value is theatre.
  description             = "ServiceNow API token for promotion-gate change records (SPEC §1.4)."
  kms_key_id              = aws_kms_key.secrets.arn
  recovery_window_in_days = 30
  tags                    = local.common_tags
}
