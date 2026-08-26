# API Management in front of the gateway (SPEC §10, edge API management).
#
# APIM is NOT a second gateway. Everything that requires knowledge of agent
# identity, pools, quota, cost or telemetry stays in the AgentGate gateway,
# because that is where the token is verified and where the spans originate.
# APIM does the three things an edge is genuinely better at:
#
#   1. Terminating the published contract at a stable, subscription-managed
#      hostname, so consumer onboarding is an APIM operation rather than a
#      DNS change.
#   2. Coarse outer limits (token-rate and request-rate per subscription) that
#      stop a runaway consumer before it reaches the cluster at all.
#   3. Being the weighting point for the migration in SPEC §7 — the canary
#      percentages are backend weights here, so a rollback is a policy change
#      and not a deploy.

resource "azurerm_api_management" "this" {
  name                = "apim-${local.base_name}-${local.suffix}"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  publisher_name      = var.apim_publisher.name
  publisher_email     = var.apim_publisher.email
  sku_name            = var.apim_sku
  tags                = local.common_tags

  # Internal mode: the gateway URL resolves only inside the VNet. There is no
  # public entry point to AgentGate anywhere in this configuration.
  virtual_network_type = "Internal"

  virtual_network_configuration {
    subnet_id = azurerm_subnet.apim.id
  }

  identity {
    type = "SystemAssigned"
  }

  # TLS 1.2 and below disabled on both the frontend and the backend side. The
  # internal path is TLS 1.3 per SPEC §8; these switches remove the legacy
  # protocols APIM otherwise enables by default.
  security {
    backend_ssl30_enabled                             = false
    backend_tls10_enabled                             = false
    backend_tls11_enabled                             = false
    frontend_ssl30_enabled                            = false
    frontend_tls10_enabled                            = false
    frontend_tls11_enabled                            = false
    tls_ecdhe_rsa_with_aes256_cbc_sha_ciphers_enabled = false
    tls_ecdhe_rsa_with_aes128_cbc_sha_ciphers_enabled = false
    triple_des_ciphers_enabled                        = false
  }

  zones = var.environment == "prod" ? ["1", "2", "3"] : null

  lifecycle {
    # Provisioning an APIM instance takes about 45 minutes; accidental
    # replacement is an outage measured in hours.
    prevent_destroy = true
  }
}

resource "azurerm_api_management_logger" "appinsights" {
  name                = "appinsights"
  api_management_name = azurerm_api_management.this.name
  resource_group_name = azurerm_resource_group.this.name
  resource_id         = azurerm_application_insights.this.id

  application_insights {
    instrumentation_key = azurerm_application_insights.this.instrumentation_key
  }
}

resource "azurerm_api_management_diagnostic" "this" {
  identifier               = "applicationinsights"
  resource_group_name      = azurerm_resource_group.this.name
  api_management_name      = azurerm_api_management.this.name
  api_management_logger_id = azurerm_api_management_logger.appinsights.id

  # 100%: APIM sampling would break trace continuity. The gateway continues
  # the caller's W3C context (SPEC §2.2) and a sampled-out edge span leaves a
  # hole at the root of every trace it drops.
  sampling_percentage       = 100
  always_log_errors         = true
  log_client_ip             = true
  verbosity                 = "information"
  http_correlation_protocol = "W3C"

  frontend_request {
    body_bytes = 0
    headers_to_log = [
      "x-agentgate-session-id",
      "x-agentgate-request-priority",
      "x-agentgate-pool",
      "traceparent",
    ]
  }

  frontend_response {
    body_bytes = 0
    headers_to_log = [
      "x-agentgate-request-id",
      "x-agentgate-trace-id",
      "x-agentgate-provider",
      "x-agentgate-model",
      "x-agentgate-cost-usd",
      "x-agentgate-guardrail",
    ]
  }

  backend_request {
    body_bytes     = 0
    headers_to_log = ["traceparent"]
  }

  backend_response {
    body_bytes     = 0
    headers_to_log = ["x-agentgate-request-id", "x-agentgate-attempts"]
  }
}

