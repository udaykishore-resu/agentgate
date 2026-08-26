package gateway

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/provider"
	"github.com/agentgate/agentgate/internal/resilience"
	"github.com/agentgate/agentgate/internal/telemetry"
)

// invoke runs the request against the pool, retrying within a backend and
// failing over between backends.
//
// The structure is two nested loops: the outer loop walks candidate backends
// in selection order, the inner loop retries one backend. That ordering is
// deliberate — retrying the same backend is cheap and often works for a
// throttle, while failing over changes the model that answers, and a caller
// that gets a different model's answer should have exhausted the cheap option
// first.
//
// Three rules keep it from becoming an amplifier:
//   - a non-retryable failure never retries and never fails over, because
//     "your request is malformed" will be equally malformed at the next
//     backend;
//   - every wait is charged against both the caller's deadline and the fleet
//     retry budget;
//   - a streaming response only fails over before the first content byte.
func (s *Server) invoke(ctx context.Context, rc *RequestContext) error {
	candidates := rc.Pool.Select(rc.Classification, false)
	if len(candidates) == 0 {
		// Distinguish "everything is broken" from "nothing was ever eligible":
		// the first is an incident, the second is a configuration or
		// classification problem, and they have different runbooks.
		if all := rc.Pool.Select(rc.Classification, true); len(all) == 0 {
			return httpx.Errorf(httpx.CodeNoHealthyBackend,
				"no backend in pool %q accepts data classified as %q", rc.Pool.Name, rc.Classification)
		}
		return httpx.Errorf(httpx.CodeNoHealthyBackend,
			"every backend in pool %q is unavailable", rc.Pool.Name).WithRetryAfter(5)
	}
	rc.Candidates = candidates

	s.retryBudget.Request()
	var lastErr error
	spentWaiting := time.Duration(0)
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))

	served := ""
	for ci, backend := range candidates {
		if ci > 0 {
			rc.FailoverFrom = served
			s.log.Warn("failing over",
				"from", rc.FailoverFrom, "to", backend.Name, "pool", rc.Pool.Name,
				"request_id", rc.RequestID, "reason", errText(lastErr))
		}

		for attempt := 1; attempt <= rc.Pool.Retry.MaxAttempts; attempt++ {
			if err := ctx.Err(); err != nil {
				return deadlineProblem(err)
			}
			// Reserve the call. For a closed breaker this is free; for a
			// half-open one it takes one of the bounded probes, which is why
			// it happens here and not in the routing filter.
			if !backend.Available() || !backend.breaker.Allow() {
				break // the breaker opened or ran out of probes while we waited
			}
			// Only now does this backend own the request, so the response
			// headers and the usage record name the backend that was actually
			// called rather than one that was merely considered.
			rc.Backend = backend
			served = backend.Name
			rc.Attempts++

			err := s.attempt(ctx, rc, backend, attempt)
			if err == nil {
				backend.breaker.Success()
				backend.successes.Add(1)
				return nil
			}
			lastErr = err

			pe, isProviderErr := provider.AsError(err)
			if isProviderErr {
				backend.lastError.Store(pe.Message)
			}
			if isProviderErr && classifyBreaker(pe) {
				backend.breaker.Failure()
				backend.failures.Add(1)
			} else {
				// A caller error is not evidence that the backend is unwell.
				// Counting it would open breakers during a bad deployment of a
				// consuming agent and turn their bug into a platform outage.
				backend.breaker.Success()
			}

			if !retryable(err) {
				// Retrying this backend cannot help, but another backend
				// might: an expired credential or a wrong deployment name is
				// this gateway's problem with one provider, not the caller's
				// with all of them.
				if !failoverAllowed(rc, err) {
					return toProblem(err)
				}
				break
			}
			if rc.Stream && rc.streamStarted() {
				return toProblem(err)
			}
			if attempt == rc.Pool.Retry.MaxAttempts {
				break
			}
			var retryAfter time.Duration
			reason := "unknown"
			if isProviderErr {
				retryAfter, reason = pe.RetryAfter, string(pe.Kind)
			}
			wait := rc.Pool.Retry.Backoff(attempt, retryAfter, rnd)
			// The deadline check comes before the budget is charged: a retry
			// the caller has no time for must not count against the fleet's
			// allowance, or the budget exhausts fastest exactly when a slow
			// backend makes it matter most.
			if budget := rc.Pool.Retry.RemainingBudget(ctx, spentWaiting); wait > budget {
				s.metrics.Retries.Inc(s.env, backend.Name, "deadline_budget")
				break
			}
			if !s.retryBudget.Allow() {
				s.metrics.Retries.Inc(s.env, backend.Name, "budget_exhausted")
				s.log.Warn("retry budget exhausted; failing fast",
					"backend", backend.Name, "ratio", s.retryBudget.Ratio())
				break
			}
			s.metrics.Retries.Inc(s.env, backend.Name, reason)
			rc.Span.AddEvent("retry",
				telemetry.Attr(telemetry.AttrBackend, backend.Name),
				telemetry.Attr(telemetry.AttrAttempt, attempt),
				telemetry.Attr(telemetry.AttrRetryReason, reason),
				telemetry.Attr("agentgate.retry.delay_ms", wait.Milliseconds()),
			)
			if err := resilience.Sleep(ctx, wait); err != nil {
				return deadlineProblem(err)
			}
			spentWaiting += wait
		}

		if !failoverAllowed(rc, lastErr) {
			break
		}
	}
	return toProblem(lastErr)
}

