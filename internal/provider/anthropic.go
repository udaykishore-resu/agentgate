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

// Anthropic adapts the Anthropic Messages dialect, used both by the native
// Anthropic API and by Amazon Bedrock's Anthropic models.
//
// This adapter is where "provider-agnostic interface" stops being a slogan.
// The caller sends OpenAI-shaped messages; this translates them into Messages
// format on the way out and back on the way in, including the differences that
// actually bite: a system prompt is a top-level field rather than a message,
// max_tokens is mandatory, tool calls are content blocks rather than a
// parallel array, and streaming is a typed event sequence rather than a
// sequence of deltas.
type Anthropic struct {
	opts    Options
	caps    map[Capability]bool
	signer  *SigV4Signer // set for Bedrock
	region  string
	version string
}

// AnthropicOptions extends Options with the fields Bedrock needs.
type AnthropicOptions struct {
	Options
	// Region, AccessKeyID, SecretAccessKey and SessionToken enable SigV4
	// signing for Bedrock. In a deployed environment these come from the
	// instance role rather than configuration.
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// AnthropicVersion is the dialect version header/field.
	AnthropicVersion string
}

// NewAnthropic builds an Anthropic or Bedrock adapter.
func NewAnthropic(o AnthropicOptions) (*Anthropic, error) {
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
		o.Capabilities = []Capability{CapChat, CapStreaming, CapTools}
	}
	if o.AnthropicVersion == "" {
		if o.Kind == KindBedrock {
			o.AnthropicVersion = "bedrock-2023-05-31"
		} else {
			o.AnthropicVersion = "2023-06-01"
		}
	}
	if o.BaseURL == "" {
		switch o.Kind {
		case KindBedrock:
			if o.Region == "" {
				return nil, fmt.Errorf("provider %s: region is required for bedrock", o.Name)
			}
			o.BaseURL = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", o.Region)
		default:
			o.BaseURL = "https://api.anthropic.com"
		}
	}
	caps := map[Capability]bool{}
	for _, c := range o.Capabilities {
		caps[c] = true
	}
	a := &Anthropic{opts: o.Options, caps: caps, region: o.Region, version: o.AnthropicVersion}
	a.opts.BaseURL = o.BaseURL
	if o.Kind == KindBedrock && o.AccessKeyID != "" {
		a.signer = &SigV4Signer{
			Region: o.Region, Service: "bedrock",
			AccessKeyID: o.AccessKeyID, SecretAccessKey: o.SecretAccessKey, SessionToken: o.SessionToken,
		}
	}
	return a, nil
}

// Name returns the backend identifier.
func (a *Anthropic) Name() string { return a.opts.Name }

// Kind returns the provider dialect.
func (a *Anthropic) Kind() Kind { return a.opts.Kind }

// Model returns the concrete backend model.
func (a *Anthropic) Model() string { return a.opts.Model }

// Capabilities lists what this backend supports.
func (a *Anthropic) Capabilities() []Capability { return a.opts.Capabilities }

// Supports reports whether a capability is available.
func (a *Anthropic) Supports(c Capability) bool { return a.caps[c] }

// --- request translation --------------------------------------------------

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicToolUse struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicToolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

func (a *Anthropic) toWire(req *ChatRequest, stream bool) map[string]any {
	body := map[string]any{
		// Bedrock carries the model in the URL and the dialect version in the
		// body; the native API is the other way round.
		"max_tokens": req.MaxOutputTokens(1024),
	}
	if a.opts.Kind == KindBedrock {
		body["anthropic_version"] = a.version
	} else {
		body["model"] = a.opts.Model
	}

	var system []anthropicTextBlock
	msgs := make([]anthropicMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			system = append(system, anthropicTextBlock{Type: "text", Text: m.Text()})
		case "tool":
			msgs = append(msgs, anthropicMessage{Role: "user", Content: []any{
				anthropicToolResult{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Text()},
			}})
		case "assistant":
			if len(m.ToolCalls) > 0 {
				blocks := make([]any, 0, len(m.ToolCalls)+1)
				if txt := m.Text(); txt != "" {
					blocks = append(blocks, anthropicTextBlock{Type: "text", Text: txt})
				}
				for _, tc := range m.ToolCalls {
					blocks = append(blocks, anthropicToolUse{
						Type: "tool_use", ID: tc.ID, Name: tc.Function.Name,
						Input: json.RawMessage(orEmptyObject(tc.Function.Arguments)),
					})
				}
				msgs = append(msgs, anthropicMessage{Role: "assistant", Content: blocks})
				continue
			}
			msgs = append(msgs, anthropicMessage{Role: "assistant", Content: m.Text()})
		default:
			msgs = append(msgs, anthropicMessage{Role: "user", Content: m.Text()})
		}
	}
	body["messages"] = msgs
	if len(system) > 0 {
		body["system"] = system
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		var stops []string
		if err := json.Unmarshal(req.Stop, &stops); err == nil {
			body["stop_sequences"] = stops
		} else {
			var one string
			if err := json.Unmarshal(req.Stop, &one); err == nil {
				body["stop_sequences"] = []string{one}
			}
		}
	}
	if len(req.Tools) > 0 && a.Supports(CapTools) {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			var fn struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			}
			if err := json.Unmarshal(t.Function, &fn); err != nil {
				continue
			}
			tools = append(tools, map[string]any{
				"name": fn.Name, "description": fn.Description,
				"input_schema": json.RawMessage(orEmptyObject(string(fn.Parameters))),
			})
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
	}
	if stream && a.opts.Kind != KindBedrock {
		body["stream"] = true
	}
	return body
}

