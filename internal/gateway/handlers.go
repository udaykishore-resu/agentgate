package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/agentgate/agentgate/internal/cache"
	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/provider"
	"github.com/agentgate/agentgate/internal/telemetry"
)

// handleChat serves POST /v1/chat/completions for both unary and streaming
// requests.
func (s *Server) handleChat(ctx context.Context, rc *RequestContext) error {
	rc.Operation = OpChat
	req := &provider.ChatRequest{}
	raw, err := decodeJSON(rc.W, rc.R, s.cfg.Gateway.MaxBodyBytes, req)
	if err != nil {
		return err
	}
	rc.Body, rc.Chat = raw, req

	// Idempotency is resolved before the chain runs, because the whole point
	// is to avoid doing the work twice.
	if key := rc.R.Header.Get(HeaderIdempotencyKey); key != "" && !req.Stream {
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		if entry, hit, conflict := s.idem.get(key, hash); conflict {
			return httpx.Errorf(httpx.CodeIdempotencyConflict,
				"idempotency key %q was already used with a different request body", key)
		} else if hit {
			rc.WriteHeaders()
			rc.W.Header().Set("Content-Type", "application/json; charset=utf-8")
			rc.W.WriteHeader(entry.status)
			_, _ = rc.W.Write(entry.response)
			return nil
		}
	}

	ctx, cancel := s.withDeadline(ctx, req.Stream)
	defer cancel()

	if req.Stream {
		return s.serveStream(ctx, rc)
	}
	if err := s.runChain(ctx, rc); err != nil {
		return err
	}
	if rc.Response == nil {
		return httpx.NewProblem(httpx.CodeInternal, "the gateway produced no response")
	}

	body, err := json.Marshal(rc.Response)
	if err != nil {
		return httpx.NewProblem(httpx.CodeInternal, "the response could not be encoded")
	}
	// Cost is computed before headers are written so the caller receives the
	// figure for this call rather than discovering it in a monthly report.
	s.priceInFlight(rc)
	rc.WriteHeaders()
	rc.W.Header().Set("Content-Type", "application/json; charset=utf-8")
	rc.W.WriteHeader(http.StatusOK)
	_, _ = rc.W.Write(body)

	if rc.IdempotencyKey != "" {
		sum := sha256.Sum256(rc.Body)
		s.idem.put(rc.IdempotencyKey, hex.EncodeToString(sum[:]), http.StatusOK, body)
	}
	return nil
}

// withDeadline derives the request deadline, honouring a shorter caller
// deadline when the transport carries one.
func (s *Server) withDeadline(ctx context.Context, stream bool) (context.Context, context.CancelFunc) {
	d := s.cfg.Gateway.RequestTimeout.Or(60 * time.Second)
	if stream {
		d = s.cfg.Gateway.StreamTimeout.Or(10 * time.Minute)
	}
	return context.WithTimeout(ctx, d)
}