// attempt performs a single call against one backend, wrapped in its own span
// so a trace shows every attempt rather than only the one that worked.
func (s *Server) attempt(ctx context.Context, rc *RequestContext, backend *Backend, n int) error {
	spanName := telemetry.SpanGenAIChat
	if rc.Operation == OpEmbeddings {
		spanName = telemetry.SpanGenAIEmbeddings
	}
	ctx, span := s.tracer.Start(ctx, spanName,
		telemetry.WithSpanKind(telemetry.KindClient),
		telemetry.WithAttributes(
			telemetry.Attr(telemetry.AttrGenAISystem, string(backend.Provider.Kind())),
			telemetry.Attr(telemetry.AttrGenAIOperation, string(rc.Operation)),
			telemetry.Attr(telemetry.AttrGenAIRequestModel, backend.Provider.Model()),
			telemetry.Attr(telemetry.AttrLogicalModel, rc.Model.Name),
			telemetry.Attr(telemetry.AttrPool, rc.Pool.Name),
			telemetry.Attr(telemetry.AttrBackend, backend.Name),
			telemetry.Attr(telemetry.AttrAttempt, n),
			telemetry.Attr(telemetry.AttrStream, rc.Stream),
		))
	// A streaming attempt outlives this function, so its span is ended by the
	// stream consumer instead; that way the span's duration covers the whole
	// generation rather than just the time to open the connection.
	endHere := true
	defer func() {
		if endHere {
			span.End()
		}
	}()
	if rc.FailoverFrom != "" {
		span.SetAttributes(telemetry.Attr(telemetry.AttrFailoverFrom, rc.FailoverFrom))
	}

	backend.inflight.Add(1)
	defer backend.inflight.Add(-1)

	started := time.Now()
	defer func() { rc.ProviderTime += time.Since(started) }()

	attemptCtx := ctx
	var attemptCancel context.CancelFunc
	if backend.Timeout > 0 {
		attemptCtx, attemptCancel = context.WithTimeout(ctx, backend.Timeout)
		if !rc.Stream {
			defer attemptCancel()
		}
	}
	// On the streaming path the context outlives this function only if a
	// stream is actually opened. Releasing it on every other exit is what
	// stops a failed attempt leaking a timer on each retry.
	releaseUnusedCancel := func() {
		if attemptCancel != nil {
			attemptCancel()
			attemptCancel = nil
		}
	}

	switch {
	case rc.Operation == OpEmbeddings:
		resp, err := backend.Provider.Embeddings(attemptCtx, rc.Embed)
		if err != nil {
			releaseUnusedCancel()
			recordAttemptError(span, err)
			return err
		}
		rc.EmbedResponse = resp
		if resp.Usage != nil {
			rc.Usage = resp.Usage
			span.SetAttributes(
				telemetry.Attr(telemetry.AttrGenAIUsageInput, resp.Usage.PromptTokens),
				telemetry.Attr(telemetry.AttrGenAIUsageOutput, resp.Usage.CompletionTokens),
			)
		}
		return nil

	case rc.Stream:
		stream, err := backend.Provider.ChatStream(attemptCtx, rc.Chat)
		if err != nil {
			releaseUnusedCancel()
			recordAttemptError(span, err)
			return err
		}
		rc.StreamHandle = stream
		rc.streamSpan = span
		rc.streamCancel = attemptCancel
		attemptCancel = nil
		endHere = false
		span.SetAttributes(telemetry.Attr("agentgate.stream.opened", true))
		return nil

	default:
		resp, err := backend.Provider.Chat(attemptCtx, rc.Chat)
		if err != nil {
			recordAttemptError(span, err)
			return err
		}
		rc.Response = resp
		if resp.Usage != nil {
			rc.Usage = resp.Usage
		} else {
			rc.Usage = &provider.Usage{
				PromptTokens:     rc.EstimatedInput,
				CompletionTokens: provider.EstimateTokens(resp.FirstText()),
			}
			rc.Usage.TotalTokens = rc.Usage.PromptTokens + rc.Usage.CompletionTokens
			rc.UsageEstimated = true
		}
		span.SetAttributes(
			telemetry.Attr(telemetry.AttrGenAIResponseModel, resp.Model),
			telemetry.Attr(telemetry.AttrGenAIResponseID, resp.ID),
			telemetry.Attr(telemetry.AttrGenAIUsageInput, rc.Usage.PromptTokens),
			telemetry.Attr(telemetry.AttrGenAIUsageOutput, rc.Usage.CompletionTokens),
			telemetry.Attr(telemetry.AttrGenAIFinishReasons, resp.FinishReasons()),
		)
		return nil
	}
}

