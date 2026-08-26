# Edge (SPEC §10, "Edge API management" — API Gateway / ALB).
#
# CHOICE: internal Application Load Balancer, not API Gateway.
#
# API Gateway is the closer analogue to Azure API Management and it is the
# wrong answer here for one decisive reason: both REST and HTTP APIs cap
# integration timeouts at 29 seconds and buffer the response. SPEC §2.5
# requires server-sent events with a 15-second heartbeat and generations that
# routinely run for minutes. Putting API Gateway in front of the gateway would
# truncate every long completion and destroy time-to-first-token — the metric
# with its own SLO in SPEC §5.
#
# So the traffic path is an internal ALB, which streams, and the functions API
# Gateway would have provided are placed where they can be done without
# breaking streaming:
#
#   subscription management  -> the control plane's registration flow, which
#                               already owns agent identity
#   token/request limiting   -> the gateway's own quota system (SPEC §3.4),
#                               which is token-aware rather than request-aware
#   canary weighting         -> ALB weighted target groups (SPEC §7), so a
#                               rollback is a weight change, not a deploy
#
# The named file `apim.tf` is kept for parity with the Azure directory: the
# same concern lives in the same filename in both clouds.

resource "aws_lb" "gateway" {
  name               = "alb-${local.base_name}"
  load_balancer_type = "application"
  internal           = true
  subnets            = aws_subnet.app[*].id
  security_groups    = [aws_security_group.alb.id]
  tags               = local.common_tags

  # Longer than any single generation. The gateway enforces its own deadline
  # (SPEC §3.3) and sends a heartbeat comment every 15s; a shorter idle
  # timeout here would cut streams the gateway considers healthy.
  idle_timeout = 3600

  drop_invalid_header_fields = true
  enable_deletion_protection = var.environment == "prod"
  # Cross-zone is on by default for ALB and is what lets a single-AZ node
  # failure be absorbed without a routing change.
  enable_http2 = true

  access_logs {
    bucket  = aws_s3_bucket.alb_logs.id
    prefix  = "alb"
    enabled = true
  }
}

resource "aws_lb_target_group" "gateway" {
  name        = "tg-${local.base_name}-gw"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.this.id
  tags        = local.common_tags

  health_check {
    path                = "/readyz"
    port                = "traffic-port"
    protocol            = "HTTP"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    interval            = 10
    timeout             = 5
    matcher             = "200"
  }

  # Long enough for in-flight streams to finish when a pod is removed, short
  # enough that a rollout is not held open by one idle connection.
  deregistration_delay = 60

  stickiness {
    # Off: sticky sessions would pin a caller to one pod and defeat the HPA.
    # Correlation is done with the trace id, not the connection.
    enabled = false
    type    = "lb_cookie"
  }
}

# The legacy first-generation gateway, kept as a second target group so the
# strangler migration in SPEC §7 is a weight change. It starts at 100% and is
# stepped down 100 -> 99 -> 95 -> 75 -> 50 -> 0 as the canary progresses.
resource "aws_lb_target_group" "legacy_gateway" {
  name        = "tg-${local.base_name}-legacy"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_vpc.this.id
  tags        = local.common_tags

  health_check {
    path                = "/health"
    port                = "traffic-port"
    protocol            = "HTTP"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    interval            = 10
    timeout             = 5
    matcher             = "200"
  }

  deregistration_delay = 60
}

resource "aws_acm_certificate" "internal" {
  domain_name = "gateway.${var.internal_domain}"
  subject_alternative_names = [
    "controlplane.${var.internal_domain}",
    "fleet.${var.internal_domain}",
  ]
  # Issued by the enterprise private CA (SPEC §8): these names never resolve
  # publicly, so a public CA could not validate them and should not be asked
  # to try.
  certificate_authority_arn = var.acm_private_ca_arn
  tags                      = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.gateway.arn
  port              = 443
  protocol          = "HTTPS"
  # TLS 1.3 only on the internal path (SPEC §8 row 1). Every caller is
  # first-party and modern, so there is no compatibility argument for keeping
  # 1.2 ciphers available.
  ssl_policy      = "ELBSecurityPolicy-TLS13-1-3-2021-06"
  certificate_arn = aws_acm_certificate.internal.arn
  tags            = local.common_tags

  default_action {
    type = "forward"

    forward {
      target_group {
        arn    = aws_lb_target_group.gateway.arn
        weight = 100
      }

      target_group {
        arn = aws_lb_target_group.legacy_gateway.arn
        # Migration weight. Change this number, not the deployment, to advance
        # or roll back a canary stage.
        weight = 0
      }

      stickiness {
        enabled  = false
        duration = 1
      }
    }
  }
}