func orEmptyObject(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// --- response translation -------------------------------------------------

type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Model   string `json:"model"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		CacheReadInput   int `json:"cache_read_input_tokens"`
		CacheCreateInput int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) fromWire(r anthropicResponse) *ChatResponse {
	msg := Message{Role: "assistant"}
	var text strings.Builder
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID: block.ID, Type: "function",
				Function: FunctionCall{Name: block.Name, Arguments: string(block.Input)},
			})
		}
	}
	msg.SetText(text.String())
	model := r.Model
	if model == "" {
		model = a.opts.Model
	}
	return &ChatResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{Index: 0, Message: &msg, FinishReason: mapStopReason(r.StopReason)}},
		Usage: &Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
			CachedTokens:     r.Usage.CacheReadInput,
		},
	}
}

// mapStopReason normalises finish reasons so a consumer never has to learn a
// second vocabulary. This mapping is part of the frozen contract.
func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "":
		return ""
	default:
		return s
	}
}

func (a *Anthropic) endpoint(stream bool) string {
	base := strings.TrimRight(a.opts.BaseURL, "/")
	if a.opts.Kind == KindBedrock {
		op := "invoke"
		if stream {
			op = "invoke-with-response-stream"
		}
		return fmt.Sprintf("%s/model/%s/%s", base, a.opts.Model, op)
	}
	return base + "/v1/messages"
}

func (a *Anthropic) newRequest(ctx context.Context, stream bool, body map[string]any) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(stream), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.opts.Kind == KindBedrock {
		req.Header.Set("Accept", "application/json")
		if a.signer != nil {
			if err := a.signer.Sign(req, raw, time.Now().UTC()); err != nil {
				return nil, err
			}
		}
	} else {
		req.Header.Set("anthropic-version", a.version)
		if a.opts.APIKey != "" {
			req.Header.Set("x-api-key", a.opts.APIKey)
		}
	}
	for k, v := range a.opts.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (a *Anthropic) fail(resp *http.Response, err error) *Error {
	e := &Error{Backend: a.opts.Name}
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
	e.Message = fmt.Sprint(err)
	switch {
	case err != nil && strings.Contains(err.Error(), "deadline exceeded"):
		e.Kind = ErrTimeout
	default:
		e.Kind = ErrTransport
	}
	return e
}

// Chat performs a unary completion.
func (a *Anthropic) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	httpReq, err := a.newRequest(ctx, false, a.toWire(req, false))
	if err != nil {
		return nil, a.fail(nil, err)
	}
	resp, err := httpx.Do(ctx, a.opts.Client, httpReq, a.opts.Timeout)
	if err != nil {
		return nil, a.fail(nil, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, a.fail(resp, nil)
	}
	var wire anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, a.fail(nil, fmt.Errorf("decode response: %w", err))
	}
	return a.fromWire(wire), nil
}

// ChatStream opens a streaming completion, translating the typed Anthropic
// event sequence into canonical chunks.
func (a *Anthropic) ChatStream(ctx context.Context, req *ChatRequest) (Stream, error) {
	if !a.Supports(CapStreaming) {
		return nil, &Error{Backend: a.opts.Name, Kind: ErrBadRequest, Message: "backend does not support streaming"}
	}
	httpReq, err := a.newRequest(ctx, true, a.toWire(req, true))
	if err != nil {
		return nil, a.fail(nil, err)
	}
	if a.opts.Kind != KindBedrock {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := httpx.Do(ctx, a.opts.Client, httpReq, a.opts.Timeout)
	if err != nil {
		return nil, a.fail(nil, err)
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, a.fail(resp, nil)
	}
	s := &anthropicStream{
		resp: resp, backend: a.opts.Name, model: a.opts.Model,
		events: make(chan streamItem, 64), done: make(chan struct{}),
	}
	if a.opts.Kind == KindBedrock {
		go s.pumpEventStream()
	} else {
		go s.pumpSSE()
	}
	return s, nil
}

// Embeddings is not offered by this dialect.
func (a *Anthropic) Embeddings(context.Context, *EmbeddingsRequest) (*EmbeddingsResponse, error) {
	return nil, &Error{Backend: a.opts.Name, Kind: ErrBadRequest, Message: "backend does not support embeddings"}
}

// Health probes the backend with a one-token completion, which is the only
// cheap call this dialect offers.
func (a *Anthropic) Health(ctx context.Context) error {
	probe := &ChatRequest{Messages: []Message{TextMessage("user", "ping")}}
	one := 1
	probe.MaxTokens = &one
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := a.Chat(ctx, probe)
	if pe, ok := AsError(err); ok && (pe.Kind == ErrBadRequest || pe.Kind == ErrContentFilter) {
		return nil // reachable and authenticated; the probe itself was rejected
	}
	return err
}

// anthropicStream converts Anthropic's typed event sequence into canonical
// chunks. The event types that matter are content_block_delta for text,
// content_block_start for the beginning of a tool call, and message_delta for
// the stop reason and output token count.
type anthropicStream struct {
	resp    *http.Response
	backend string
	model   string

	mu     sync.Mutex
	events chan streamItem
	usage  *Usage
	closed bool
	done   chan struct{}
}

func (s *anthropicStream) emit(text string, finish string, toolCall *ToolCall) {
	delta := &Message{Role: "assistant"}
	if text != "" {
		delta.SetText(text)
	}
	if toolCall != nil {
		delta.ToolCalls = []ToolCall{*toolCall}
	}
	c := &Chunk{
		Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model,
		Choices: []Choice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
	select {
	case s.events <- streamItem{chunk: c}:
	case <-s.done:
	}
}

func (s *anthropicStream) handleEvent(name string, payload []byte) {
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		ContentBlock struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
		Message struct {
			ID    string `json:"id"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	if ev.Type == "" {
		ev.Type = name
	}
	switch ev.Type {
	case "message_start":
		s.mu.Lock()
		s.usage = &Usage{PromptTokens: ev.Message.Usage.InputTokens}
		s.mu.Unlock()
	case "content_block_start":
		if ev.ContentBlock.Type == "tool_use" {
			idx := ev.Index
			s.emit("", "", &ToolCall{
				ID: ev.ContentBlock.ID, Type: "function", Index: &idx,
				Function: FunctionCall{Name: ev.ContentBlock.Name},
			})
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				s.emit(ev.Delta.Text, "", nil)
			}
		case "input_json_delta":
			if ev.Delta.PartialJSON != "" {
				idx := ev.Index
				s.emit("", "", &ToolCall{
					Type: "function", Index: &idx,
					Function: FunctionCall{Arguments: ev.Delta.PartialJSON},
				})
			}
		}
	case "message_delta":
		s.mu.Lock()
		if s.usage == nil {
			s.usage = &Usage{}
		}
		if ev.Usage.OutputTokens > 0 {
			s.usage.CompletionTokens = ev.Usage.OutputTokens
		}
		s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
		s.mu.Unlock()
		if ev.Delta.StopReason != "" {
			s.emit("", mapStopReason(ev.Delta.StopReason), nil)
		}
	case "error":
		select {
		case s.events <- streamItem{err: &Error{Backend: s.backend, Kind: ErrUnavailable, Message: string(payload)}}:
		case <-s.done:
		}
	}
}

