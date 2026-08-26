# Observability plane (SPEC §4.4, §6).
#
# AgentGate's own pipeline runs in-cluster; what is created here is the
# AWS-native side of it: Amazon Managed Prometheus for metrics that must
# outlive the cluster, CloudWatch for platform diagnostics, X-Ray for the
# managed trace backend, and Kinesis for the immutable UsageRecord stream.

# ---------------------------------------------------------------------------
# Log groups
# ---------------------------------------------------------------------------
# Created explicitly rather than left to the services. An implicitly created
# log group has never-expiring retention and no KMS key, which is how a
# forgotten debug log becomes a compliance finding two years later.

resource "aws_cloudwatch_log_group" "eks" {
  name              = "/aws/eks/eks-${local.base_name}/cluster"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.logs.arn
  tags              = local.common_tags
}

resource "aws_cloudwatch_log_group" "vpc_flow" {
  name              = "/aws/vpc/${local.base_name}/flow-logs"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.logs.arn
  tags              = local.common_tags
}

resource "aws_cloudwatch_log_group" "redis_slow" {
  name              = "/aws/elasticache/${local.base_name}/slow-log"
  retention_in_days = 30
  kms_key_id        = aws_kms_key.logs.arn
  tags              = local.common_tags
}

resource "aws_cloudwatch_log_group" "network_firewall" {
  name              = "/aws/network-firewall/${local.base_name}/alert"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.logs.arn
  tags              = local.common_tags
}

resource "aws_cloudwatch_log_group" "agentgate" {
  name              = "/agentgate/${var.environment}"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.logs.arn
  tags              = local.common_tags
}

# The record of what was allowed out and what was denied — the operational
# counterpart to the FQDN allowlist, and the first thing to check when a
# dependency "suddenly stopped working" after a review.
resource "aws_networkfirewall_logging_configuration" "this" {
  firewall_arn = aws_networkfirewall_firewall.this.arn

  logging_configuration {
    log_destination_config {
      log_type             = "ALERT"
      log_destination_type = "CloudWatchLogs"

      log_destination = {
        logGroup = aws_cloudwatch_log_group.network_firewall.name
      }
    }
  }
}

# ---------------------------------------------------------------------------
# Amazon Managed Prometheus
# ---------------------------------------------------------------------------

resource "aws_prometheus_workspace" "this" {
  alias = "amp-${local.base_name}"
  tags  = local.common_tags

  logging_configuration {
    log_group_arn = "${aws_cloudwatch_log_group.agentgate.arn}:*"
  }
}

# The SLO recording and alerting rules from deploy/prometheus/rules, loaded
# into the managed workspace. Same expressions, same thresholds, same
# runbook links as the self-hosted path — an environment where the rules
# differ is an environment where the SLO means something different.
resource "aws_prometheus_rule_group_namespace" "slo" {
  name         = "agentgate-slo"
  workspace_id = aws_prometheus_workspace.this.id
  data         = file("${path.module}/../../prometheus/rules/slo.rules.yml")
}

resource "aws_prometheus_rule_group_namespace" "operational" {
  name         = "agentgate-operational"
  workspace_id = aws_prometheus_workspace.this.id
  data         = file("${path.module}/../../prometheus/rules/operational.rules.yml")
}

resource "aws_prometheus_alert_manager_definition" "this" {
  workspace_id = aws_prometheus_workspace.this.id

  # Managed Alertmanager speaks a subset of upstream Alertmanager config and
  # delivers through SNS rather than directly to PagerDuty, so this is not the
  # file in deploy/prometheus/alertmanager.yml. The routing decision it encodes
  # is the same one: severity decides page versus ticket.
  definition = <<-YAML
    alertmanager_config: |
      route:
        receiver: platform-ticket
        group_by: [alertname, env, service]
        group_wait: 30s
        group_interval: 5m
        repeat_interval: 4h
        routes:
          - matchers:
              - env =~ "dev|staging"
            receiver: platform-ticket
            repeat_interval: 24h
          - matchers:
              - notify = "page"
            receiver: platform-page
            group_wait: 15s
            repeat_interval: 1h
      receivers:
        - name: platform-page
          sns_configs:
            - topic_arn: ${aws_sns_topic.page.arn}
              sigv4:
                region: ${var.region}
              subject: '{{ template "__subject" . }}'
        - name: platform-ticket
          sns_configs:
            - topic_arn: ${aws_sns_topic.ticket.arn}
              sigv4:
                region: ${var.region}
              subject: '{{ template "__subject" . }}'
  YAML
}

# ---------------------------------------------------------------------------
# X-Ray
# ---------------------------------------------------------------------------

resource "aws_xray_group" "agentgate" {
  group_name        = "agentgate-${var.environment}"
  filter_expression = "annotation.agentgate_env = \"${var.environment}\""

  insights_configuration {
    insights_enabled      = true
    notifications_enabled = true
  }

  tags = local.common_tags
}

