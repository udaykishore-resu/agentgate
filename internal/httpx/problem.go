// Package httpx holds the HTTP primitives shared by every AgentGate service:
// the RFC 9457 problem+json error contract, SSE plumbing, and an instrumented
// HTTP client.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// ErrorCode is a stable, machine-readable error identifier. These strings are
// part of the frozen v1 contract: a code never changes meaning and never
// changes HTTP status. See SPEC.md section 2.4 and docs/04-gateway-contract.md.
type ErrorCode string

// The complete v1 error code set.
const (
	CodeInvalidRequest       ErrorCode = "invalid_request"
	CodeUnauthenticated      ErrorCode = "unauthenticated"
	CodeForbiddenPool        ErrorCode = "forbidden_pool"
	CodeAgentNotPromoted     ErrorCode = "agent_not_promoted"
	CodeGuardrailBlocked     ErrorCode = "guardrail_blocked"
	CodeUnknownModel         ErrorCode = "unknown_model"
	CodeClientTimeout        ErrorCode = "client_timeout"
	CodeIdempotencyConflict  ErrorCode = "idempotency_conflict"
	CodeContextTooLarge      ErrorCode = "context_too_large"
	CodeRateLimited          ErrorCode = "rate_limited"
	CodeQuotaExceeded        ErrorCode = "quota_exceeded"
	CodeClientClosedRequest  ErrorCode = "client_closed_request"
	CodeProviderError        ErrorCode = "provider_error"
	CodeNoHealthyBackend     ErrorCode = "no_healthy_backend"
	CodeProviderTimeout      ErrorCode = "provider_timeout"
	CodeInternal             ErrorCode = "internal_error"
	CodeNotFound             ErrorCode = "not_found"
	CodeConflict             ErrorCode = "conflict"
	CodePromotionGateBlocked ErrorCode = "promotion_gate_blocked"
)

// StatusClosedRequest is the non-standard status used, as nginx does, to
// record a caller that disconnected mid-stream. It is never written to the
// wire — the connection is already gone — but it is recorded on the span and
// in metrics so a disconnect is not counted as a gateway failure.
const StatusClosedRequest = 499

// statusFor is the single source of truth mapping code to HTTP status. The
// mapping is frozen; adding a code is additive, changing one is a v2 change.
var statusFor = map[ErrorCode]int{
	CodeInvalidRequest:       http.StatusBadRequest,
	CodeUnauthenticated:      http.StatusUnauthorized,
	CodeForbiddenPool:        http.StatusForbidden,
	CodeAgentNotPromoted:     http.StatusForbidden,
	CodeGuardrailBlocked:     http.StatusForbidden,
	CodeUnknownModel:         http.StatusNotFound,
	CodeNotFound:             http.StatusNotFound,
	CodeClientTimeout:        http.StatusRequestTimeout,
	CodeIdempotencyConflict:  http.StatusConflict,
	CodeConflict:             http.StatusConflict,
	CodePromotionGateBlocked: http.StatusConflict,
	CodeContextTooLarge:      http.StatusRequestEntityTooLarge,
	CodeRateLimited:          http.StatusTooManyRequests,
	CodeQuotaExceeded:        http.StatusTooManyRequests,
	CodeClientClosedRequest:  StatusClosedRequest,
	CodeProviderError:        http.StatusBadGateway,
	CodeNoHealthyBackend:     http.StatusServiceUnavailable,
	CodeProviderTimeout:      http.StatusGatewayTimeout,
	CodeInternal:             http.StatusInternalServerError,
}

// StatusFor returns the frozen HTTP status for a code.
func StatusFor(code ErrorCode) int {
	if s, ok := statusFor[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Problem is an RFC 9457 problem details document, extended with the fields a
// caller needs to open a support ticket that can actually be investigated.
type Problem struct {
	Type              string    `json:"type"`
	Title             string    `json:"title"`
	Status            int       `json:"status"`
	Detail            string    `json:"detail,omitempty"`
	Code              ErrorCode `json:"code"`
	RequestID         string    `json:"request_id,omitempty"`
	TraceID           string    `json:"trace_id,omitempty"`
	RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
	// Param names the offending field for validation errors.
	Param string `json:"param,omitempty"`
}

// Error implements error so a Problem can be returned through normal Go
// plumbing and unwrapped at the edge.
func (p *Problem) Error() string {
	return fmt.Sprintf("%s: %s", p.Code, p.Detail)
}

var titles = map[ErrorCode]string{
	CodeInvalidRequest:       "Invalid request",
	CodeUnauthenticated:      "Unauthenticated",
	CodeForbiddenPool:        "Pool not permitted for this agent",
	CodeAgentNotPromoted:     "Agent version not promoted for this environment",
	CodeGuardrailBlocked:     "Blocked by content safety policy",
	CodeUnknownModel:         "Unknown model",
	CodeNotFound:             "Not found",
	CodeClientTimeout:        "Client deadline exceeded",
	CodeIdempotencyConflict:  "Idempotency key conflict",
	CodeConflict:             "Conflict",
	CodePromotionGateBlocked: "Promotion gate blocked",
	CodeContextTooLarge:      "Context window exceeded",
	CodeRateLimited:          "Rate limited",
	CodeQuotaExceeded:        "Token quota exceeded",
	CodeClientClosedRequest:  "Client closed request",
	CodeProviderError:        "Upstream provider error",
	CodeNoHealthyBackend:     "No healthy backend",
	CodeProviderTimeout:      "Upstream provider timeout",
	CodeInternal:             "Internal error",
}

// NewProblem builds a Problem for a code with a caller-facing detail message.
// The detail must never contain prompt content or a secret: it is returned to
// the caller and written to logs.
func NewProblem(code ErrorCode, detail string) *Problem {
	return &Problem{
		Type:   "https://agentgate.internal/errors/" + string(code),
		Title:  titles[code],
		Status: StatusFor(code),
		Detail: detail,
		Code:   code,
	}
}

// Errorf builds a Problem with a formatted detail message.
func Errorf(code ErrorCode, format string, args ...any) *Problem {
	return NewProblem(code, fmt.Sprintf(format, args...))
}

// WithRetryAfter attaches a Retry-After hint.
func (p *Problem) WithRetryAfter(seconds int) *Problem {
	p.RetryAfterSeconds = seconds
	return p
}

// WithParam names the offending request field.
func (p *Problem) WithParam(name string) *Problem {
	p.Param = name
	return p
}

// AsProblem extracts a *Problem from an error chain, or wraps an unexpected
// error as an internal error. Unexpected error text is deliberately not
// forwarded to the caller.
func AsProblem(err error) *Problem {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	return NewProblem(CodeInternal, "the gateway encountered an unexpected error")
}

// WriteProblem renders a Problem as the response. It is safe to call after
// headers have been written only if nothing has been flushed; callers on the
// streaming path must use an SSE error frame instead.
func WriteProblem(w http.ResponseWriter, p *Problem, requestID, traceID string) {
	if p == nil {
		p = NewProblem(CodeInternal, "unspecified error")
	}
	p.RequestID = requestID
	p.TraceID = traceID
	if p.Status == StatusClosedRequest {
		// The caller is gone; nothing to write.
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/problem+json; charset=utf-8")
	if p.RetryAfterSeconds > 0 {
		h.Set("Retry-After", strconv.Itoa(p.RetryAfterSeconds))
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON renders any value as a JSON response.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
