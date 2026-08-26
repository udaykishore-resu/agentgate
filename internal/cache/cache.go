// Package cache implements the gateway's response cache: an exact-match cache
// keyed on the normalised request, and an optional semantic cache keyed on
// embedding similarity.
//
// Caching model responses is not free of consequences. Two requests that a
// hash says are identical may come from different tenants, and two requests
// that an embedding says are similar may differ in exactly the way that
// mattered. The rules here — never share across tenants, never cache
// tool-calling requests by default, never cache above a low temperature, and
// keep semantic matching off unless a pool explicitly enables it — exist
// because a wrong cache hit in a financial services context is not a
// performance problem, it is an incorrect answer delivered confidently.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/provider"
)

// Result is the outcome of a lookup, used as a metric label and returned in
// the x-agentgate-cache response header.
type Result string

// Lookup outcomes.
const (
	ResultHit      Result = "hit"
	ResultMiss     Result = "miss"
	ResultBypass   Result = "bypass"
	ResultRefresh  Result = "refresh"
	ResultSemantic Result = "semantic_hit"
)

// Entry is a cached response together with the provenance needed to keep a
// cache hit attributable: the trace that produced it and what it cost.
type Entry struct {
	Response  *provider.ChatResponse `json:"response"`
	Backend   string                 `json:"backend"`
	Model     string                 `json:"model"`
	TraceID   string                 `json:"trace_id"`
	CostUSD   float64                `json:"cost_usd"`
	CreatedAt time.Time              `json:"created_at"`
	// Embedding is retained only for semantic-cache candidates.
	Embedding []float64 `json:"embedding,omitempty"`
	// Text is the final user turn, kept so a semantic match can be logged and
	// audited when a hit is questioned.
	Text string `json:"text,omitempty"`
}

// Clone returns a deep copy of the cached response.
//
// The chain mutates the response it is handed — it rewrites the model name to
// the logical one and may redact the completion — so serving the stored
// pointer would let one request corrupt the entry other requests are reading,
// and would race with the JSON encoder writing the previous caller's response.
func (e *Entry) Clone() *provider.ChatResponse {
	if e == nil || e.Response == nil {
		return nil
	}
	src := e.Response
	out := *src
	out.Choices = make([]provider.Choice, len(src.Choices))
	copy(out.Choices, src.Choices)
	for i := range out.Choices {
		if src.Choices[i].Message != nil {
			msg := *src.Choices[i].Message
			msg.Content = append(json.RawMessage(nil), src.Choices[i].Message.Content...)
			msg.ToolCalls = append([]provider.ToolCall(nil), src.Choices[i].Message.ToolCalls...)
			out.Choices[i].Message = &msg
		}
		if src.Choices[i].Delta != nil {
			delta := *src.Choices[i].Delta
			delta.Content = append(json.RawMessage(nil), src.Choices[i].Delta.Content...)
			out.Choices[i].Delta = &delta
		}
	}
	if src.Usage != nil {
		usage := *src.Usage
		out.Usage = &usage
	}
	return &out
}

// Cache stores and retrieves responses.
type Cache interface {
	Get(ctx context.Context, key string) (*Entry, bool)
	Set(ctx context.Context, key string, e *Entry, ttl time.Duration)
	// Candidates returns recent entries for a tenant and pool, for semantic
	// matching. Returning nil disables semantic matching for that store.
	Candidates(ctx context.Context, tenant, pool string, limit int) []*Entry
	Delete(ctx context.Context, key string)
	Purge(ctx context.Context, tenant string) int
	Stats() Stats
}

// Stats reports cache behaviour.
type Stats struct {
	Entries  int   `json:"entries"`
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Evicted  int64 `json:"evicted"`
	Expired  int64 `json:"expired"`
	BytesApx int64 `json:"bytes_approx"`
}

// Policy is a pool's cache configuration.
type Policy struct {
	Enabled bool
	TTL     time.Duration
	// MaxTemperature disables caching above this sampling temperature. A high
	// temperature is a request for variety; serving the same answer twice
	// defeats the caller's intent.
	MaxTemperature float64
	// AllowTools permits caching of tool-calling requests. Off by default: a
	// tool call is an instruction to act, and replaying one from cache can
	// mean the action is skipped when the caller expected it to happen.
	AllowTools bool
	// Semantic enables embedding-similarity matching.
	Semantic           bool
	SemanticThreshold  float64
	SemanticCandidates int
}

