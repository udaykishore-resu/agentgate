package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
)

// Options configures a provider adapter.
type Options struct {
	Name         string
	Kind         Kind
	BaseURL      string
	Model        string
	APIKey       string
	APIVersion   string // Azure OpenAI api-version
	Deployment   string // Azure OpenAI deployment name; defaults to Model
	Client       *http.Client
	Timeout      time.Duration
	Capabilities []Capability
	// ExtraHeaders carry things an enterprise egress path requires, such as a
	// proxy authorisation token or a data-residency hint.
	ExtraHeaders map[string]string
}

// OpenAICompatible adapts any backend speaking the OpenAI chat-completions
// wire format: OpenAI itself, Azure OpenAI, and self-hosted vLLM, TGI or
// llama.cpp servers.
type OpenAICompatible struct {
	opts Options
	caps map[Capability]bool
}

// NewOpenAICompatible builds an adapter.
func NewOpenAICompatible(o Options) (*OpenAICompatible, error) {
	if o.BaseURL == "" {
		return nil, fmt.Errorf("provider %s: base_url is required", o.Name)
	}
	if o.Model == "" {
		return nil, fmt.Errorf("provider %s: model is required", o.Name)
	}
	if o.Client == nil {
		c, err := httpx.NewClient(httpx.ClientConfig{Timeout: o.Timeout})
		if err != nil {
			return nil, err
		}
		o.Client = c
	}
	if len(o.Capabilities) == 0 {
		o.Capabilities = []Capability{CapChat, CapStreaming, CapTools, CapJSONMode, CapEmbeddings}
	}
	caps := map[Capability]bool{}
	for _, c := range o.Capabilities {
		caps[c] = true
	}
	return &OpenAICompatible{opts: o, caps: caps}, nil
}

// Name returns the backend identifier used in metrics and headers.
func (p *OpenAICompatible) Name() string { return p.opts.Name }

// Kind returns the provider dialect.
func (p *OpenAICompatible) Kind() Kind { return p.opts.Kind }

// Model returns the concrete backend model.
func (p *OpenAICompatible) Model() string { return p.opts.Model }

// Capabilities lists what this backend supports.
func (p *OpenAICompatible) Capabilities() []Capability { return p.opts.Capabilities }

// Supports reports whether a capability is available.
func (p *OpenAICompatible) Supports(c Capability) bool { return p.caps[c] }

func (p *OpenAICompatible) url(path string) string {
	base := strings.TrimRight(p.opts.BaseURL, "/")
	if p.opts.Kind == KindAzureOpenAI {
		dep := p.opts.Deployment
		if dep == "" {
			dep = p.opts.Model
		}
		v := p.opts.APIVersion
		if v == "" {
			v = "2024-10-21"
		}
		return fmt.Sprintf("%s/openai/deployments/%s%s?api-version=%s", base, dep, path, v)
	}
	return base + path
}

func (p *OpenAICompatible) newRequest(ctx context.Context, path string, body any) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url(path), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	switch p.opts.Kind {
	case KindAzureOpenAI:
		// Azure OpenAI accepts either an api-key header or a bearer token from
		// managed identity. The key form is used only where managed identity
		// is not available on the runtime.
		if strings.HasPrefix(p.opts.APIKey, "Bearer ") {
			req.Header.Set("Authorization", p.opts.APIKey)
		} else if p.opts.APIKey != "" {
			req.Header.Set("api-key", p.opts.APIKey)
		}
	default:
		if p.opts.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.opts.APIKey)
		}
	}
	for k, v := range p.opts.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// toWire converts the canonical request into the provider's own body,
// substituting the concrete backend model for the caller's logical model.
func (p *OpenAICompatible) toWire(req *ChatRequest, stream bool) map[string]any {
	out := map[string]any{
		"model":    p.opts.Model,
		"messages": req.Messages,
	}
	if p.opts.Kind == KindAzureOpenAI {
		// The deployment is in the URL; sending a model field as well is
		// tolerated but redundant and confuses provider-side logging.
		delete(out, "model")
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.N != nil {
		out["n"] = *req.N
	}
	if len(req.Stop) > 0 {
		out["stop"] = req.Stop
	}
	if req.PresencePenalty != nil {
		out["presence_penalty"] = *req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		out["frequency_penalty"] = *req.FrequencyPenalty
	}
	if req.Seed != nil {
		out["seed"] = *req.Seed
	}
	if len(req.Tools) > 0 && p.Supports(CapTools) {
		out["tools"] = req.Tools
		if len(req.ToolChoice) > 0 {
			out["tool_choice"] = req.ToolChoice
		}
	}
	if len(req.ResponseFormat) > 0 && p.Supports(CapJSONMode) {
		out["response_format"] = req.ResponseFormat
	}
	if req.User != "" {
		out["user"] = req.User
	}
	if stream {
		out["stream"] = true
		// Always ask for usage on streams. Without it the gateway has to
		// estimate output tokens, and an estimated number is not something a
		// team can be charged against.
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	for k, v := range req.Extra {
		out[k] = v
	}
	return out
}

func (p *OpenAICompatible) fail(resp *http.Response, err error) *Error {
	e := &Error{Backend: p.opts.Name}
	if resp != nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		e.StatusCode = resp.StatusCode
		e.Message = strings.TrimSpace(string(body))
		e.Kind = Classify(resp.StatusCode, e.Message)
		e.RetryAfter = ParseRetryAfter(resp.Header)
		if len(e.Message) > 512 {
			e.Message = e.Message[:512]
		}
		return e
	}
	e.Err = err
	switch {
	case err == nil:
		e.Kind = ErrUnknown
	case strings.Contains(err.Error(), "context deadline exceeded"):
		e.Kind = ErrTimeout
	case strings.Contains(err.Error(), "context canceled"):
		e.Kind = ErrTimeout
	default:
		e.Kind = ErrTransport
	}
	e.Message = fmt.Sprint(err)
	return e
}

// Chat performs a unary chat completion.
func (p *OpenAICompatible) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	httpReq, err := p.newRequest(ctx, "/chat/completions", p.toWire(req, false))
	if err != nil {
		return nil, p.fail(nil, err)
	}
	resp, err := httpx.Do(ctx, p.opts.Client, httpReq, p.opts.Timeout)
	if err != nil {
		return nil, p.fail(nil, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, p.fail(resp, nil)
	}
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, p.fail(nil, fmt.Errorf("decode response: %w", err))
	}
	if out.Model == "" {
		out.Model = p.opts.Model
	}
	return &out, nil
}

