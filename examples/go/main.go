//go:build ignore

// Command agentgate-example shows a Go agent calling the AgentGate gateway.
//
// # Why the build tag
//
// The `//go:build ignore` tag above keeps this file out of the AgentGate module
// build. That is deliberate and it is the point of the file: this is not part
// of the platform, it is a page of code a consuming team copies into their own
// repository. Without the tag it would be a second `package main` in a
// directory the module builds, and `go build ./...` would fail.
//
// It also means this file is checked the way a consumer would check it - copy
// it out and build it - rather than being kept compiling by the platform's own
// build. To verify it yourself:
//
//	mkdir -p /tmp/agentgate-example && cp main.go /tmp/agentgate-example/
//	cd /tmp/agentgate-example && go mod init example && go vet . && go build .
//
// # Why there are no imports from github.com/agentgate/agentgate
//
// There are none on purpose. A consuming team must be able to paste this into
// a service that has never heard of AgentGate and have it compile against the
// standard library alone. Every type it needs is declared here. If this file
// imported the platform's internal packages it would stop being an example and
// become a client library, which is a thing the platform has deliberately not
// published - see api/README.md.
//
// # What it demonstrates
//
//  1. Obtaining an access token, by RFC 8693 token exchange (preferred, no
//     secret) or by client credentials (fallback).
//  2. A unary chat completion.
//  3. A streaming chat completion, including the agentgate.usage frame, the
//     heartbeat comment, the error frame, and the [DONE] sentinel.
//  4. Propagating W3C trace context so the agent's work and the gateway's
//     appear as one trace.
//  5. Reading and printing the x-agentgate-* response headers.
//  6. Handling the documented error codes correctly: honouring Retry-After on
//     429, and never retrying a 400.
//
// # Running it
//
//	export AGENTGATE_GATEWAY_URL=http://localhost:8080
//	export AGENTGATE_CONTROLPLANE_URL=http://localhost:8081
//	export AGENTGATE_CLIENT_ID=cli_...
//	export AGENTGATE_CLIENT_SECRET=ags_...
//	export AGENTGATE_ENV=dev
//	go run main.go
//
// Or, on a runtime with federated workload identity, drop the client id and
// secret and set these instead:
//
//	export AGENTGATE_SUBJECT_TOKEN_FILE=/var/run/secrets/tokens/agentgate
//	export AGENTGATE_AGENT_IDENTITY=agent://fsclient/payments-risk/dispute-triage
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Wire types
//
// These mirror the frozen v1 contract in api/openapi/gateway.v1.yaml. Only the
// fields this example uses are modelled; the contract guarantees that fields
// may be added, so an unknown field must be ignored rather than treated as an
// error. That is what encoding/json does by default, and this file relies on
// it.
// ---------------------------------------------------------------------------

// Message is one conversation turn. Content is json.RawMessage rather than a
// string because the contract permits an array of content parts, and a client
// that types it as a string breaks the first time someone sends an image.
type Message struct {
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

// TextMessage builds a plain-text turn.
func TextMessage(role, text string) Message {
	raw, _ := json.Marshal(text)
	return Message{Role: role, Content: raw}
}

// Text extracts plain text from a message, flattening content parts.
func (m Message) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(m.Content)
}

// StreamOptions asks the gateway for a final usage frame.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Metadata is caller context joined onto the trace. It is stripped before the
// request reaches a provider, so it costs no tokens and leaks no internal
// identifiers.
type Metadata struct {
	SessionID      string   `json:"session_id,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	Step           string   `json:"step,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

// ChatRequest is the chat-completion request body.
type ChatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	Metadata      *Metadata      `json:"metadata,omitempty"`
}

// Usage is token accounting for one call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
}

// Choice is one completion alternative. A unary response carries Message; a
// streaming chunk carries Delta.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`
}

// ChatResponse is the unary chat-completion response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Chunk is one streaming delta.
type Chunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// UsageFrame is the payload of the agentgate.usage SSE event.
type UsageFrame struct {
	RequestID  string  `json:"request_id"`
	TraceID    string  `json:"trace_id"`
	Provider   string  `json:"provider"`
	Backend    string  `json:"backend"`
	Model      string  `json:"model"`
	Attempts   int     `json:"attempts"`
	Usage      *Usage  `json:"usage"`
	CostUSD    float64 `json:"cost_usd"`
	Estimated  bool    `json:"estimated"`
	Stalls     int     `json:"stalls"`
	Cache      string  `json:"cache,omitempty"`
	SavingsUSD float64 `json:"savings_usd,omitempty"`
}

