# IAM: cluster roles and IRSA (SPEC §1.1 mode 1, SPEC §10).
#
# IRSA is the AWS half of the same idea as Entra workload identity federation.
# The cluster's OIDC provider is registered with IAM, and each role's trust
# policy conditions on the exact `system:serviceaccount:<ns>:<name>` subject.
# The chain:
#
#   pod -> projected SA token (signed by the cluster's OIDC issuer)
#       -> sts:AssumeRoleWithWebIdentity
#       -> temporary credentials scoped to one role
#
# No static access key exists anywhere in this configuration.
#
# The grants are asymmetric on purpose. The gateway may invoke models and read
# its own secrets; only the control plane may read the token signing key. A
# gateway compromise is bad; a gateway compromise that could mint agent
# identities would be unrecoverable.

resource "aws_iam_openid_connect_provider" "eks" {
  url             = aws_eks_cluster.this.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.eks_oidc.certificates[0].sha1_fingerprint]
  tags            = local.common_tags
}

# ---------------------------------------------------------------------------
# Cluster and node roles
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "eks_cluster_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eks_cluster" {
  name               = "${local.base_name}-eks-cluster"
  assume_role_policy = data.aws_iam_policy_document.eks_cluster_assume.json
  tags               = local.common_tags
}

resource "aws_iam_role_policy_attachment" "eks_cluster" {
  role       = aws_iam_role.eks_cluster.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonEKSClusterPolicy"
}

data "aws_iam_policy_document" "eks_nodes_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "eks_nodes" {
  name               = "${local.base_name}-eks-nodes"
  assume_role_policy = data.aws_iam_policy_document.eks_nodes_assume.json
  tags               = local.common_tags
}

resource "aws_iam_role_policy_attachment" "eks_nodes" {
  for_each = toset([
    "AmazonEKSWorkerNodePolicy",
    "AmazonEKS_CNI_Policy",
    "AmazonEC2ContainerRegistryReadOnly",
    # SSM instead of SSH. There are no key pairs and no bastion; a node is
    # reached through Session Manager, which is logged.
    "AmazonSSMManagedInstanceCore",
  ])

  role       = aws_iam_role.eks_nodes.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/${each.value}"
}

# ---------------------------------------------------------------------------
# IRSA roles
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "irsa_assume" {
  for_each = local.workload_identities

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.eks.arn]
    }

    # The subject condition is what binds this role to one ServiceAccount in
    # one namespace. Without it — or with a wildcard — any pod in the cluster
    # could assume any role, and IRSA becomes a very elaborate way of sharing
    # one credential.
    condition {
      test     = "StringEquals"
      variable = "${replace(aws_iam_openid_connect_provider.eks.url, "https://", "")}:sub"
      values   = ["system:serviceaccount:${local.k8s_namespace}:${each.value.sa}"]
    }

    condition {
      test     = "StringEquals"
      variable = "${replace(aws_iam_openid_connect_provider.eks.url, "https://", "")}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "irsa" {
  for_each = local.workload_identities

  name               = "${local.base_name}-${each.key}"
  description        = each.value.description
  assume_role_policy = data.aws_iam_policy_document.irsa_assume[each.key].json
  # An hour matches the projected token lifetime; longer would outlive the
  # trust that produced it.
  max_session_duration = 3600
  tags                 = local.common_tags
}

