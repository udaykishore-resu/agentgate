package gateway

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/agentgate/agentgate/internal/cache"
	"github.com/agentgate/agentgate/internal/config"
	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/provider"
	"github.com/agentgate/agentgate/internal/ratelimit"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/resilience"
	"github.com/agentgate/agentgate/internal/telemetry"
)

// Request headers defined by the frozen contract.
const (
	HeaderSessionID      = "x-agentgate-session-id"
	HeaderPriority       = "x-agentgate-request-priority"
	HeaderCache          = "x-agentgate-cache"
	HeaderPool           = "x-agentgate-pool"
	HeaderIdempotencyKey = "x-agentgate-idempotency-key"
)

// Response headers defined by the frozen contract.
const (
	HeaderRequestID       = "x-agentgate-request-id"
	HeaderTraceID         = "x-agentgate-trace-id"
	HeaderProvider        = "x-agentgate-provider"
	HeaderModel           = "x-agentgate-model"
	HeaderPoolUsed        = "x-agentgate-pool"
	HeaderAttempts        = "x-agentgate-attempts"
	HeaderCacheResult     = "x-agentgate-cache"
	HeaderTokensInput     = "x-agentgate-tokens-input"
	HeaderTokensOutput    = "x-agentgate-tokens-output"
	HeaderCostUSD         = "x-agentgate-cost-usd"
	HeaderRateLimitLimit  = "x-agentgate-ratelimit-limit-tokens"
	HeaderRateLimitRemain = "x-agentgate-ratelimit-remaining-tokens"
	HeaderRateLimitReset  = "x-agentgate-ratelimit-reset"
	HeaderGuardrail       = "x-agentgate-guardrail"
	HeaderDegraded        = "x-agentgate-degraded"
)

// Operation distinguishes the endpoints for metering and span naming.
type Operation string

// Gateway operations.
const (
	OpChat       Operation = "chat"
	OpEmbeddings Operation = "embeddings"
	OpTokenCount Operation = "token_count"
)

// RequestContext carries one request through the policy chain.
//
// It is a single mutable struct rather than a chain of wrapped values because
// every stage needs to contribute to the same telemetry record and the same
// response headers, and threading fifteen return values through a pipeline
// produces code nobody can safely change at 3am.
type RequestContext struct {
	Start     time.Time
	RequestID string
	TraceID   string
	Operation Operation

	W http.ResponseWriter
	R *http.Request

	Claims         *identity.Claims
	Body           []byte
	Chat           *provider.ChatRequest
	Embed          *provider.EmbeddingsRequest
	Model          config.Model
	Pool           *Pool
	Classification registry.DataClassification

	Priority       resilience.Priority
	CacheMode      string
	IdempotencyKey string
	Stream         bool

	// Routing and invocation results.
	Backend      *Backend
	Candidates   []*Backend
	Attempts     int
	FailoverFrom string
	ProviderTime time.Duration
	TTFT         time.Duration

	// Outputs.
	Response      *provider.ChatResponse
	EmbedResponse *provider.EmbeddingsResponse
	StreamHandle  provider.Stream
	CacheResult   cache.Result
	CacheReason   string
	CacheEntry    *cache.Entry

	// Accounting.
	EstimatedInput int
	ReservedTokens int64
	Reservation    *ratelimit.Reservation
	Usage          *provider.Usage
	UsageEstimated bool
	CostUSD        float64
	SavingsUSD     float64
	RateLimit      ratelimit.Result
	Degraded       bool
	// AttributionCorrected records that the gateway overrode an ownership
	// attribute the agent reported with the value from its verified token.
	AttributionCorrected bool

	// Guardrails.
	GuardrailInput  guardrails.Decision
	GuardrailOutput guardrails.Decision
	GuardrailHeader string

	Span     *telemetry.Span
	Problem  *httpx.Problem
	stageEnd map[string]time.Duration

	// Streaming bookkeeping. firstByte is the point after which the response
	// can no longer be retried, failed over, or replaced with an error status.
	firstByte    time.Time
	streamCancel context.CancelFunc
	streamSpan   *telemetry.Span
}

// streamStarted reports whether any content has already reached the caller.
func (rc *RequestContext) streamStarted() bool { return !rc.firstByte.IsZero() }

// markFirstByte records time-to-first-token exactly once.
func (rc *RequestContext) markFirstByte() {
	if rc.firstByte.IsZero() {
		rc.firstByte = time.Now()
		rc.TTFT = rc.firstByte.Sub(rc.Start)
	}
}

// releaseStream closes the provider stream and its attempt context.
func (rc *RequestContext) releaseStream() {
	if rc.StreamHandle != nil {
		_ = rc.StreamHandle.Close()
		rc.StreamHandle = nil
	}
	if rc.streamCancel != nil {
		rc.streamCancel()
		rc.streamCancel = nil
	}
	if rc.streamSpan != nil {
		rc.streamSpan.End()
		rc.streamSpan = nil
	}
}

