// Package telemetry implements the AgentGate telemetry plane: the schema, a
// dependency-free tracing SDK that speaks OTLP/HTTP JSON, a metric registry
// with Prometheus exposition, and trace-correlated structured logging.
//
// The schema constants in this file are the contract every runtime instruments
// against. They are referenced by the collector processors in deploy/otel and
// by the SLO recording rules in deploy/prometheus. Changing a name here is a
// breaking change to dashboards, alerts and chargeback, and is treated as such.
package telemetry

// Resource attributes. Present on every signal from every runtime.
// See docs/06-telemetry-schema.md.
const (
	AttrServiceName        = "service.name"
	AttrServiceVersion     = "service.version"
	AttrServiceNamespace   = "service.namespace"
	AttrServiceInstanceID  = "service.instance.id"
	AttrDeploymentEnv      = "deployment.environment.name"
	AttrAgentID            = "agentgate.agent.id"
	AttrAgentIdentity      = "agentgate.agent.identity"
	AttrAgentVersion       = "agentgate.agent.version"
	AttrTenantID           = "agentgate.tenant.id"
	AttrTeamID             = "agentgate.team.id"
	AttrOwnerEmail         = "agentgate.owner.email"
	AttrCostCenter         = "agentgate.cost_center"
	AttrRuntime            = "agentgate.runtime"
	AttrFramework          = "agentgate.framework"
	AttrAttributionFixed   = "agentgate.attribution.corrected"
	AttrTelemetrySDKName   = "telemetry.sdk.name"
	AttrTelemetrySDKLang   = "telemetry.sdk.language"
	AttrTelemetrySDKVer    = "telemetry.sdk.version"
	AttrHostName           = "host.name"
	AttrK8sPodName         = "k8s.pod.name"
	AttrK8sNamespace       = "k8s.namespace.name"
	AttrCloudProvider      = "cloud.provider"
	AttrCloudRegion        = "cloud.region"
	AttrDataClassification = "agentgate.data_classification"
)

// GenAI semantic-convention attributes, used on backend-invocation spans.
const (
	AttrGenAISystem        = "gen_ai.system"
	AttrGenAIOperation     = "gen_ai.operation.name"
	AttrGenAIRequestModel  = "gen_ai.request.model"
	AttrGenAIResponseModel = "gen_ai.response.model"
	AttrGenAIRequestMaxTok = "gen_ai.request.max_tokens"
	AttrGenAIRequestTemp   = "gen_ai.request.temperature"
	AttrGenAIRequestTopP   = "gen_ai.request.top_p"
	AttrGenAIUsageInput    = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutput   = "gen_ai.usage.output_tokens"
	AttrGenAIFinishReasons = "gen_ai.response.finish_reasons"
	AttrGenAIResponseID    = "gen_ai.response.id"
	EventGenAIPrompt       = "gen_ai.content.prompt"
	EventGenAICompletion   = "gen_ai.content.completion"
)

// AgentGate span attributes, covering what the gen_ai conventions do not:
// ownership, routing decisions, policy outcomes and cost.
const (
	AttrLogicalModel   = "agentgate.logical_model"
	AttrPool           = "agentgate.pool"
	AttrBackend        = "agentgate.backend"
	AttrProvider       = "agentgate.provider"
	AttrAttempt        = "agentgate.attempt"
	AttrFailoverFrom   = "agentgate.failover.from"
	AttrCostUSD        = "agentgate.cost.usd"
	AttrCache          = "agentgate.cache"
	AttrTTFTMillis     = "agentgate.ttft_ms"
	AttrStream         = "agentgate.stream"
	AttrRequestID      = "agentgate.request.id"
	AttrSessionID      = "agentgate.session.id"
	AttrConversationID = "agentgate.conversation.id"
	AttrPriority       = "agentgate.request.priority"
	AttrErrorCode      = "agentgate.error.code"
	AttrGuardrail      = "agentgate.guardrail"
	AttrGuardrailCat   = "agentgate.guardrail.category"
	AttrPolicyStage    = "agentgate.policy.stage"
	AttrQuotaReserved  = "agentgate.quota.reserved_tokens"
	AttrQuotaSettled   = "agentgate.quota.settled_tokens"
	AttrPromotedState  = "agentgate.promotion.state"
	AttrIdempotencyKey = "agentgate.idempotency_key"
	AttrRetryReason    = "agentgate.retry.reason"
	AttrBreakerState   = "agentgate.breaker.state"
)

// Span names. Fixed strings so dashboards and tail-sampling policies can match
// on them without regular expressions.
const (
	SpanAgentInvoke     = "agent.invoke"
	SpanAgentStep       = "agent.step"
	SpanAgentTool       = "agent.tool"
	SpanGatewayRequest  = "gateway.request"
	SpanGatewayPolicy   = "gateway.policy." // + stage name
	SpanGatewayGuardra  = "gateway.guardrail"
	SpanGatewayCache    = "gateway.cache"
	SpanGenAIChat       = "gen_ai.chat"
	SpanGenAIEmbeddings = "gen_ai.embeddings"
	SpanCPRegister      = "controlplane.register"
	SpanCPPromote       = "controlplane.promote"
	SpanCPTokenExchange = "controlplane.token_exchange"
)

// ContentCapture controls whether prompt and completion content is emitted as
// span events. Financial-services default is CaptureOff in production.
type ContentCapture string

const (
	CaptureOff      ContentCapture = "off"
	CaptureRedacted ContentCapture = "redacted"
	CaptureFull     ContentCapture = "full"
)

// ParseContentCapture maps a configuration string to a ContentCapture,
// defaulting to the safe value on anything unrecognised.
func ParseContentCapture(s string) ContentCapture {
	switch ContentCapture(s) {
	case CaptureRedacted:
		return CaptureRedacted
	case CaptureFull:
		return CaptureFull
	default:
		return CaptureOff
	}
}