# ---------------------------------------------------------------------------
# gateway
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "gateway" {
  # Bedrock, restricted to an explicit model allowlist. A pool referencing a
  # model that is not in `bedrock_model_ids` fails here, at the IAM boundary,
  # rather than being quietly billable.
  statement {
    sid    = "InvokeAllowlistedBedrockModels"
    effect = "Allow"
    actions = [
      "bedrock:InvokeModel",
      "bedrock:InvokeModelWithResponseStream",
    ]
    resources = [
      for m in var.bedrock_model_ids :
      "arn:${data.aws_partition.current.partition}:bedrock:${var.region}::foundation-model/${m}"
    ]
  }

  statement {
    sid    = "ReadOwnSecrets"
    effect = "Allow"
    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
    ]
    resources = [
      aws_secretsmanager_secret.onprem_ca_bundle.arn,
      aws_db_instance.this.master_user_secret[0].secret_arn,
    ]
  }

  statement {
    sid       = "DecryptSecrets"
    effect    = "Allow"
    actions   = ["kms:Decrypt"]
    resources = [aws_kms_key.secrets.arn]
  }

  # Writes UsageRecords (SPEC §6). Put-only: the stream is the immutable input
  # to chargeback, and the producer must not be able to read back or delete
  # what it wrote.
  statement {
    sid    = "WriteUsageStream"
    effect = "Allow"
    actions = [
      "kinesis:PutRecord",
      "kinesis:PutRecords",
    ]
    resources = [aws_kinesis_stream.usage.arn]
  }

  statement {
    sid    = "ConnectToRedisAndDatabase"
    effect = "Allow"
    actions = [
      "elasticache:Connect",
      "rds-db:connect",
    ]
    resources = [
      aws_elasticache_replication_group.this.arn,
      "arn:${data.aws_partition.current.partition}:rds-db:${var.region}:${var.account_id}:dbuser:${aws_db_instance.this.resource_id}/gateway",
    ]
  }
}

resource "aws_iam_role_policy" "gateway" {
  name   = "gateway"
  role   = aws_iam_role.irsa["gateway"].id
  policy = data.aws_iam_policy_document.gateway.json
}

# ---------------------------------------------------------------------------
# controlplane — the only identity that can read the signing key
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "controlplane" {
  statement {
    sid    = "ReadTokenSigningKey"
    effect = "Allow"
    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
    ]
    resources = [
      aws_secretsmanager_secret.token_signing_key.arn,
      aws_secretsmanager_secret.servicenow_token.arn,
      aws_db_instance.this.master_user_secret[0].secret_arn,
    ]
  }

  statement {
    sid       = "DecryptSecrets"
    effect    = "Allow"
    actions   = ["kms:Decrypt"]
    resources = [aws_kms_key.secrets.arn]
  }

  statement {
    sid       = "ConnectToDatabase"
    effect    = "Allow"
    actions   = ["rds-db:connect"]
    resources = ["arn:${data.aws_partition.current.partition}:rds-db:${var.region}:${var.account_id}:dbuser:${aws_db_instance.this.resource_id}/controlplane"]
  }
}

resource "aws_iam_role_policy" "controlplane" {
  name   = "controlplane"
  role   = aws_iam_role.irsa["controlplane"].id
  policy = data.aws_iam_policy_document.controlplane.json
}

# ---------------------------------------------------------------------------
# fleetview — read-only everywhere
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "fleetview" {
  statement {
    sid       = "ConnectToDatabaseReadOnly"
    effect    = "Allow"
    actions   = ["rds-db:connect"]
    resources = ["arn:${data.aws_partition.current.partition}:rds-db:${var.region}:${var.account_id}:dbuser:${aws_db_instance.this.resource_id}/fleetview"]
  }

  statement {
    sid    = "ConsumeUsageStream"
    effect = "Allow"
    actions = [
      "kinesis:GetRecords",
      "kinesis:GetShardIterator",
      "kinesis:DescribeStream",
      "kinesis:ListShards",
      "kinesis:SubscribeToShard",
    ]
    resources = [aws_kinesis_stream.usage.arn]
  }

  statement {
    sid    = "QueryManagedPrometheus"
    effect = "Allow"
    actions = [
      "aps:QueryMetrics",
      "aps:GetLabels",
      "aps:GetSeries",
      "aps:GetMetricMetadata",
    ]
    resources = [aws_prometheus_workspace.this.arn]
  }
}