// ChatStream opens a streaming chat completion.
func (p *OpenAICompatible) ChatStream(ctx context.Context, req *ChatRequest) (Stream, error) {
	if !p.Supports(CapStreaming) {
		return nil, &Error{Backend: p.opts.Name, Kind: ErrBadRequest, Message: "backend does not support streaming"}
	}
	httpReq, err := p.newRequest(ctx, "/chat/completions", p.toWire(req, true))
	if err != nil {
		return nil, p.fail(nil, err)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := httpx.Do(ctx, p.opts.Client, httpReq, p.opts.Timeout)
	if err != nil {
		return nil, p.fail(nil, err)
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, p.fail(resp, nil)
	}
	return newSSEStream(resp, p.opts.Name, p.opts.Model), nil
}

// Embeddings computes embeddings.
func (p *OpenAICompatible) Embeddings(ctx context.Context, req *EmbeddingsRequest) (*EmbeddingsResponse, error) {
	if !p.Supports(CapEmbeddings) {
		return nil, &Error{Backend: p.opts.Name, Kind: ErrBadRequest, Message: "backend does not support embeddings"}
	}
	body := map[string]any{"input": req.Input}
	if p.opts.Kind != KindAzureOpenAI {
		body["model"] = p.opts.Model
	}
	if req.Dimensions != nil {
		body["dimensions"] = *req.Dimensions
	}
	if req.EncodingFormat != "" {
		body["encoding_format"] = req.EncodingFormat
	}
	httpReq, err := p.newRequest(ctx, "/embeddings", body)
	if err != nil {
		return nil, p.fail(nil, err)
	}
	resp, err := httpx.Do(ctx, p.opts.Client, httpReq, p.opts.Timeout)
	if err != nil {
		return nil, p.fail(nil, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, p.fail(resp, nil)
	}
	var out EmbeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, p.fail(nil, fmt.Errorf("decode response: %w", err))
	}
	return &out, nil
}

// Health probes the backend cheaply. A models listing is used where available
// because it exercises auth and routing without spending tokens.
func (p *OpenAICompatible) Health(ctx context.Context) error {
	base := strings.TrimRight(p.opts.BaseURL, "/")
	probe := base + "/models"
	if p.opts.Kind == KindAzureOpenAI {
		v := p.opts.APIVersion
		if v == "" {
			v = "2024-10-21"
		}
		probe = base + "/openai/models?api-version=" + v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe, nil)
	if err != nil {
		return err
	}
	switch p.opts.Kind {
	case KindAzureOpenAI:
		if p.opts.APIKey != "" {
			req.Header.Set("api-key", p.opts.APIKey)
		}
	default:
		if p.opts.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.opts.APIKey)
		}
	}
	resp, err := httpx.Do(ctx, p.opts.Client, req, 5*time.Second)
	if err != nil {
		return p.fail(nil, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 500 {
		return p.fail(resp, nil)
	}
	return nil
}

// sseStream decodes an OpenAI-style SSE response into canonical chunks.
type sseStream struct {
	resp    *http.Response
	backend string
	model   string

	mu     sync.Mutex
	events chan streamItem
	usage  *Usage
	closed bool
	done   chan struct{}
}

type streamItem struct {
	chunk *Chunk
	err   error
}

func newSSEStream(resp *http.Response, backend, model string) *sseStream {
	s := &sseStream{
		resp: resp, backend: backend, model: model,
		events: make(chan streamItem, 64), done: make(chan struct{}),
	}
	go s.pump()
	return s
}

func (s *sseStream) pump() {
	defer close(s.events)
	err := httpx.ReadSSE(s.resp.Body, func(ev httpx.SSEEvent) error {
		if ev.IsDone() {
			return nil
		}
		if ev.Data == "" {
			return nil
		}
		var c Chunk
		if err := json.Unmarshal([]byte(ev.Data), &c); err != nil {
			// A frame the gateway cannot parse is dropped rather than
			// failing the stream: providers occasionally emit keep-alive or
			// vendor-specific frames mid-stream.
			return nil
		}
		if c.Model == "" {
			c.Model = s.model
		}
		if c.Usage != nil {
			s.mu.Lock()
			s.usage = c.Usage
			s.mu.Unlock()
		}
		select {
		case s.events <- streamItem{chunk: &c}:
		case <-s.done:
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF {
		select {
		case s.events <- streamItem{err: &Error{Backend: s.backend, Kind: ErrTransport, Message: err.Error(), Err: err}}:
		case <-s.done:
		}
	}
}

// Recv returns the next chunk.
func (s *sseStream) Recv() (*Chunk, bool, error) {
	item, ok := <-s.events
	if !ok {
		return nil, false, nil
	}
	if item.err != nil {
		return nil, false, item.err
	}
	return item.chunk, true, nil
}

// Usage returns final token accounting if the provider reported it.
func (s *sseStream) Usage() *Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// Close releases the underlying response.
func (s *sseStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
	return s.resp.Body.Close()
}