// serveStream runs the chain and then drives the SSE response.
//
// The split matters. Everything up to and including opening the provider
// stream is retryable and failoverable; everything after the first content
// byte is not. Keeping the two phases visibly separate is what stops a future
// change from quietly introducing a retry that duplicates half a completion in
// the caller's buffer.
func (s *Server) serveStream(ctx context.Context, rc *RequestContext) error {
	if err := s.runChain(ctx, rc); err != nil {
		return err
	}

	sse, err := httpx.NewSSEWriter(rc.W, s.cfg.Gateway.StreamHeartbeat.Or(15*time.Second))
	if err != nil {
		return httpx.NewProblem(httpx.CodeInternal, "this deployment cannot stream responses")
	}
	defer sse.Close()

	// A cache hit on a streaming request is replayed as a single chunk. It is
	// still a stream as far as the caller's parser is concerned.
	if rc.Response != nil && rc.StreamHandle == nil {
		s.priceInFlight(rc)
		rc.WriteHeaders()
		return s.replayCachedStream(sse, rc)
	}
	if rc.StreamHandle == nil {
		return httpx.NewProblem(httpx.CodeInternal, "no stream was opened")
	}

	rc.WriteHeaders()
	sse.WriteHeaderOnce()

	// The contract promises a failover frame when a stream was restarted on
	// another backend before any content reached the caller. Emitting it is
	// what makes an otherwise invisible routing decision auditable from the
	// client side; a strict OpenAI client ignores the named event.
	if rc.FailoverFrom != "" {
		_ = sse.Event("agentgate.failover", map[string]any{
			"request_id": rc.RequestID, "trace_id": rc.TraceID,
			"from": rc.FailoverFrom, "to": rc.BackendName(),
			"pool": rc.PoolName(), "attempts": rc.Attempts,
		})
	}

	scanner := guardrails.NewStreamScanner(rc.Pool.GuardrailProvider, rc.Pool.Guardrail, guardrails.Request{
		Pool: rc.Pool.Name, Tenant: rc.Claims.Tenant, AgentID: rc.Claims.AgentID,
		DataClassification: string(rc.Classification),
	})
	scanEnabled := rc.Pool.Guardrail.Enabled && rc.Pool.ScanOutput

	var (
		lastChunkAt = time.Now()
		completion  []string
		stalls      int
		chunkCount  int
		// Terminal frames — the finish reason and the provider's usage frame —
		// are held back while output scanning is on, because the scanner
		// releases content a window at a time and a client that sees
		// finish_reason before the last content chunk will truncate the answer.
		tail []*provider.Chunk
		// last is kept so the frame the scanner flushes at the end carries the
		// same completion id as the frames before it. A client correlating
		// chunks by id would otherwise see the final release as a new
		// completion.
		last *provider.Chunk
	)
	stallAfter := s.cfg.Gateway.StreamStallAfter.Or(30 * time.Second)

	for {
		chunk, ok, err := rc.StreamHandle.Recv()
		if err != nil {
			problem := httpx.AsProblem(toProblem(err))
			sse.ErrorFrame(problem, rc.RequestID, rc.TraceID)
			rc.Problem = problem
			return nil
		}
		if !ok {
			break
		}
		chunkCount++
		gap := time.Since(lastChunkAt)
		if chunkCount > 1 {
			s.metrics.InterToken.Observe(gap.Seconds(), s.env, rc.PoolName(), rc.BackendName())
			if gap > stallAfter {
				stalls++
				s.metrics.StreamStalls.Inc(s.env, rc.PoolName(), rc.BackendName())
			}
		}
		lastChunkAt = time.Now()
		last = chunk

		if chunk.Usage != nil {
			rc.Usage = chunk.Usage
		}
		text := chunk.DeltaText()
		if text != "" {
			completion = append(completion, text)
		}

		chunk.Model = rc.Model.Name
		if !scanEnabled {
			if text != "" {
				rc.markFirstByte()
			}
			if err := sse.Data(chunk); err != nil {
				return s.clientGone(rc, err)
			}
			continue
		}

		// With output scanning on, terminal information is held back until the
		// scanner has released the content that precedes it. A provider that
		// puts content and a finish reason in one frame — several
		// OpenAI-compatible servers do — has the two parts separated here,
		// rather than the whole frame being dropped.
		terminal := isTerminalChunk(chunk)
		if terminal {
			held := *chunk
			held.Choices = withoutDeltaContent(chunk.Choices)
			tail = append(tail, &held)
		}
		if text == "" {
			if !terminal {
				if err := sse.Data(chunk); err != nil {
					return s.clientGone(rc, err)
				}
			}
			continue
		}

		release, decision, allowed := scanner.Push(ctx, text)
		if !allowed {
			rc.GuardrailHeader = "blocked:" + decision.Category()
			s.metrics.Guardrail.Inc(s.env, rc.PoolName(), decision.Category(), string(guardrails.ActionBlock))
			problem := httpx.Errorf(httpx.CodeGuardrailBlocked,
				"the response was blocked mid-stream by content safety policy (%s)", decision.Category())
			sse.ErrorFrame(problem, rc.RequestID, rc.TraceID)
			rc.Problem = problem
			return nil
		}
		if release == "" {
			continue
		}
		rc.markFirstByte()
		if err := sse.Data(contentChunk(chunk, rc.Model.Name, release)); err != nil {
			return s.clientGone(rc, err)
		}
	}

	if scanEnabled {
		release, decision, allowed := scanner.Flush(ctx)
		if !allowed {
			rc.GuardrailHeader = "blocked:" + decision.Category()
			problem := httpx.Errorf(httpx.CodeGuardrailBlocked,
				"the response was blocked by content safety policy (%s)", decision.Category())
			sse.ErrorFrame(problem, rc.RequestID, rc.TraceID)
			rc.Problem = problem
			return nil
		}
		if release != "" {
			rc.markFirstByte()
			if err := sse.Data(contentChunk(last, rc.Model.Name, release)); err != nil {
				return s.clientGone(rc, err)
			}
		}
		if scanner.FailedOpen() {
			s.metrics.GuardrailFail.Inc(s.env, rc.PoolName(), string(rc.Pool.Guardrail.FailureMode))
		}
		for _, held := range tail {
			if err := sse.Data(held); err != nil {
				return s.clientGone(rc, err)
			}
		}
	}

	// Usage: providers that report it win; otherwise the completion is
	// estimated and the record is marked so, because an estimated number must
	// never be presented as a measured one in a chargeback report.
	if u := rc.StreamHandle.Usage(); u != nil {
		rc.Usage = u
	}
	if rc.Usage == nil || rc.Usage.CompletionTokens == 0 {
		rc.Usage = &provider.Usage{
			PromptTokens:     rc.EstimatedInput,
			CompletionTokens: provider.CountStreamTokens(completion),
		}
		rc.Usage.TotalTokens = rc.Usage.PromptTokens + rc.Usage.CompletionTokens
		rc.UsageEstimated = true
	}
	s.priceInFlight(rc)

	if rc.Chat.StreamOptions != nil && rc.Chat.StreamOptions.IncludeUsage {
		_ = sse.Event("agentgate.usage", map[string]any{
			"request_id": rc.RequestID, "trace_id": rc.TraceID,
			"provider": rc.Backend.Provider.Kind(), "backend": rc.BackendName(),
			"model": rc.Model.Name, "attempts": rc.Attempts,
			"usage": rc.Usage, "cost_usd": rc.CostUSD,
			"estimated": rc.UsageEstimated, "stalls": stalls,
		})
	}
	sse.Done()
	return nil
}