func recordAttemptError(span *telemetry.Span, err error) {
	span.RecordError(err)
	if pe, ok := provider.AsError(err); ok {
		span.SetAttributes(
			telemetry.Attr("agentgate.provider.error_kind", string(pe.Kind)),
			telemetry.Attr("agentgate.provider.status_code", pe.StatusCode),
		)
	}
}

// retryable reports whether another attempt could plausibly succeed.
func retryable(err error) bool {
	if pe, ok := provider.AsError(err); ok {
		return pe.Retryable()
	}
	return false
}

// classifyBreaker reports whether a failure is evidence about the backend's
// health, as opposed to evidence about the caller's request.
func classifyBreaker(pe *provider.Error) bool {
	switch pe.Kind {
	case provider.ErrBadRequest, provider.ErrContextLength, provider.ErrContentFilter, provider.ErrNotFound:
		return false
	default:
		return true
	}
}

// failoverAllowed reports whether the request may be tried on another backend.
func failoverAllowed(rc *RequestContext, err error) bool {
	if rc.Stream && rc.streamStarted() {
		return false
	}
	if pe, ok := provider.AsError(err); ok {
		switch pe.Kind {
		case provider.ErrBadRequest, provider.ErrContextLength, provider.ErrNotFound:
			// The next backend will reject it identically; failing over only
			// spends someone else's quota to produce the same error.
			return false
		case provider.ErrContentFilter:
			// A provider-side content refusal is a policy outcome, not an
			// availability event. Shopping for a backend that will answer is
			// exactly what a regulated client does not want.
			return false
		}
	}
	return true
}

// toProblem converts an invocation failure into the frozen error contract.
func toProblem(err error) error {
	if err == nil {
		return nil
	}
	var p *httpx.Problem
	if errors.As(err, &p) {
		return p
	}
	pe, ok := provider.AsError(err)
	if !ok {
		return httpx.NewProblem(httpx.CodeInternal, "the gateway encountered an unexpected error")
	}
	switch pe.Kind {
	case provider.ErrTimeout:
		return httpx.Errorf(httpx.CodeProviderTimeout, "upstream provider did not respond within the deadline")
	case provider.ErrThrottled:
		return httpx.Errorf(httpx.CodeRateLimited, "upstream provider is throttling this request").
			WithRetryAfter(retryAfterSeconds(pe.RetryAfter))
	case provider.ErrContextLength:
		return httpx.Errorf(httpx.CodeContextTooLarge, "the prompt exceeds the model's context window")
	case provider.ErrContentFilter:
		return httpx.Errorf(httpx.CodeGuardrailBlocked, "the upstream provider refused the request under its content policy")
	case provider.ErrBadRequest:
		return httpx.Errorf(httpx.CodeInvalidRequest, "the upstream provider rejected the request as invalid")
	case provider.ErrUnavailable, provider.ErrOverloaded, provider.ErrTransport:
		return httpx.Errorf(httpx.CodeProviderError, "upstream provider is unavailable").WithRetryAfter(5)
	case provider.ErrAuth:
		// The caller authenticated fine; the gateway's own credential for the
		// backend did not. That is a platform fault and must not be reported
		// as the caller's.
		return httpx.Errorf(httpx.CodeProviderError, "the gateway could not authenticate to the upstream provider")
	default:
		return httpx.Errorf(httpx.CodeProviderError, "upstream provider error")
	}
}

func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 5
	}
	s := int(d.Seconds())
	if s < 1 {
		return 1
	}
	return s
}

func deadlineProblem(err error) error {
	if errors.Is(err, context.Canceled) {
		return httpx.NewProblem(httpx.CodeClientClosedRequest, "the caller disconnected")
	}
	return httpx.NewProblem(httpx.CodeClientTimeout, "the caller's deadline was exceeded")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