func (s *anthropicStream) pumpSSE() {
	defer close(s.events)
	_ = httpx.ReadSSE(s.resp.Body, func(ev httpx.SSEEvent) error {
		if ev.Data == "" || ev.IsDone() {
			return nil
		}
		s.handleEvent(ev.Event, []byte(ev.Data))
		return nil
	})
}

func (s *anthropicStream) pumpEventStream() {
	defer close(s.events)
	dec := NewEventStreamDecoder(s.resp.Body)
	for {
		msg, err := dec.Next()
		if err != nil {
			if err != io.EOF {
				select {
				case s.events <- streamItem{err: &Error{Backend: s.backend, Kind: ErrTransport, Message: err.Error(), Err: err}}:
				case <-s.done:
				}
			}
			return
		}
		// Bedrock wraps each dialect event in a base64 "bytes" envelope.
		var envelope struct {
			Bytes []byte `json:"bytes"`
		}
		payload := msg.Payload
		if err := json.Unmarshal(payload, &envelope); err == nil && len(envelope.Bytes) > 0 {
			payload = envelope.Bytes
		}
		s.handleEvent(msg.Headers[":event-type"], payload)
	}
}

// Recv returns the next chunk.
func (s *anthropicStream) Recv() (*Chunk, bool, error) {
	item, ok := <-s.events
	if !ok {
		return nil, false, nil
	}
	if item.err != nil {
		return nil, false, item.err
	}
	return item.chunk, true, nil
}

// Usage returns final token accounting.
func (s *anthropicStream) Usage() *Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// Close releases the underlying response.
func (s *anthropicStream) Close() error {
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