// Problem is the RFC 9457 error document. Every AgentGate failure is one of
// these, with Code the stable, machine-readable identifier.
//
// Branch on Code. Never on Title or Detail: the code is frozen, the prose is
// not.
type Problem struct {
	Type              string `json:"type"`
	Title             string `json:"title"`
	Status            int    `json:"status"`
	Detail            string `json:"detail,omitempty"`
	Code              string `json:"code"`
	RequestID         string `json:"request_id,omitempty"`
	TraceID           string `json:"trace_id,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	Param             string `json:"param,omitempty"`
}

// Error implements error so a Problem can travel through normal Go plumbing.
func (p *Problem) Error() string {
	if p.Param != "" {
		return fmt.Sprintf("%s (%d): %s [param=%s]", p.Code, p.Status, p.Detail, p.Param)
	}
	return fmt.Sprintf("%s (%d): %s", p.Code, p.Status, p.Detail)
}

// The frozen v1 error codes this example reasons about. The full set is in
// api/openapi/gateway.v1.yaml; an unrecognised code must be handled by falling
// back to the HTTP status class it arrived with.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeUnauthenticated  = "unauthenticated"
	CodeForbiddenPool    = "forbidden_pool"
	CodeAgentNotPromoted = "agent_not_promoted"
	CodeGuardrailBlocked = "guardrail_blocked"
	CodeUnknownModel     = "unknown_model"
	CodeClientTimeout    = "client_timeout"
	CodeContextTooLarge  = "context_too_large"
	CodeRateLimited      = "rate_limited"
	CodeQuotaExceeded    = "quota_exceeded"
	CodeProviderError    = "provider_error"
	CodeNoHealthyBackend = "no_healthy_backend"
	CodeProviderTimeout  = "provider_timeout"
	CodeInternalError    = "internal_error"
)

// retryable reports whether a Problem is worth another attempt.
//
// The distinction matters more than it looks. Retrying a 400 wastes the
// caller's deadline and produces exactly the same failure; retrying a 429
// without honouring Retry-After turns a rate limit into an outage, because the
// fleet-wide retry budget is capped at 10% of request volume and will shed the
// excess. The rule is: retry only what the platform said was transient, and
// only when it said so.
func retryable(p *Problem) bool {
	switch p.Code {
	case CodeRateLimited, CodeQuotaExceeded,
		CodeProviderError, CodeNoHealthyBackend, CodeProviderTimeout,
		CodeInternalError:
		return true
	default:
		// invalid_request, unknown_model, context_too_large, forbidden_pool,
		// agent_not_promoted, guardrail_blocked, idempotency_conflict: all
		// permanent for this request as written. client_timeout is a deadline
		// problem, not a load problem; retrying it with the same deadline fails
		// the same way.
		return false
	}
}

// ---------------------------------------------------------------------------
// Response headers
// ---------------------------------------------------------------------------

// GatewayHeaders is the x-agentgate-* metadata on every response, success or
// failure. Reading it is half the value of routing through the gateway: it is
// how an application learns what a call actually cost and where it went,
// without waiting for a monthly report or opening a dashboard.
type GatewayHeaders struct {
	RequestID        string
	TraceID          string
	Provider         string
	BackendModel     string
	Pool             string
	Attempts         int
	Cache            string
	TokensInput      int
	TokensOutput     int
	CostUSD          float64
	RateLimitLimit   int64
	RateLimitRemain  int64
	RateLimitResetIn time.Duration
	Guardrail        string
	Degraded         bool
	RetryAfter       time.Duration
}

// ReadGatewayHeaders extracts the contract headers from a response.
func ReadGatewayHeaders(h http.Header) GatewayHeaders {
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	atoi64 := func(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
	atof := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	secs := func(s string) time.Duration {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	return GatewayHeaders{
		RequestID:        h.Get("x-agentgate-request-id"),
		TraceID:          h.Get("x-agentgate-trace-id"),
		Provider:         h.Get("x-agentgate-provider"),
		BackendModel:     h.Get("x-agentgate-model"),
		Pool:             h.Get("x-agentgate-pool"),
		Attempts:         atoi(h.Get("x-agentgate-attempts")),
		Cache:            h.Get("x-agentgate-cache"),
		TokensInput:      atoi(h.Get("x-agentgate-tokens-input")),
		TokensOutput:     atoi(h.Get("x-agentgate-tokens-output")),
		CostUSD:          atof(h.Get("x-agentgate-cost-usd")),
		RateLimitLimit:   atoi64(h.Get("x-agentgate-ratelimit-limit-tokens")),
		RateLimitRemain:  atoi64(h.Get("x-agentgate-ratelimit-remaining-tokens")),
		RateLimitResetIn: secs(h.Get("x-agentgate-ratelimit-reset")),
		Guardrail:        h.Get("x-agentgate-guardrail"),
		Degraded:         h.Get("x-agentgate-degraded") == "true",
		RetryAfter:       secs(h.Get("Retry-After")),
	}
}

// String renders the headers as one operator-readable line.
func (g GatewayHeaders) String() string {
	s := fmt.Sprintf(
		"request=%s trace=%s provider=%s backend=%s pool=%s attempts=%d cache=%s in=%d out=%d cost=$%.6f",
		g.RequestID, g.TraceID, g.Provider, g.BackendModel, g.Pool,
		g.Attempts, g.Cache, g.TokensInput, g.TokensOutput, g.CostUSD)
	if g.RateLimitLimit > 0 {
		s += fmt.Sprintf(" quota=%d/%d reset=%s", g.RateLimitRemain, g.RateLimitLimit, g.RateLimitResetIn)
	}
	if g.Guardrail != "" {
		s += " guardrail=" + g.Guardrail
	}
	if g.Degraded {
		// Not an error. The platform is telling you a dependency was degraded
		// for this call so that you can decide whether to retry or fall back,
		// instead of guessing.
		s += " DEGRADED"
	}
	return s
}

// ---------------------------------------------------------------------------
// W3C trace context
// ---------------------------------------------------------------------------

// TraceContext is a W3C traceparent. Propagating it is what makes an agent run
// and the gateway work it caused appear as one trace instead of two unrelated
// ones, which is the difference between "the call was slow" and "the call was
// slow in the guardrail stage of the third of five steps".
type TraceContext struct {
	TraceID string // 32 lower-case hex characters
	SpanID  string // 16 lower-case hex characters
	Sampled bool
}

// NewTraceContext starts a new trace. A real agent would take this from its
// OpenTelemetry SDK rather than minting one; the shape is identical.
func NewTraceContext() TraceContext {
	return TraceContext{TraceID: randomHex(16), SpanID: randomHex(8), Sampled: true}
}

// Child derives a new span within the same trace, which is what each outbound
// call from an agent step should carry.
func (t TraceContext) Child() TraceContext {
	return TraceContext{TraceID: t.TraceID, SpanID: randomHex(8), Sampled: t.Sampled}
}

// Header renders the traceparent header value.
func (t TraceContext) Header() string {
	flags := "00"
	if t.Sampled {
		flags = "01"
	}
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + flags
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a condition an agent can sensibly
		// continue through, but a trace id is not worth crashing over either.
		// A zero id is obviously wrong in a trace viewer, which is the right
		// failure mode: visibly broken beats silently plausible.
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client calls the AgentGate gateway.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	// SessionID correlates every call this agent run makes. It lands on every
	// span, which is how a multi-step run is reconstructed as one story.
	SessionID string
}

// NewClient builds a client. The timeout is generous because streaming
// generations legitimately run for minutes; per-request deadlines come from
// the context, not from here.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		HTTP:      &http.Client{Timeout: 10 * time.Minute},
		SessionID: "sess_" + randomHex(8),
	}
}

// newRequest builds an authenticated request carrying trace context and the
// contract headers.
func (c *Client) newRequest(ctx context.Context, method, path string, body any, tc TraceContext) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("traceparent", tc.Header())
	if c.SessionID != "" {
		req.Header.Set("x-agentgate-session-id", c.SessionID)
	}
	return req, nil
}

// problemFrom decodes an error response. When the body is not a problem
// document - a proxy returned an HTML error page, say - it synthesises one from
// the status so that callers always get a Problem and never a nil error with a
// failed request.
func problemFrom(resp *http.Response, body []byte) *Problem {
	var p Problem
	if err := json.Unmarshal(body, &p); err == nil && p.Code != "" {
		if p.Status == 0 {
			p.Status = resp.StatusCode
		}
		return &p
	}
	code := CodeInternalError
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		code = CodeRateLimited
	case resp.StatusCode >= 500:
		code = CodeProviderError
	case resp.StatusCode >= 400:
		code = CodeInvalidRequest
	}
	return &Problem{
		Status: resp.StatusCode,
		Code:   code,
		Title:  resp.Status,
		Detail: strings.TrimSpace(string(body)),
	}
}

// retryDelay decides how long to wait before another attempt.
//
// Retry-After, when the platform sends it, is authoritative: the gateway knows
// when the bucket refills and the caller does not. Only when it is absent does
// the client fall back to exponential backoff with full jitter - full jitter,
// not equal jitter, because a fleet of agents backing off in lockstep
// reproduces the thundering herd the backoff was meant to prevent.
func retryDelay(attempt int, p *Problem, h GatewayHeaders) time.Duration {
	if h.RetryAfter > 0 {
		return h.RetryAfter
	}
	if p != nil && p.RetryAfterSeconds > 0 {
		return time.Duration(p.RetryAfterSeconds) * time.Second
	}
	base := time.Duration(1<<uint(attempt)) * 250 * time.Millisecond
	if base > 20*time.Second {
		base = 20 * time.Second
	}
	jitter := time.Duration(int64(randomUint64() % uint64(base+1)))
	return jitter
}

func randomUint64() uint64 {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return uint64(time.Now().UnixNano())
	}
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

// Chat performs a unary chat completion, retrying only what the contract says
// is transient.
func (c *Client) Chat(ctx context.Context, req ChatRequest, tc TraceContext, maxAttempts int) (*ChatResponse, GatewayHeaders, error) {
	req.Stream = false
	var lastProblem *Problem
	var lastHeaders GatewayHeaders

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryDelay(attempt-1, lastProblem, lastHeaders)
			fmt.Fprintf(os.Stderr, "  retrying after %s (attempt %d/%d, last=%s)\n",
				delay.Round(time.Millisecond), attempt+1, maxAttempts, lastProblem.Code)
			select {
			case <-ctx.Done():
				return nil, lastHeaders, ctx.Err()
			case <-time.After(delay):
			}
		}

		// Each attempt is a new span within the same trace, so a retry is
		// visible as a retry rather than as one long call.
		httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/chat/completions", req, tc.Child())
		if err != nil {
			return nil, lastHeaders, err
		}
		resp, err := c.HTTP.Do(httpReq)
		if err != nil {
			// A transport failure before the request was answered is safe to
			// retry: nothing was billed and nothing was generated.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, lastHeaders, err
			}
			lastProblem = &Problem{Code: CodeProviderError, Status: 0, Detail: err.Error()}
			lastHeaders = GatewayHeaders{}
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		headers := ReadGatewayHeaders(resp.Header)

		if resp.StatusCode == http.StatusOK {
			if readErr != nil {
				return nil, headers, readErr
			}
			var out ChatResponse
			if err := json.Unmarshal(body, &out); err != nil {
				return nil, headers, fmt.Errorf("decode response (request %s): %w", headers.RequestID, err)
			}
			return &out, headers, nil
		}

		p := problemFrom(resp, body)
		if !retryable(p) {
			// Permanent. Returning immediately is not giving up early; it is
			// declining to spend the caller's deadline on a request that has
			// already been definitively answered.
			return nil, headers, p
		}
		lastProblem, lastHeaders = p, headers
	}
	return nil, lastHeaders, fmt.Errorf("gave up after %d attempts: %w", maxAttempts, lastProblem)
}

// StreamHandlers receives the frames of a streaming response.
type StreamHandlers struct {
	// OnDelta is called for each content fragment.
	OnDelta func(text string)
	// OnUsage is called for the agentgate.usage frame, before [DONE].
	OnUsage func(UsageFrame)
	// OnError is called for a mid-stream error frame. After it, the stream is
	// over and no [DONE] will arrive.
	OnError func(*Problem)
}

// ChatStream performs a streaming chat completion.
//
// The one rule that matters: a stream that ends without `data: [DONE]` FAILED.
// Once the first content byte has been written the HTTP status is long gone and
// cannot be changed, so the contract carries the failure in band as an `error`
// frame. A client that treats a truncated stream as a short answer will
// silently show a user half a response, which in a regulated context is worse
// than showing them an error.
//
// This function returns an error when the stream did not complete, whether or
// not an error frame explained why.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, tc TraceContext, h StreamHandlers) (GatewayHeaders, error) {
	req.Stream = true
	if req.StreamOptions == nil {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/chat/completions", req, tc.Child())
	if err != nil {
		return GatewayHeaders{}, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return GatewayHeaders{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	headers := ReadGatewayHeaders(resp.Header)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return headers, problemFrom(resp, body)
	}

	var (
		done       bool
		streamErr  *Problem
		event      string
		dataBuffer []string
	)

	sc := bufio.NewScanner(resp.Body)
	// Chunks are small but a single frame can be large when a provider batches
	// tool-call arguments. 8 MiB is generous and bounded.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	dispatch := func() {
		if len(dataBuffer) == 0 && event == "" {
			return
		}
		data := strings.Join(dataBuffer, "\n")
		event, dataBuffer = "", nil

		if strings.TrimSpace(data) == "[DONE]" {
			done = true
			return
		}
		switch event {
		case "":
			var chunk Chunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return // an unparseable frame is ignorable, not fatal
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil {
				if text := chunk.Choices[0].Delta.Text(); text != "" && h.OnDelta != nil {
					h.OnDelta(text)
				}
			}
		case "agentgate.usage":
			var u UsageFrame
			if err := json.Unmarshal([]byte(data), &u); err == nil && h.OnUsage != nil {
				h.OnUsage(u)
			}
		case "error":
			var p Problem
			if err := json.Unmarshal([]byte(data), &p); err == nil {
				streamErr = &p
				if h.OnError != nil {
					h.OnError(&p)
				}
			}
		default:
			// An unknown named event is ignorable by design: the contract adds
			// frames, and a strict client must not choke on one it has not
			// heard of.
		}
	}

	for sc.Scan() {
		if done {
			break
		}
		line := sc.Text()
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, ":"):
			// A comment. This is the heartbeat the gateway sends every 15s so
			// that idle proxies do not drop a long generation. Ignore it - but
			// note that receiving it is evidence the connection is alive and
			// the model is simply still thinking.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			chunk := strings.TrimPrefix(line, "data:")
			dataBuffer = append(dataBuffer, strings.TrimPrefix(chunk, " "))
		}
	}
	if !done {
		dispatch()
	}

	if err := sc.Err(); err != nil && !done {
		return headers, fmt.Errorf("stream read failed after %d frames (request %s): %w",
			len(dataBuffer), headers.RequestID, err)
	}
	if streamErr != nil {
		return headers, streamErr
	}
	if !done {
		// No [DONE], no error frame. The connection dropped. Treat it as a
		// failure; do NOT present the partial content as an answer.
		return headers, &Problem{
			Status: 0, Code: CodeProviderError,
			Title:  "Truncated stream",
			Detail: "the stream ended without the [DONE] sentinel and without an error frame",
		}
	}
	return headers, nil
}

// ---------------------------------------------------------------------------
// Token acquisition
// ---------------------------------------------------------------------------

// TokenResponse is the control plane's /oauth2/token result.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	IssuedAt    int64  `json:"issued_at"`
	AgentID     string `json:"agent_id"`
	Env         string `json:"env"`
	Version     string `json:"agent_version"`
}

// oauthError is the token endpoint's error body. Note that /oauth2/token
// returns OAuth2-shaped errors, not problem+json: it speaks the dialect an
// OAuth2 client library expects.
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// FetchTokenExchange obtains an access token by RFC 8693 token exchange.
//
// This is the preferred path. The runtime has already proved who the workload
// is - a Kubernetes projected service account token, an Azure managed identity
// token, a SPIFFE JWT-SVID - and the control plane converts that proof into an
// AgentGate identity. No shared secret ever exists, and the resulting token
// carries attestation=workload-identity, which is what the production promotion
// gate requires.
func FetchTokenExchange(ctx context.Context, controlPlaneURL, subjectToken, agentIdentity, env, agentVersion string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token", subjectToken)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:jwt")
	form.Set("agent_identity", agentIdentity)
	form.Set("env", env)
	if agentVersion != "" {
		form.Set("agent_version", agentVersion)
	}
	return postToken(ctx, controlPlaneURL, form)
}

// FetchTokenClientCredentials obtains an access token with a client secret.
//
// The fallback, for runtimes that cannot do federated workload identity. The
// resulting token carries attestation=client-secret, which the production
// promotion gate refuses - so an agent using this path can reach staging but
// not production.
func FetchTokenClientCredentials(ctx context.Context, controlPlaneURL, clientID, clientSecret, env, agentVersion string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("env", env)
	if agentVersion != "" {
		form.Set("agent_version", agentVersion)
	}
	return postToken(ctx, controlPlaneURL, form)
}

func postToken(ctx context.Context, controlPlaneURL string, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(controlPlaneURL, "/")+"/oauth2/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var oe oauthError
		if json.Unmarshal(body, &oe) == nil && oe.Error != "" {
			return nil, fmt.Errorf("token request refused: %s: %s", oe.Error, oe.ErrorDescription)
		}
		return nil, fmt.Errorf("token request refused: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out TokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("token endpoint returned an empty access token")
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Walkthrough
// ---------------------------------------------------------------------------

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	gateway := envOr("AGENTGATE_GATEWAY_URL", "http://localhost:8080")
	controlPlane := envOr("AGENTGATE_CONTROLPLANE_URL", "http://localhost:8081")
	env := envOr("AGENTGATE_ENV", "dev")
	model := envOr("AGENTGATE_MODEL", "general-chat")

	// --- 1. Obtain a token -------------------------------------------------
	//
	// Real agents refresh the token before expiry rather than fetching one per
	// run. Tokens are deliberately short-lived (15 minutes by default) so that
	// a quarantine takes effect quickly, which means a long-running agent MUST
	// refresh. Refresh at roughly half the lifetime, not at expiry: a token
	// that expires mid-request produces a 401 the caller cannot distinguish
	// from a real authentication failure.
	tok, err := obtainToken(ctx, controlPlane, env)
	if err != nil {
		fatal("could not obtain an access token: %v", err)
	}
	fmt.Printf("token: agent_id=%s env=%s version=%s expires_in=%ds scope=%q\n\n",
		tok.AgentID, tok.Env, tok.Version, tok.ExpiresIn, tok.Scope)

	client := NewClient(gateway, tok.AccessToken)

	// The trace this agent run belongs to. In a real agent this comes from the
	// OpenTelemetry SDK; every gateway call below carries a child span of it,
	// so the whole run is one trace in the observability backend.
	trace := NewTraceContext()
	fmt.Printf("trace: %s (session %s)\n\n", trace.TraceID, client.SessionID)

	// --- 2. Unary call -----------------------------------------------------

	fmt.Println("=== unary ===")
	req := ChatRequest{
		Model: model,
		Messages: []Message{
			TextMessage("system", "You are a dispute triage assistant. Answer in two sentences."),
			TextMessage("user", "Cardholder disputes a 42.10 GBP contactless transaction from 3 March."),
		},
		MaxTokens:   intPtr(256),
		Temperature: floatPtr(0.2),
		Metadata: &Metadata{
			SessionID: client.SessionID,
			Step:      "classify",
			Tags:      []string{"dispute", "tier2"},
		},
	}

	resp, headers, err := client.Chat(ctx, req, trace, 3)
	fmt.Printf("headers: %s\n", headers)
	if err != nil {
		reportFailure(err)
	} else if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
		fmt.Printf("answer:  %s\n", resp.Choices[0].Message.Text())
		if resp.Usage != nil {
			fmt.Printf("usage:   in=%d out=%d total=%d\n",
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
		}
	}
	fmt.Println()

	// --- 3. Streaming call -------------------------------------------------

	fmt.Println("=== streaming ===")
	streamReq := ChatRequest{
		Model: model,
		Messages: []Message{
			TextMessage("user", "Summarise the common chargeback reason codes for card-present fraud."),
		},
		MaxTokens:     intPtr(512),
		Temperature:   floatPtr(0.2),
		StreamOptions: &StreamOptions{IncludeUsage: true},
		Metadata:      &Metadata{SessionID: client.SessionID, Step: "summarise"},
	}

	// A streaming call is NOT retried by this client. Before the first content
	// byte the gateway retries and fails over on the caller's behalf; after it,
	// re-issuing the request would bill for and regenerate a completion the
	// caller has already partly consumed. If a stream fails mid-flight, the
	// application - not the transport - decides what to do with the fragment.
	var received strings.Builder
	streamHeaders, err := client.ChatStream(ctx, streamReq, trace, StreamHandlers{
		OnDelta: func(text string) {
			received.WriteString(text)
			fmt.Print(text)
		},
		OnUsage: func(u UsageFrame) {
			fmt.Printf("\n\nusage frame: backend=%s attempts=%d cost=$%.6f estimated=%v stalls=%d",
				u.Backend, u.Attempts, u.CostUSD, u.Estimated, u.Stalls)
			if u.Usage != nil {
				fmt.Printf(" in=%d out=%d", u.Usage.PromptTokens, u.Usage.CompletionTokens)
			}
			if u.Cache != "" {
				fmt.Printf(" cache=%s saved=$%.6f", u.Cache, u.SavingsUSD)
			}
			fmt.Println()
		},
		OnError: func(p *Problem) {
			fmt.Fprintf(os.Stderr, "\n\nstream error frame: %s\n", p.Error())
		},
	})
	fmt.Printf("headers: %s\n", streamHeaders)
	if err != nil {
		// The fragment in `received` is genuine model output, but it is an
		// INCOMPLETE answer. Log it, discard it, or hand it to the application
		// with an explicit "truncated" marker - never present it as the answer.
		fmt.Fprintf(os.Stderr, "stream failed after %d characters; the partial answer is NOT the answer\n",
			received.Len())
		reportFailure(err)
	}
	fmt.Println()

	// --- 4. Pre-flight token count ----------------------------------------
	//
	// Cheaper than discovering context_too_large by paying for a refusal.
	fmt.Println("=== token-count ===")
	if err := preflight(ctx, client, trace, model, req.Messages); err != nil {
		reportFailure(err)
	}
}

// obtainToken picks the token grant from what the environment provides,
// preferring workload identity.
func obtainToken(ctx context.Context, controlPlane, env string) (*TokenResponse, error) {
	agentVersion := os.Getenv("AGENTGATE_AGENT_VERSION")

	if path := os.Getenv("AGENTGATE_SUBJECT_TOKEN_FILE"); path != "" {
		// The projected service account token is refreshed in place by the
		// kubelet, so it must be read at each use, never cached.
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read subject token: %w", err)
		}
		identity := os.Getenv("AGENTGATE_AGENT_IDENTITY")
		if identity == "" {
			return nil, errors.New("AGENTGATE_AGENT_IDENTITY is required with AGENTGATE_SUBJECT_TOKEN_FILE")
		}
		fmt.Println("obtaining a token by RFC 8693 token exchange (federated workload identity)")
		return FetchTokenExchange(ctx, controlPlane, strings.TrimSpace(string(raw)), identity, env, agentVersion)
	}

	clientID, secret := os.Getenv("AGENTGATE_CLIENT_ID"), os.Getenv("AGENTGATE_CLIENT_SECRET")
	if clientID == "" || secret == "" {
		return nil, errors.New("set AGENTGATE_SUBJECT_TOKEN_FILE and AGENTGATE_AGENT_IDENTITY, " +
			"or AGENTGATE_CLIENT_ID and AGENTGATE_CLIENT_SECRET")
	}
	fmt.Println("obtaining a token by client credentials (prefer workload identity where available)")
	return FetchTokenClientCredentials(ctx, controlPlane, clientID, secret, env, agentVersion)
}

// preflight asks the gateway to estimate the request before it is spent.
func preflight(ctx context.Context, c *Client, tc TraceContext, model string, messages []Message) error {
	body := ChatRequest{Model: model, Messages: messages, MaxTokens: intPtr(2048)}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/token-count", body, tc.Child())
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return problemFrom(resp, raw)
	}
	var out struct {
		Model                string `json:"model"`
		EstimatedInputTokens int    `json:"estimated_input_tokens"`
		ContextWindow        int    `json:"context_window"`
		MaxOutputTokens      int    `json:"max_output_tokens"`
		MonthlyTokensUsed    int64  `json:"monthly_tokens_used"`
		MonthlyTokenBudget   int64  `json:"monthly_token_budget"`
		TokensPerMinuteLimit int64  `json:"tokens_per_minute_limit"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	fmt.Printf("model=%s estimated_input=%d window=%d max_output=%d\n",
		out.Model, out.EstimatedInputTokens, out.ContextWindow, out.MaxOutputTokens)
	fmt.Printf("monthly budget: %d/%d used, %d tokens/minute limit\n",
		out.MonthlyTokensUsed, out.MonthlyTokenBudget, out.TokensPerMinuteLimit)
	return nil
}