resource "aws_iam_role_policy" "fleetview" {
  name   = "fleetview"
  role   = aws_iam_role.irsa["fleetview"].id
  policy = data.aws_iam_policy_document.fleetview.json
}

# ---------------------------------------------------------------------------
# guardrails
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "guardrails" {
  # Bedrock Guardrails as the managed content-safety backend for `callout`
  # mode. Apply-only: the service can evaluate content against a guardrail but
  # cannot create or modify one, so the policy set is not editable from inside
  # the data path.
  statement {
    sid       = "ApplyGuardrail"
    effect    = "Allow"
    actions   = ["bedrock:ApplyGuardrail"]
    resources = ["arn:${data.aws_partition.current.partition}:bedrock:${var.region}:${var.account_id}:guardrail/*"]
  }
}

resource "aws_iam_role_policy" "guardrails" {
  name   = "guardrails"
  role   = aws_iam_role.irsa["guardrails"].id
  policy = data.aws_iam_policy_document.guardrails.json
}

# ---------------------------------------------------------------------------
# otel-collector
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "otel_collector" {
  statement {
    sid    = "WriteTelemetry"
    effect = "Allow"
    actions = [
      "logs:PutLogEvents",
      "logs:CreateLogStream",
      "logs:DescribeLogStreams",
      "cloudwatch:PutMetricData",
      "xray:PutTraceSegments",
      "xray:PutTelemetryRecords",
      "xray:GetSamplingRules",
      "xray:GetSamplingTargets",
    ]
    # X-Ray and PutMetricData have no resource-level permissions; the
    # namespace condition below is the available constraint.
    resources = ["*"]

    condition {
      test     = "StringEqualsIfExists"
      variable = "cloudwatch:namespace"
      values   = ["agentgate"]
    }
  }

  statement {
    sid       = "RemoteWriteToManagedPrometheus"
    effect    = "Allow"
    actions   = ["aps:RemoteWrite"]
    resources = [aws_prometheus_workspace.this.arn]
  }
}

resource "aws_iam_role_policy" "otel_collector" {
  name   = "otel-collector"
  role   = aws_iam_role.irsa["otel-collector"].id
  policy = data.aws_iam_policy_document.otel_collector.json
}

# ---------------------------------------------------------------------------
# VPC flow logs
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "flow_logs_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["vpc-flow-logs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "flow_logs" {
  name               = "${local.base_name}-flow-logs"
  assume_role_policy = data.aws_iam_policy_document.flow_logs_assume.json
  tags               = local.common_tags
}

data "aws_iam_policy_document" "flow_logs" {
  statement {
    effect = "Allow"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
      "logs:DescribeLogGroups",
      "logs:DescribeLogStreams",
    ]
    resources = ["${aws_cloudwatch_log_group.vpc_flow.arn}:*"]
  }
}

resource "aws_iam_role_policy" "flow_logs" {
  name   = "flow-logs"
  role   = aws_iam_role.flow_logs.id
  policy = data.aws_iam_policy_document.flow_logs.json
}

# ---------------------------------------------------------------------------
# EKS access entries
# ---------------------------------------------------------------------------
# Cluster admin is granted to a named IAM role, not to individuals and not to
# the account root. Membership of that role is managed in the identity
# provider, so revoking someone's cluster access is one action in one place.

resource "aws_eks_access_entry" "platform_admin" {
  cluster_name  = aws_eks_cluster.this.name
  principal_arn = "arn:${data.aws_partition.current.partition}:iam::${var.account_id}:role/PlatformAIAdmin"
  type          = "STANDARD"
  tags          = local.common_tags
}

resource "aws_eks_access_policy_association" "platform_admin" {
  cluster_name  = aws_eks_cluster.this.name
  principal_arn = aws_eks_access_entry.platform_admin.principal_arn
  policy_arn    = "arn:${data.aws_partition.current.partition}:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }
}