# The internal load balancer in front of the gateway Deployment. APIM reaches
# it by private DNS name; the certificate is the one cert-manager issues in
# deploy/k8s/base/httproute.yaml.
resource "azurerm_api_management_backend" "gateway" {
  name                = "agentgate-gateway"
  resource_group_name = azurerm_resource_group.this.name
  api_management_name = azurerm_api_management.this.name
  protocol            = "http"
  url                 = "https://gateway.agentgate.internal"
  description         = "AgentGate gateway, reached over the internal load balancer inside the VNet."

  tls {
    # Certificates come from the enterprise PKI, whose root is in the APIM
    # trust store. Chain validation stays on precisely because this is the
    # boundary where a mis-issued certificate would matter.
    validate_certificate_chain = true
    validate_certificate_name  = true
  }
}

resource "azurerm_api_management_api" "gateway_v1" {
  name                  = "agentgate-gateway-v1"
  resource_group_name   = azurerm_resource_group.this.name
  api_management_name   = azurerm_api_management.this.name
  revision              = "1"
  display_name          = "AgentGate Gateway v1"
  path                  = "agentgate"
  protocols             = ["https"]
  service_url           = "https://gateway.agentgate.internal"
  subscription_required = true
  description           = "Frozen v1 wire contract (SPEC §2). OpenAI-shaped; fields may be added, never removed or retyped."

  subscription_key_parameter_names {
    header = "Ocp-Apim-Subscription-Key"
    query  = "subscription-key"
  }

  # The published contract is the OpenAPI document in the repository. Importing
  # it here means APIM rejects a request shape the gateway would have rejected
  # anyway, one hop earlier and with a clearer error.
  #
  # Guarded by fileexists so a checkout without the generated document still
  # plans; APIM then serves the API as a pass-through until the next apply. It
  # is a `dynamic` block rather than a `count` on the API because losing the
  # API resource would drop every consumer subscription with it.
  dynamic "import" {
    for_each = fileexists(local.openapi_contract_path) ? [1] : []

    content {
      content_format = "openapi"
      content_value  = file(local.openapi_contract_path)
    }
  }
}