// reportFailure prints the guidance the contract attaches to each error code.
// A real agent would branch here rather than print; the point is that every one
// of these has a different correct response, and treating them uniformly as
// "the call failed" throws that away.
func reportFailure(err error) {
	var p *Problem
	if !errors.As(err, &p) {
		fmt.Fprintf(os.Stderr, "transport failure: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "failed: %s\n", p.Error())
	if p.RequestID != "" {
		fmt.Fprintf(os.Stderr, "  quote request_id=%s trace_id=%s in any ticket\n", p.RequestID, p.TraceID)
	}
	switch p.Code {
	case CodeInvalidRequest:
		fmt.Fprintln(os.Stderr, "  permanent. Fix the request; retrying it unchanged fails identically.")
		if p.Param != "" {
			fmt.Fprintf(os.Stderr, "  the offending field is %q\n", p.Param)
		}
	case CodeUnauthenticated:
		fmt.Fprintln(os.Stderr, "  obtain a fresh token and retry once. A second failure with a fresh token is configuration, not transience.")
	case CodeForbiddenPool:
		fmt.Fprintln(os.Stderr, "  this token is not entitled to that pool, or lacks the scope. Ask the platform team for a grant.")
	case CodeAgentNotPromoted:
		fmt.Fprintln(os.Stderr, "  this token's environment does not match this gateway. Promote the version, or call the right gateway.")
	case CodeGuardrailBlocked:
		fmt.Fprintln(os.Stderr, "  content safety refused. Do not retry the same content. If this is a false positive, raise it with the guardrail owner.")
	case CodeUnknownModel:
		fmt.Fprintln(os.Stderr, "  call GET /v1/models for the list this token may actually use.")
	case CodeContextTooLarge:
		fmt.Fprintln(os.Stderr, "  trim the context or lower max_tokens. POST /v1/token-count answers this before a request is spent.")
	case CodeClientTimeout:
		fmt.Fprintln(os.Stderr, "  the deadline expired. Retry with a longer deadline or a smaller request, not with the same one.")
	case CodeRateLimited, CodeQuotaExceeded:
		fmt.Fprintf(os.Stderr, "  honour Retry-After (%ds). If the monthly budget is exhausted, backoff will not help; the quota must change.\n",
			p.RetryAfterSeconds)
	case CodeNoHealthyBackend:
		fmt.Fprintln(os.Stderr, "  every backend in the pool is unavailable. This is a platform failure and has already paged someone.")
	case CodeProviderError, CodeProviderTimeout:
		fmt.Fprintln(os.Stderr, "  upstream failure after the gateway's own retries and failover. Back off, then retry.")
	case CodeInternalError:
		fmt.Fprintln(os.Stderr, "  retry once with backoff. If it persists, open a ticket quoting the request id.")
	default:
		// An unrecognised code is not an error in the client. The contract
		// permits new codes; fall back to the status class.
		if p.Status >= 500 || p.Status == http.StatusTooManyRequests {
			fmt.Fprintln(os.Stderr, "  unrecognised code, transient status class: back off and retry.")
		} else {
			fmt.Fprintln(os.Stderr, "  unrecognised code, permanent status class: do not retry.")
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