resource "aws_xray_sampling_rule" "errors" {
  rule_name = "agentgate-${var.environment}-errors"
  priority  = 1000
  version   = 1
  # X-Ray sampling is head-based and cannot see the outcome, so it cannot
  # implement the tail policy in SPEC §4.4. The real decision is made by the
  # collector's tail_sampling processor; this rule only governs what the AWS
  # SDKs inside the platform's own dependencies emit.
  reservoir_size = 10
  fixed_rate     = 0.05
  service_name   = "*"
  service_type   = "*"
  host           = "*"
  http_method    = "*"
  url_path       = "*"
  resource_arn   = "*"

  tags = local.common_tags
}

# ---------------------------------------------------------------------------
# Usage stream (SPEC §6)
# ---------------------------------------------------------------------------

resource "aws_kinesis_stream" "usage" {
  name             = "kds-${local.base_name}-usage"
  retention_period = 168 # 7 days, matching the Event Hubs side
  encryption_type  = "KMS"
  kms_key_id       = aws_kms_key.data.arn
  tags             = local.common_tags

  stream_mode_details {
    stream_mode = "PROVISIONED"
  }

  shard_count = var.usage_stream_shards

  # Per-shard metrics: without these, "the usage stream is behind" is not a
  # question CloudWatch can answer, and chargeback silently lags.
  shard_level_metrics = [
    "IncomingRecords",
    "OutgoingRecords",
    "WriteProvisionedThroughputExceeded",
    "ReadProvisionedThroughputExceeded",
    "IteratorAgeMilliseconds",
  ]
}

# ---------------------------------------------------------------------------
# Alerting destinations
# ---------------------------------------------------------------------------

resource "aws_sns_topic" "page" {
  name              = "sns-${local.base_name}-page"
  kms_master_key_id = aws_kms_key.logs.arn
  tags              = local.common_tags
}

resource "aws_sns_topic" "ticket" {
  name              = "sns-${local.base_name}-ticket"
  kms_master_key_id = aws_kms_key.logs.arn
  tags              = local.common_tags
}

# ---------------------------------------------------------------------------
# Platform-level alarms
# ---------------------------------------------------------------------------
# Application SLO alerts live in deploy/prometheus/rules, where they can be
# reviewed next to the SLIs they measure. What is here is the small set of
# infrastructure conditions Prometheus cannot see, because they take
# Prometheus itself down.

resource "aws_cloudwatch_metric_alarm" "rds_storage" {
  alarm_name          = "${local.base_name}-rds-free-storage"
  alarm_description   = "RDS free storage below 20GB. At zero the instance goes read-only and the promotion gate stops recording approvals."
  namespace           = "AWS/RDS"
  metric_name         = "FreeStorageSpace"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 2
  threshold           = 21474836480
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.page.arn]
  ok_actions          = [aws_sns_topic.page.arn]
  tags                = local.common_tags

  dimensions = {
    DBInstanceIdentifier = aws_db_instance.this.identifier
  }
}

resource "aws_cloudwatch_metric_alarm" "redis_memory" {
  alarm_name          = "${local.base_name}-redis-memory"
  alarm_description   = "Redis memory above 85%. Eviction of rate-limit buckets makes quota enforcement approximate, and it fails open."
  namespace           = "AWS/ElastiCache"
  metric_name         = "DatabaseMemoryUsagePercentage"
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 5
  threshold           = 85
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.page.arn]
  tags                = local.common_tags

  dimensions = {
    ReplicationGroupId = aws_elasticache_replication_group.this.id
  }
}

resource "aws_cloudwatch_metric_alarm" "usage_stream_lag" {
  alarm_name          = "${local.base_name}-usage-stream-lag"
  alarm_description   = "UsageRecord consumers are more than 5 minutes behind. Chargeback rollups are stale and the cost-anomaly detector is scoring old data."
  namespace           = "AWS/Kinesis"
  metric_name         = "GetRecords.IteratorAgeMilliseconds"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = 300000
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.ticket.arn]
  tags                = local.common_tags

  dimensions = {
    StreamName = aws_kinesis_stream.usage.name
  }
}

resource "aws_cloudwatch_metric_alarm" "bedrock_throttles" {
  alarm_name          = "${local.base_name}-bedrock-throttles"
  alarm_description   = "Bedrock is throttling. The gateway will fail over to the on-prem tier, raising TTFT and shifting cost."
  namespace           = "AWS/Bedrock"
  metric_name         = "InvocationThrottles"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 2
  threshold           = 10
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.page.arn]
  tags                = local.common_tags
}

resource "aws_cloudwatch_metric_alarm" "firewall_drops" {
  alarm_name          = "${local.base_name}-firewall-drops"
  alarm_description   = "Network Firewall is dropping outbound flows. Usually a new dependency that is not on the FQDN allowlist; occasionally something that should not be calling out at all."
  namespace           = "AWS/NetworkFirewall"
  metric_name         = "DroppedPackets"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 2
  threshold           = 100
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.ticket.arn]
  tags                = local.common_tags

  dimensions = {
    FirewallName = aws_networkfirewall_firewall.this.name
  }
}