// contentChunk builds a delta-only frame carrying released text, preserving
// the source frame's identity where there is one.
func contentChunk(src *provider.Chunk, model, text string) *provider.Chunk {
	out := provider.Chunk{Object: "chat.completion.chunk", Model: model, Created: time.Now().Unix()}
	if src != nil {
		out.ID, out.SystemFingerprint = src.ID, src.SystemFingerprint
		if src.Created != 0 {
			out.Created = src.Created
		}
	}
	delta := &provider.Message{Role: "assistant"}
	delta.SetText(text)
	out.Choices = []provider.Choice{{Index: 0, Delta: delta}}
	return &out
}

// withoutDeltaContent strips the content from a frame's deltas, leaving the
// terminal fields. The content itself goes through the scanner.
func withoutDeltaContent(in []provider.Choice) []provider.Choice {
	out := make([]provider.Choice, len(in))
	copy(out, in)
	for i := range out {
		if out[i].Delta == nil {
			continue
		}
		delta := *out[i].Delta
		delta.Content = nil
		out[i].Delta = &delta
	}
	return out
}

// isTerminalChunk reports whether a chunk carries end-of-stream information: a
// finish reason, or the provider's final usage frame.
func isTerminalChunk(c *provider.Chunk) bool {
	if c.Usage != nil {
		return true
	}
	for _, ch := range c.Choices {
		if ch.FinishReason != "" {
			return true
		}
	}
	return false
}

// replayCachedStream serves a cached completion through the streaming contract.
func (s *Server) replayCachedStream(sse *httpx.SSEWriter, rc *RequestContext) error {
	sse.WriteHeaderOnce()
	rc.markFirstByte()
	chunk := provider.Chunk{
		ID: rc.Response.ID, Object: "chat.completion.chunk", Created: time.Now().Unix(),
		Model:   rc.Model.Name,
		Choices: []provider.Choice{{Index: 0, Delta: &provider.Message{Role: "assistant"}, FinishReason: "stop"}},
	}
	chunk.Choices[0].Delta.SetText(rc.Response.FirstText())
	if err := sse.Data(&chunk); err != nil {
		return s.clientGone(rc, err)
	}
	if rc.Chat.StreamOptions != nil && rc.Chat.StreamOptions.IncludeUsage {
		_ = sse.Event("agentgate.usage", map[string]any{
			"request_id": rc.RequestID, "trace_id": rc.TraceID,
			"cache": string(rc.CacheResult), "usage": rc.Usage,
			"cost_usd": 0.0, "savings_usd": rc.SavingsUSD,
		})
	}
	sse.Done()
	return nil
}