# Health endpoints are unauthenticated (SPEC §2.1) and must never be routed to
# the legacy backend during a migration — a health check answering from the
# wrong implementation makes the canary's rollback signal meaningless.
resource "aws_lb_listener_rule" "health" {
  listener_arn = aws_lb_listener.https.arn
  priority     = 10
  tags         = local.common_tags

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.gateway.arn
  }

  condition {
    path_pattern {
      values = ["/healthz", "/readyz"]
    }
  }
}

# ---------------------------------------------------------------------------
# DNS
# ---------------------------------------------------------------------------

resource "aws_route53_record" "gateway" {
  zone_id = aws_route53_zone.internal.zone_id
  name    = "gateway.${var.internal_domain}"
  type    = "A"

  alias {
    name                   = aws_lb.gateway.dns_name
    zone_id                = aws_lb.gateway.zone_id
    evaluate_target_health = true
  }
}

resource "aws_route53_record" "controlplane" {
  zone_id = aws_route53_zone.internal.zone_id
  name    = "controlplane.${var.internal_domain}"
  type    = "A"

  alias {
    name                   = aws_lb.gateway.dns_name
    zone_id                = aws_lb.gateway.zone_id
    evaluate_target_health = true
  }
}

resource "aws_route53_record" "fleet" {
  zone_id = aws_route53_zone.internal.zone_id
  name    = "fleet.${var.internal_domain}"
  type    = "A"

  alias {
    name                   = aws_lb.gateway.dns_name
    zone_id                = aws_lb.gateway.zone_id
    evaluate_target_health = true
  }
}

# ---------------------------------------------------------------------------
# Access logs
# ---------------------------------------------------------------------------

resource "aws_s3_bucket" "alb_logs" {
  bucket        = "s3-${local.base_name}-alb-logs-${local.suffix}"
  force_destroy = false
  tags          = local.common_tags
}

resource "aws_s3_bucket_public_access_block" "alb_logs" {
  bucket                  = aws_s3_bucket.alb_logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "alb_logs" {
  bucket = aws_s3_bucket.alb_logs.id

  rule {
    apply_server_side_encryption_by_default {
      # SSE-S3, not SSE-KMS: ELB access-log delivery does not support a
      # customer-managed key, and a bucket policy that silently drops logs is
      # worse than a slightly weaker key.
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_versioning" "alb_logs" {
  bucket = aws_s3_bucket.alb_logs.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "alb_logs" {
  bucket = aws_s3_bucket.alb_logs.id

  rule {
    id     = "expire"
    status = "Enabled"

    filter {}

    transition {
      days          = 30
      storage_class = "STANDARD_IA"
    }

    expiration {
      days = var.log_retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }
}

data "aws_elb_service_account" "current" {}

data "aws_iam_policy_document" "alb_logs" {
  statement {
    sid       = "AllowELBLogDelivery"
    effect    = "Allow"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.alb_logs.arn}/alb/AWSLogs/${var.account_id}/*"]

    principals {
      type        = "AWS"
      identifiers = [data.aws_elb_service_account.current.arn]
    }
  }

  statement {
    sid       = "DenyUnencryptedTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.alb_logs.arn, "${aws_s3_bucket.alb_logs.arn}/*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "alb_logs" {
  bucket = aws_s3_bucket.alb_logs.id
  policy = data.aws_iam_policy_document.alb_logs.json
}