// SessionID returns the caller-supplied session correlation id.
func (rc *RequestContext) SessionID() string {
	if v := rc.R.Header.Get(HeaderSessionID); v != "" {
		return v
	}
	if rc.Chat != nil && rc.Chat.Metadata != nil {
		return rc.Chat.Metadata.SessionID
	}
	return ""
}

// BackendName returns the selected backend name, or empty.
func (rc *RequestContext) BackendName() string {
	if rc.Backend == nil {
		return ""
	}
	return rc.Backend.Name
}

// PoolName returns the selected pool name, or empty.
func (rc *RequestContext) PoolName() string {
	if rc.Pool == nil {
		return ""
	}
	return rc.Pool.Name
}

// InputTokens returns the billed input tokens, falling back to the estimate.
func (rc *RequestContext) InputTokens() int {
	if rc.Usage != nil && rc.Usage.PromptTokens > 0 {
		return rc.Usage.PromptTokens
	}
	return rc.EstimatedInput
}

// OutputTokens returns the billed output tokens.
func (rc *RequestContext) OutputTokens() int {
	if rc.Usage != nil {
		return rc.Usage.CompletionTokens
	}
	return 0
}

// TenantLabels returns the label values used on every gateway metric, in the
// order the instruments declare them.
func (rc *RequestContext) TenantLabels() (env, tenant, team, agent string) {
	if rc.Claims == nil {
		return "unknown", "unknown", "unknown", "unknown"
	}
	return string(rc.Claims.Env), rc.Claims.Tenant, rc.Claims.Team, rc.Claims.AgentName
}

// CostCenter returns the chargeback key.
func (rc *RequestContext) CostCenter() string {
	if rc.Claims == nil || rc.Claims.CostCenter == "" {
		// A request that reaches metering without a cost centre is a defect,
		// not a rounding error. Recording it under a reserved value makes it
		// visible in the chargeback report instead of silently unattributed.
		return "UNATTRIBUTED"
	}
	return rc.Claims.CostCenter
}

// WriteHeaders sets the response headers the frozen contract requires. It is
// called once, before the status line, on both the success and error paths.
func (rc *RequestContext) WriteHeaders() {
	h := rc.W.Header()
	h.Set(HeaderRequestID, rc.RequestID)
	h.Set(HeaderTraceID, rc.TraceID)
	if rc.Pool != nil {
		h.Set(HeaderPoolUsed, rc.Pool.Name)
	}
	if rc.Backend != nil {
		h.Set(HeaderProvider, string(rc.Backend.Provider.Kind()))
		h.Set(HeaderModel, rc.Backend.Provider.Model())
	}
	if rc.Attempts > 0 {
		h.Set(HeaderAttempts, strconv.Itoa(rc.Attempts))
	}
	if rc.CacheResult != "" {
		h.Set(HeaderCacheResult, string(rc.CacheResult))
	}
	if rc.Usage != nil {
		h.Set(HeaderTokensInput, strconv.Itoa(rc.Usage.PromptTokens))
		h.Set(HeaderTokensOutput, strconv.Itoa(rc.Usage.CompletionTokens))
	}
	if rc.CostUSD > 0 {
		h.Set(HeaderCostUSD, strconv.FormatFloat(rc.CostUSD, 'f', 6, 64))
	}
	if rc.RateLimit.Limit > 0 {
		h.Set(HeaderRateLimitLimit, strconv.FormatInt(rc.RateLimit.Limit, 10))
		h.Set(HeaderRateLimitRemain, strconv.FormatInt(rc.RateLimit.Remaining, 10))
		h.Set(HeaderRateLimitReset, strconv.Itoa(int(rc.RateLimit.ResetAfter.Seconds())))
	}
	if rc.GuardrailHeader != "" {
		h.Set(HeaderGuardrail, rc.GuardrailHeader)
	}
	if rc.Degraded {
		// Telling the caller the platform is degraded is not an admission of
		// weakness; it is what lets a consuming team decide whether to retry
		// or to fall back, instead of guessing.
		h.Set(HeaderDegraded, "true")
	}
}

// RecordStage notes how long a policy stage took, for the span.
func (rc *RequestContext) RecordStage(name string, d time.Duration) {
	if rc.stageEnd == nil {
		rc.stageEnd = map[string]time.Duration{}
	}
	rc.stageEnd[name] = d
	if rc.Span != nil {
		rc.Span.SetAttributes(telemetry.Attr("agentgate.policy."+name+".duration_ms", float64(d.Microseconds())/1000))
	}
}

// GatewayOverhead returns the time spent inside the gateway, excluding time
// waiting on a provider. This is the latency SLI: it is the only part of the
// number the platform team can actually be held to.
func (rc *RequestContext) GatewayOverhead() time.Duration {
	total := time.Since(rc.Start)
	if rc.ProviderTime > total {
		return 0
	}
	return total - rc.ProviderTime
}

// StreamLabel returns the metric label value for streaming.
func (rc *RequestContext) StreamLabel() string {
	if rc.Stream {
		return "true"
	}
	return "false"
}
