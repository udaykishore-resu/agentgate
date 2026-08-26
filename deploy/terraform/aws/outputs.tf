# Outputs.
#
# Consumed by the deploy pipeline, which patches them into the Kubernetes
# overlay before applying. Nothing here is a secret: credentials live in
# Secrets Manager and are read at runtime through IRSA, because a value in an
# output is a value in the state file's S3 bucket.

output "region" {
  description = "Region this environment is deployed in."
  value       = var.region
}

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

output "eks_cluster_name" {
  description = "EKS cluster name."
  value       = aws_eks_cluster.this.name
}

output "eks_cluster_endpoint" {
  description = "API server endpoint. Private only — reachable from inside the VPC or over Direct Connect."
  value       = aws_eks_cluster.this.endpoint
}

output "eks_oidc_issuer_url" {
  description = "Cluster OIDC issuer. The trust anchor for every IRSA role, and what an auditor checks when asked how workload identity is bound."
  value       = aws_eks_cluster.this.identity[0].oidc[0].issuer
}

output "eks_oidc_provider_arn" {
  description = "IAM OIDC provider ARN, for roles created outside this configuration."
  value       = aws_iam_openid_connect_provider.eks.arn
}

# ---------------------------------------------------------------------------
# Workload identity — patched into the ServiceAccount annotations
# ---------------------------------------------------------------------------

output "irsa_role_arns" {
  description = "Map of ServiceAccount name to IAM role ARN. Goes into eks.amazonaws.com/role-arn on each ServiceAccount in deploy/k8s/base/serviceaccounts.yaml."
  value       = { for k, v in aws_iam_role.irsa : k => v.arn }
}

# ---------------------------------------------------------------------------
# Dependencies — endpoints only, never credentials
# ---------------------------------------------------------------------------

output "rds_endpoint" {
  description = "PostgreSQL endpoint (host:port). Reachable only from the app subnets; connections authenticate with IAM."
  value       = aws_db_instance.this.endpoint
}

output "rds_database_name" {
  description = "Application database name."
  value       = aws_db_instance.this.db_name
}

output "rds_master_user_secret_arn" {
  description = "ARN of the RDS-managed master password secret. The ARN is not sensitive; the value is, and stays in Secrets Manager."
  value       = aws_db_instance.this.master_user_secret[0].secret_arn
}

output "redis_primary_endpoint" {
  description = "ElastiCache primary endpoint. TLS required; authentication is IAM."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "redis_reader_endpoint" {
  description = "ElastiCache reader endpoint, for read-heavy cache lookups."
  value       = aws_elasticache_replication_group.this.reader_endpoint_address
}

output "secrets_manager_arns" {
  description = "Secret containers created for this environment. Values are written out of band by the PKI and network pipelines."
  value = {
    token_signing_key = aws_secretsmanager_secret.token_signing_key.arn
    onprem_ca_bundle  = aws_secretsmanager_secret.onprem_ca_bundle.arn
    servicenow_token  = aws_secretsmanager_secret.servicenow_token.arn
  }
}

output "bedrock_allowed_models" {
  description = "Models the gateway's IAM policy permits. A pool referencing anything else fails at the IAM boundary rather than being quietly billable."
  value       = var.bedrock_model_ids
}

output "usage_stream_name" {
  description = "Kinesis stream carrying UsageRecords (SPEC §6)."
  value       = aws_kinesis_stream.usage.name
}

# ---------------------------------------------------------------------------
# Edge
# ---------------------------------------------------------------------------

output "gateway_url" {
  description = "Internal gateway URL. Resolves only inside the VPC or over Direct Connect."
  value       = "https://gateway.${var.internal_domain}"
}

output "alb_dns_name" {
  description = "ALB DNS name, behind the Route 53 alias records."
  value       = aws_lb.gateway.dns_name
}

output "migration_target_group_arns" {
  description = "Target groups whose listener weights implement the SPEC §7 canary. Roll back by changing a weight, never by deploying."
  value = {
    agentgate = aws_lb_target_group.gateway.arn
    legacy    = aws_lb_target_group.legacy_gateway.arn
  }
}

# ---------------------------------------------------------------------------
# Observability
# ---------------------------------------------------------------------------

output "prometheus_workspace_id" {
  description = "Amazon Managed Prometheus workspace id."
  value       = aws_prometheus_workspace.this.id
}

output "prometheus_query_endpoint" {
  description = "AMP query endpoint, for AGENTGATE_PROM_URL when running against managed Prometheus rather than in-cluster."
  value       = aws_prometheus_workspace.this.prometheus_endpoint
}

output "sns_topic_arns" {
  description = "Alert destinations. `page` wakes the on-call; `ticket` goes to the queue."
  value = {
    page   = aws_sns_topic.page.arn
    ticket = aws_sns_topic.ticket.arn
  }
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

output "vpc_id" {
  description = "VPC id, for peering an agent-hosting VPC into this one."
  value       = aws_vpc.this.id
}

output "app_subnet_ids" {
  description = "Application subnet ids (EKS nodes and pods)."
  value       = aws_subnet.app[*].id
}

output "nat_public_ips" {
  description = "The outbound IPs this environment egresses from. Third parties allowlist these addresses."
  value       = aws_eip.nat[*].public_ip
}

output "private_hosted_zone_id" {
  description = "Route 53 private hosted zone for the internal service names."
  value       = aws_route53_zone.internal.zone_id
}

# ---------------------------------------------------------------------------
# Convenience
# ---------------------------------------------------------------------------

output "kubectl_config_command" {
  description = "Command to obtain cluster credentials. The API server is private, so this must run from inside the VPC or over Direct Connect."
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${aws_eks_cluster.this.name}"
}