// DefaultPolicy is the conservative default applied when a pool says nothing.
func DefaultPolicy() Policy {
	return Policy{
		Enabled: true, TTL: 10 * time.Minute, MaxTemperature: 0.2,
		AllowTools: false, Semantic: false, SemanticThreshold: 0.97, SemanticCandidates: 200,
	}
}

// Cacheable reports whether a request may be cached under a policy, and why
// not when it may not. The reason is attached to the span so a team asking
// "why is my cache hit rate zero" gets an answer from the trace.
func Cacheable(p Policy, req *provider.ChatRequest) (bool, string) {
	if !p.Enabled {
		return false, "cache disabled for pool"
	}
	if req.Stream && len(req.Tools) > 0 {
		return false, "streaming tool call"
	}
	if len(req.Tools) > 0 && !p.AllowTools {
		return false, "tool-calling request"
	}
	if req.Temperature != nil && *req.Temperature > p.MaxTemperature {
		return false, "temperature above cache threshold"
	}
	if req.N != nil && *req.N > 1 {
		return false, "multiple completions requested"
	}
	if req.Seed != nil {
		return false, "explicit seed requested"
	}
	return true, ""
}

// Key computes the exact-match cache key.
//
// The tenant is part of the key, not a namespace applied afterwards, so a
// cross-tenant hit is impossible by construction rather than by convention.
// The logical model is used rather than the backend model, because the caller
// asked for a logical model and expects consistent answers from it regardless
// of which backend happened to serve the last one.
func Key(tenant, logicalModel string, req *provider.ChatRequest) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0x1f})
		}
	}
	write("v2", tenant, logicalModel)
	for _, m := range req.Messages {
		// Plain-text content is whitespace-normalised, so trivial formatting
		// differences still hit. Structured content — images, audio, any
		// future part type — is hashed verbatim, because flattening it to its
		// text would make two requests carrying different images look
		// identical to the cache.
		if m.IsPlainText() {
			write(m.Role, "text", normaliseWhitespace(m.Text()), m.Name, m.ToolCallID)
		} else {
			write(m.Role, "parts", canonicalJSON(m.Content), m.Name, m.ToolCallID)
		}
		for _, tc := range m.ToolCalls {
			write(tc.Function.Name, tc.Function.Arguments)
		}
	}
	if req.MaxTokens != nil {
		write("max_tokens=" + strconv.Itoa(*req.MaxTokens))
	}
	if req.Temperature != nil {
		write("temperature=" + strconv.FormatFloat(*req.Temperature, 'f', 4, 64))
	}
	if req.TopP != nil {
		write("top_p=" + strconv.FormatFloat(*req.TopP, 'f', 4, 64))
	}
	// Every parameter the gateway forwards has to be in the key, or two
	// requests that will get different answers share one cached response.
	if req.PresencePenalty != nil {
		write("presence_penalty=" + strconv.FormatFloat(*req.PresencePenalty, 'f', 4, 64))
	}
	if req.FrequencyPenalty != nil {
		write("frequency_penalty=" + strconv.FormatFloat(*req.FrequencyPenalty, 'f', 4, 64))
	}
	if req.N != nil {
		write("n=" + strconv.Itoa(*req.N))
	}
	if len(req.ResponseFormat) > 0 {
		write("response_format=" + canonicalJSON(req.ResponseFormat))
	}
	if len(req.Stop) > 0 {
		write("stop=" + canonicalJSON(req.Stop))
	}
	for _, t := range req.Tools {
		write("tool=" + canonicalJSON(t.Function))
	}
	if len(req.ToolChoice) > 0 {
		write("tool_choice=" + canonicalJSON(req.ToolChoice))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normaliseWhitespace collapses runs of whitespace so that formatting
// differences that a model would not notice do not defeat the cache.
func normaliseWhitespace(s string) string { return strings.Join(strings.Fields(s), " ") }

// canonicalJSON re-encodes JSON with sorted object keys so that two
// semantically identical bodies produce the same key.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(sortKeys(v))
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func sortKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(map[string]any, len(t))
		for _, k := range keys {
			ordered[k] = sortKeys(t[k])
		}
		return ordered
	case []any:
		for i := range t {
			t[i] = sortKeys(t[i])
		}
		return t
	default:
		return v
	}
}