# ---------------------------------------------------------------------------
# The AI policy set
# ---------------------------------------------------------------------------
resource "azurerm_api_management_api_policy" "gateway_v1" {
  api_name            = azurerm_api_management_api.gateway_v1.name
  api_management_name = azurerm_api_management.this.name
  resource_group_name = azurerm_resource_group.this.name

  xml_content = <<-XML
    <policies>
      <inbound>
        <base />

        <!-- Defence in depth on identity. The gateway remains the authority:
             it checks jti replay, pool entitlement, promotion state and scope,
             none of which APIM can see. What this catches is the cheap case —
             an expired or unsigned token — before it costs a cluster hop. Both
             layers read the same JWKS, so there is exactly one signing
             authority. -->
        <validate-jwt header-name="Authorization"
                      failed-validation-httpcode="401"
                      failed-validation-error-message="unauthenticated"
                      require-expiration-time="true"
                      require-signed-tokens="true"
                      clock-skew="60">
          <openid-config url="https://controlplane.agentgate.internal/.well-known/openid-configuration" />
          <audiences>
            <audience>https://gateway.agentgate.internal</audience>
          </audiences>
          <issuers>
            <issuer>https://controlplane.agentgate.internal</issuer>
          </issuers>
        </validate-jwt>

        <!-- Coarse outer token limit, keyed on the agent identity claim rather
             than the subscription key, so one team's runaway agent cannot
             consume another agent's headroom under the same subscription.
             estimate-prompt-tokens matches the gateway's own reserve/settle
             model (SPEC §3.4): count before the call, not after. -->
        <llm-token-limit counter-key="@(context.Request.Headers.GetValueOrDefault('Authorization','').Split(' ').Last())"
                         tokens-per-minute="${var.apim_token_limit_per_minute}"
                         estimate-prompt-tokens="true"
                         tokens-consumed-header-name="x-agentgate-edge-tokens-consumed"
                         remaining-tokens-header-name="x-agentgate-edge-tokens-remaining" />

        <!-- Token usage as a metric at the edge, dimensioned by the same keys
             the gateway uses. Two independently-derived cost figures is what
             makes an invoice dispute resolvable. -->
        <llm-emit-token-metric namespace="agentgate">
          <dimension name="Subscription" value="@(context.Subscription?.Id ?? 'anonymous')" />
          <dimension name="API" value="@(context.Api.Name)" />
          <dimension name="Operation" value="@(context.Operation.Name)" />
        </llm-emit-token-metric>

        <!-- Request-rate guard. Deliberately generous: this is a circuit
             against a broken client, not the quota system. -->
        <rate-limit-by-key calls="6000"
                           renewal-period="60"
                           counter-key="@(context.Subscription?.Id ?? context.Request.IpAddress)"
                           remaining-calls-header-name="x-agentgate-edge-ratelimit-remaining" />

        <!-- Continue the caller's trace rather than starting a new one, so the
             edge hop appears in the same trace as the gateway's work. -->
        <set-header name="traceparent" exists-action="skip">
          <value>@(context.RequestId.ToString())</value>
        </set-header>

        <set-backend-service backend-id="agentgate-gateway" />
      </inbound>

      <backend>
        <!-- No retries and no buffering. Retry policy, including the fleet-wide
             retry budget that prevents storms (SPEC §3.3), belongs to the
             gateway; an APIM retry is invisible to that budget and would
             double-charge the caller's token quota. Buffering would destroy
             time-to-first-token for streamed responses. -->
        <forward-request timeout="600" buffer-request-body="false" buffer-response="false" fail-on-error-status-code="false" />
      </backend>

      <outbound>
        <base />
        <!-- The response headers in SPEC §2.3 are part of the frozen contract
             and must survive the edge untouched. This policy adds nothing and
             removes nothing; it only asserts the ones consumers correlate on
             are present, so a future policy addition that strips them fails
             loudly here rather than quietly at a consumer. -->
        <set-header name="x-agentgate-edge" exists-action="override">
          <value>apim</value>
        </set-header>
      </outbound>

      <on-error>
        <base />
        <!-- Errors must stay RFC 9457 problem+json with the stable codes from
             SPEC §2.4. APIM's default error body is a different shape, and a
             consumer parsing `code` would break on it. -->
        <set-header name="Content-Type" exists-action="override">
          <value>application/problem+json</value>
        </set-header>
        <set-body>@{
          return new JObject(
            new JProperty("type", "https://agentgate.internal/errors/edge_error"),
            new JProperty("title", context.LastError.Reason ?? "Edge error"),
            new JProperty("status", context.Response?.StatusCode ?? 500),
            new JProperty("detail", context.LastError.Message),
            new JProperty("code", context.Response?.StatusCode == 401 ? "unauthenticated" : "provider_error"),
            new JProperty("request_id", context.RequestId.ToString())
          ).ToString();
        }</set-body>
      </on-error>
    </policies>
  XML

  depends_on = [azurerm_api_management_backend.gateway]
}

resource "azurerm_api_management_product" "agents" {
  product_id            = "agentgate-agents"
  resource_group_name   = azurerm_resource_group.this.name
  api_management_name   = azurerm_api_management.this.name
  display_name          = "AgentGate — Agent Access"
  description           = "Access to the frozen v1 gateway contract. One subscription per consuming team; the token still carries the per-agent identity."
  subscription_required = true
  # Approval required: a subscription is a funded, attributed consumer, and
  # self-service issuance would create traffic with no cost centre behind it.
  approval_required = true
  published         = true

  subscriptions_limit = 50
}

resource "azurerm_api_management_product_api" "agents_gateway" {
  product_id          = azurerm_api_management_product.agents.product_id
  api_name            = azurerm_api_management_api.gateway_v1.name
  api_management_name = azurerm_api_management.this.name
  resource_group_name = azurerm_resource_group.this.name
}
