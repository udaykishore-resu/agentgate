package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind identifies a provider dialect.
type Kind string

// Supported provider dialects. Azure OpenAI, OpenAI and self-hosted vLLM all
// speak the same wire format and differ only in URL shape and auth header,
// which is why they share one adapter.
const (
	KindOpenAI      Kind = "openai"
	KindAzureOpenAI Kind = "azure-openai"
	KindVLLM        Kind = "onprem-vllm"
	KindBedrock     Kind = "bedrock"
	KindAnthropic   Kind = "anthropic"
)

// Capability describes what a backend can do, used to refuse a request the
// backend cannot serve before it is sent rather than after it fails.
type Capability string

// Backend capabilities.
const (
	CapChat       Capability = "chat"
	CapStreaming  Capability = "streaming"
	CapTools      Capability = "tools"
	CapJSONMode   Capability = "json_mode"
	CapEmbeddings Capability = "embeddings"
	CapVision     Capability = "vision"
)

// Stream is an open streaming response. Recv returns io.EOF-equivalent by
// returning ok=false when the stream is complete.
type Stream interface {
	// Recv returns the next chunk. ok is false once the stream has ended.
	Recv() (chunk *Chunk, ok bool, err error)
	// Usage returns final token accounting once the stream has ended. Some
	// providers only report usage on the last frame; others never do, in which
	// case the gateway estimates and marks the record as estimated.
	Usage() *Usage
	Close() error
}

// Provider is one model backend.
type Provider interface {
	// Name is the backend identifier used in metrics, spans and headers, in
	// the form "provider/model".
	Name() string
	Kind() Kind
	Model() string
	Capabilities() []Capability
	Supports(c Capability) bool

	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req *ChatRequest) (Stream, error)
	Embeddings(ctx context.Context, req *EmbeddingsRequest) (*EmbeddingsResponse, error)

	// Health is a cheap liveness probe used by the readiness endpoint and by
	// the breaker's half-open probe.
	Health(ctx context.Context) error
}

// Error is a normalised provider failure. The classification, not the
// provider's own status code, is what the retry and failover logic acts on:
// every provider expresses "you are being throttled" differently and the
// gateway must not learn each one twice.
type Error struct {
	Backend    string
	StatusCode int
	Kind       ErrorKind
	RetryAfter time.Duration
	Message    string
	Err        error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Backend, e.Message, e.Kind)
	}
	return fmt.Sprintf("%s: %s", e.Backend, e.Kind)
}

// Unwrap exposes the underlying transport error.
func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether another attempt could succeed.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case ErrThrottled, ErrUnavailable, ErrTimeout, ErrTransport, ErrOverloaded:
		return true
	default:
		return false
	}
}

// ErrorKind is the normalised failure classification.
type ErrorKind string

// Failure classifications.
const (
	ErrBadRequest    ErrorKind = "bad_request"
	ErrAuth          ErrorKind = "auth"
	ErrNotFound      ErrorKind = "not_found"
	ErrContentFilter ErrorKind = "content_filter"
	ErrContextLength ErrorKind = "context_length"
	ErrThrottled     ErrorKind = "throttled"
	ErrOverloaded    ErrorKind = "overloaded"
	ErrUnavailable   ErrorKind = "unavailable"
	ErrTimeout       ErrorKind = "timeout"
	ErrTransport     ErrorKind = "transport"
	ErrUnknown       ErrorKind = "unknown"
)

// AsError extracts a *Error from an error chain.
func AsError(err error) (*Error, bool) {
	var pe *Error
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// Classify maps an HTTP status and body to a normalised failure kind.
func Classify(status int, body string) ErrorKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusTooManyRequests:
		return ErrThrottled
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return ErrTimeout
	case status == http.StatusServiceUnavailable:
		return ErrUnavailable
	case status == http.StatusBadGateway:
		return ErrUnavailable
	case status >= 500:
		return ErrUnavailable
	case status == http.StatusRequestEntityTooLarge:
		return ErrContextLength
	case status == http.StatusBadRequest:
		switch {
		case strings.Contains(lower, "context length"), strings.Contains(lower, "maximum context"),
			strings.Contains(lower, "too many tokens"), strings.Contains(lower, "input is too long"):
			return ErrContextLength
		case strings.Contains(lower, "content filter"), strings.Contains(lower, "responsible ai"),
			strings.Contains(lower, "content_policy"), strings.Contains(lower, "blocked by"):
			return ErrContentFilter
		}
		return ErrBadRequest
	default:
		return ErrUnknown
	}
}

// ParseRetryAfter reads a Retry-After header, accepting the seconds form. The
// HTTP-date form is rare from model providers and is treated as absent rather
// than mis-parsed into an enormous delay.
func ParseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		v = h.Get("retry-after-ms")
		if v == "" {
			return 0
		}
		if ms, err := time.ParseDuration(v + "ms"); err == nil {
			return ms
		}
		return 0
	}
	if d, err := time.ParseDuration(v + "s"); err == nil && d > 0 && d < 5*time.Minute {
		return d
	}
	return 0
}