// clientGone records a caller disconnect. It is not a gateway failure and must
// not be counted as one, or every cancelled agent run would burn error budget.
func (s *Server) clientGone(rc *RequestContext, err error) error {
	if errors.Is(err, context.Canceled) || rc.R.Context().Err() != nil {
		rc.Problem = httpx.NewProblem(httpx.CodeClientClosedRequest, "the caller disconnected mid-stream")
		rc.Span.SetAttributes(telemetry.Attr(telemetry.AttrErrorCode, string(httpx.CodeClientClosedRequest)))
		return nil
	}
	rc.Problem = httpx.NewProblem(httpx.CodeClientClosedRequest, "the response stream could not be written")
	return nil
}

// priceInFlight computes the cost for the response headers before the record
// is written.
func (s *Server) priceInFlight(rc *RequestContext) {
	if rc.Claims == nil {
		return
	}
	rec := s.usageRecord(rc, http.StatusOK, "", time.Since(rc.Start))
	_ = rec
}

// handleEmbeddings serves POST /v1/embeddings.
func (s *Server) handleEmbeddings(ctx context.Context, rc *RequestContext) error {
	rc.Operation = OpEmbeddings
	req := &provider.EmbeddingsRequest{}
	raw, err := decodeJSON(rc.W, rc.R, s.cfg.Gateway.MaxBodyBytes, req)
	if err != nil {
		return err
	}
	rc.Body, rc.Embed = raw, req

	ctx, cancel := s.withDeadline(ctx, false)
	defer cancel()
	if err := s.runChain(ctx, rc); err != nil {
		return err
	}
	if rc.EmbedResponse == nil {
		return httpx.NewProblem(httpx.CodeInternal, "the gateway produced no response")
	}
	rc.EmbedResponse.Model = rc.Model.Name
	s.priceInFlight(rc)
	rc.WriteHeaders()
	httpx.WriteJSON(rc.W, http.StatusOK, rc.EmbedResponse)
	return nil
}

// handleTokenCount serves POST /v1/token-count, a pre-flight estimate that
// lets an agent decide whether to trim its context before spending a request.
func (s *Server) handleTokenCount(ctx context.Context, rc *RequestContext) error {
	rc.Operation = OpTokenCount
	req := &provider.ChatRequest{}
	raw, err := decodeJSON(rc.W, rc.R, s.cfg.Gateway.MaxBodyBytes, req)
	if err != nil {
		return err
	}
	rc.Body, rc.Chat = raw, req
	if err := s.runChain(ctx, rc); err != nil {
		return err
	}
	limits := s.quotaLimitsFor(ctx, rc.Claims)
	used, _ := s.limiter.Usage(ctx, rc.Claims.QuotaKey())
	rc.WriteHeaders()
	httpx.WriteJSON(rc.W, http.StatusOK, map[string]any{
		"model":                   rc.Model.Name,
		"estimated_input_tokens":  rc.EstimatedInput,
		"context_window":          rc.Model.ContextWindow,
		"max_output_tokens":       rc.Model.MaxOutput,
		"estimated":               true,
		"monthly_tokens_used":     used,
		"monthly_token_budget":    limits.MonthlyTokenBudget,
		"tokens_per_minute_limit": limits.TokensPerMinute,
	})
	return nil
}

// handleModels serves GET /v1/models, listing only what this agent may use.
func (s *Server) handleModels(ctx context.Context, rc *RequestContext) error {
	if err := s.stageAuthn(ctx, rc); err != nil {
		return err
	}
	models := s.providerModelsFor(rc.Claims)
	rc.WriteHeaders()
	httpx.WriteJSON(rc.W, http.StatusOK, provider.ModelList{Object: "list", Data: models})
	return nil
}

// handleModel serves GET /v1/models/{id}.
func (s *Server) handleModel(ctx context.Context, rc *RequestContext) error {
	if err := s.stageAuthn(ctx, rc); err != nil {
		return err
	}
	id := rc.R.PathValue("id")
	for _, m := range s.providerModelsFor(rc.Claims) {
		if m.ID == id {
			rc.WriteHeaders()
			httpx.WriteJSON(rc.W, http.StatusOK, m)
			return nil
		}
	}
	return httpx.Errorf(httpx.CodeUnknownModel, "unknown model %q", id)
}

// cacheResultLabel is used by the fleet view when reporting cache behaviour.
func cacheResultLabel(r cache.Result) string {
	if r == "" {
		return string(cache.ResultBypass)
	}
	return string(r)
}
