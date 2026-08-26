// Package provider adapts each model backend to one provider-agnostic
// interface.
//
// The canonical shape is the OpenAI chat-completions schema. That is a
// deliberate choice rather than an aesthetic one: every agent framework the
// client's teams use already speaks it, so adopting it removes an SDK
// migration from the critical path and lets the gateway be dropped in front of
// existing code. Providers that do not speak it — Bedrock's Anthropic Messages
// dialect, for instance — are translated in their adapter, not at the edge.
package provider

import (
	"encoding/json"
	"strings"
)

// Message is one conversation turn. Content is either a plain string or an
// array of content parts; both are preserved verbatim so that a provider which
// supports multi-modal input receives what the caller sent.
type Message struct {
	Role       string          `json:"role,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Text extracts the plain-text content of a message, flattening content parts.
// Used for token estimation, cache keys and guardrail scanning.
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
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	return string(m.Content)
}

// IsPlainText reports whether the content is a single JSON string rather than
// an array of content parts. Callers that must distinguish a text-only message
// from a multi-modal one — the cache key, above all — ask here rather than
// inferring it from the flattened text.
func (m Message) IsPlainText() bool {
	if len(m.Content) == 0 {
		return true
	}
	var s string
	return json.Unmarshal(m.Content, &s) == nil
}

// TextMessage builds a message with plain string content.
func TextMessage(role, text string) Message {
	raw, _ := json.Marshal(text)
	return Message{Role: role, Content: raw}
}

// SetText replaces a message's content with plain text, used when the guardrail
// engine redacts rather than blocks.
func (m *Message) SetText(text string) {
	raw, _ := json.Marshal(text)
	m.Content = raw
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Index    *int         `json:"index,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function half of a tool call.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Tool is a tool definition offered to the model.
type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

// StreamOptions mirrors the OpenAI streaming options object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Metadata is caller-supplied context joined onto the trace. It is not sent to
// the provider.
type Metadata struct {
	SessionID      string   `json:"session_id,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	Step           string   `json:"step,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

// ChatRequest is the canonical chat-completion request.
type ChatRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	N                *int            `json:"n,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	Tools            []Tool          `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	User             string          `json:"user,omitempty"`
	Metadata         *Metadata       `json:"metadata,omitempty"`
	// Extra preserves provider-specific parameters the gateway does not model,
	// so a new provider feature does not require a gateway release to use.
	Extra map[string]json.RawMessage `json:"-"`
}

// PromptText concatenates all message text, for estimation and cache keying.
func (r *ChatRequest) PromptText() string {
	var b strings.Builder
	for _, m := range r.Messages {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Text())
		b.WriteByte('\n')
	}
	return b.String()
}

// LastUserText returns the final user turn, which is what semantic caching and
// input guardrails are most interested in.
func (r *ChatRequest) LastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].Text()
		}
	}
	return ""
}

// MaxOutputTokens returns the requested output cap, or a default when the
// caller did not set one. Quota reservation needs a number either way.
func (r *ChatRequest) MaxOutputTokens(def int) int {
	if r.MaxTokens != nil && *r.MaxTokens > 0 {
		return *r.MaxTokens
	}
	return def
}

// Usage is token accounting for one call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is the provider's own prompt-cache hit count, distinct from
	// the gateway's response cache and billed differently.
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// Choice is one completion alternative.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"`
}

// ChatResponse is the canonical chat-completion response.
type ChatResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
}

// FirstText returns the first choice's message text.
func (r *ChatResponse) FirstText() string {
	if len(r.Choices) == 0 || r.Choices[0].Message == nil {
		return ""
	}
	return r.Choices[0].Message.Text()
}

// FinishReasons collects the finish reasons for the span attribute.
func (r *ChatResponse) FinishReasons() []string {
	out := make([]string, 0, len(r.Choices))
	for _, c := range r.Choices {
		if c.FinishReason != "" {
			out = append(out, c.FinishReason)
		}
	}
	return out
}

// Chunk is one streaming delta.
type Chunk struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
}

// DeltaText returns the text carried by this chunk.
func (c *Chunk) DeltaText() string {
	if len(c.Choices) == 0 || c.Choices[0].Delta == nil {
		return ""
	}
	return c.Choices[0].Delta.Text()
}

// EmbeddingsRequest is the canonical embeddings request.
type EmbeddingsRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	User           string          `json:"user,omitempty"`
}

// Inputs decodes the input field, which may be a string or an array.
func (r *EmbeddingsRequest) Inputs() []string {
	var one string
	if err := json.Unmarshal(r.Input, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(r.Input, &many); err == nil {
		return many
	}
	return nil
}

// Embedding is one embedding vector.
type Embedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// EmbeddingsResponse is the canonical embeddings response.
type EmbeddingsResponse struct {
	Object string      `json:"object"`
	Data   []Embedding `json:"data"`
	Model  string      `json:"model"`
	Usage  *Usage      `json:"usage,omitempty"`
}

// Model is a logical model as advertised on /v1/models.
type Model struct {
	ID            string   `json:"id"`
	Object        string   `json:"object"`
	Created       int64    `json:"created"`
	OwnedBy       string   `json:"owned_by"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxOutput     int      `json:"max_output_tokens,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Pool          string   `json:"pool,omitempty"`
}

// ModelList is the /v1/models response envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
