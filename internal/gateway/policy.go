package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/agentgate/agentgate/internal/cache"
	"github.com/agentgate/agentgate/internal/config"
	"github.com/agentgate/agentgate/internal/guardrails"
	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/provider"
	"github.com/agentgate/agentgate/internal/ratelimit"
	"github.com/agentgate/agentgate/internal/resilience"
	"github.com/agentgate/agentgate/internal/telemetry"
)

// Stage is one step of the policy chain.
//
// Stages are named and individually timed. That is not decoration: "where did
// the latency go" is the most common question asked of a gateway, and a chain
// whose stages are anonymous can only ever be answered with a guess.
type Stage interface {
	Name() string
	Execute(ctx context.Context, rc *RequestContext) error
}

// StageFunc adapts a function to the Stage interface.
type StageFunc struct {
	name string
	fn   func(context.Context, *RequestContext) error
}

// Name returns the stage name.
func (s StageFunc) Name() string { return s.name }

// Execute runs the stage.
func (s StageFunc) Execute(ctx context.Context, rc *RequestContext) error { return s.fn(ctx, rc) }

func stage(name string, fn func(context.Context, *RequestContext) error) Stage {
	return StageFunc{name: name, fn: fn}
}

// buildChain assembles the policy chain in the order documented in SPEC.md
// section 3.2. The order is load-bearing: authentication before authorisation,
// authorisation before any work is done on the caller's behalf, limits before
// a backend is touched, guardrails before content leaves the estate, and cache
// lookup before transformation so a hit costs nothing but a hash.
func (s *Server) buildChain() []Stage {
	return []Stage{
		stage("authn", s.stageAuthn),
		stage("authz", s.stageAuthz),
		stage("admission", s.stageAdmission),
		stage("ratelimit", s.stageRateLimit),
		stage("quota", s.stageQuota),
		stage("guardrail_input", s.stageGuardrailInput),
		stage("cache_lookup", s.stageCacheLookup),
		stage("transform_request", s.stageTransformRequest),
		stage("route", s.stageRoute),
		stage("invoke", s.stageInvoke),
		stage("transform_response", s.stageTransformResponse),
		stage("guardrail_output", s.stageGuardrailOutput),
		stage("cache_store", s.stageCacheStore),
	}
}

// runChain executes the chain, timing each stage. A stage that sets a response
// short-circuits the ones that would have produced it.
func (s *Server) runChain(ctx context.Context, rc *RequestContext) error {
	for _, st := range s.chain {
		start := time.Now()
		err := st.Execute(ctx, rc)
		rc.RecordStage(st.Name(), time.Since(start))
		if err != nil {
			rc.Span.SetAttributes(telemetry.Attr(telemetry.AttrPolicyStage, st.Name()))
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return deadlineProblem(ctxErr)
		}
	}
	return nil
}

// --- stage 2: authentication ---------------------------------------------

func (s *Server) stageAuthn(ctx context.Context, rc *RequestContext) error {
	if s.cfg.Identity.AllowUnverified {
		// Local development only; refused in staging and production by config
		// validation. The synthetic identity is obviously synthetic so it can
		// never be mistaken for a real one in a trace.
		rc.Claims = developerClaims(rc.R)
		s.metrics.TokenValidations.Inc(s.env, "bypassed", "allow_unverified")
		s.stampIdentity(rc)
		return nil
	}
	raw, ok := identity.BearerToken(rc.R.Header)
	if !ok {
		s.metrics.TokenValidations.Inc(s.env, "failure", identity.ReasonMissingToken)
		return httpx.NewProblem(httpx.CodeUnauthenticated,
			"an Authorization: Bearer token issued by the AgentGate control plane is required")
	}
	claims, err := s.verifier.Verify(ctx, raw)
	if err != nil {
		var reason string
		if ue, ok := err.(*identity.ErrUnauthenticated); ok {
			reason = ue.Reason
		}
		s.log.Info("token rejected", "request_id", rc.RequestID, "reason", reason)
		rc.Span.SetAttributes(telemetry.Attr("agentgate.authn.reason", reason))
		return httpx.NewProblem(httpx.CodeUnauthenticated, "the presented token could not be verified")
	}
	rc.Claims = claims
	s.stampIdentity(rc)
	return nil
}

// stampIdentity puts the verified ownership attributes on the span. The token
// is authoritative: an agent may report whatever it likes in its own
// resource attributes, and where the two disagree the gateway records the
// correction rather than silently trusting either.
func (s *Server) stampIdentity(rc *RequestContext) {
	c := rc.Claims
	rc.Classification = classificationFor(c)
	rc.Span.SetAttributes(
		telemetry.Attr(telemetry.AttrAgentID, c.AgentID),
		telemetry.Attr(telemetry.AttrAgentIdentity, c.Subject),
		telemetry.Attr(telemetry.AttrAgentVersion, c.AgentVersion),
		telemetry.Attr(telemetry.AttrTenantID, c.Tenant),
		telemetry.Attr(telemetry.AttrTeamID, c.Team),
		telemetry.Attr(telemetry.AttrOwnerEmail, c.OwnerEmail),
		telemetry.Attr(telemetry.AttrCostCenter, c.CostCenter),
		telemetry.Attr(telemetry.AttrRuntime, c.Runtime),
		telemetry.Attr(telemetry.AttrFramework, c.Framework),
		telemetry.Attr(telemetry.AttrDeploymentEnv, string(c.Env)),
		telemetry.Attr(telemetry.AttrDataClassification, string(rc.Classification)),
		telemetry.Attr("agentgate.attestation", string(c.Attestation)),
	)
	if sid := rc.SessionID(); sid != "" {
		rc.Span.SetAttributes(telemetry.Attr(telemetry.AttrSessionID, sid))
	}
	if c.CostCenter == "" {
		rc.AttributionCorrected = true
		rc.Span.SetAttributes(telemetry.Attr(telemetry.AttrAttributionFixed, true))
	}
	s.metrics.TokenValidations.Inc(s.env, "success", "")
}

// developerClaims synthesises an identity for local development, where no
// control plane is running. Everything about it is marked as developer-issued
// so that a trace, a metric or a usage record produced this way can never be
// mistaken for a real agent's.
func developerClaims(r *http.Request) *identity.Claims {
	tenant := headerOr(r, "x-agentgate-dev-tenant", "localdev")
	team := headerOr(r, "x-agentgate-dev-team", "platform")
	name := headerOr(r, "x-agentgate-dev-agent", "developer")
	id, err := identity.ParseAgentIdentity("agent://" + tenant + "/" + team + "/" + name)
	if err != nil {
		id, _ = identity.ParseAgentIdentity("agent://localdev/platform/developer")
	}
	return &identity.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: id.String()},
		AgentID:          "agt_developer",
		Tenant:           id.Tenant,
		Team:             id.Team,
		AgentName:        id.Name,
		AgentVersion:     "0.0.0-dev",
		Env:              identity.EnvDev,
		CostCenter:       "CC-DEV",
		Scopes:           []string{identity.ScopeInvoke, identity.ScopeEmbed},
		ModelPools:       []string{"*"},
		Attestation:      identity.AttestDeveloper,
	}
}

func headerOr(r *http.Request, key, def string) string {
	if v := r.Header.Get(key); v != "" {
		return v
	}
	return def
}

// --- stage 3: authorisation ----------------------------------------------

func (s *Server) stageAuthz(_ context.Context, rc *RequestContext) error {
	c := rc.Claims
	scope := identity.ScopeInvoke
	if rc.Operation == OpEmbeddings {
		scope = identity.ScopeEmbed
	}
	if !c.HasScope(scope) {
		return httpx.Errorf(httpx.CodeForbiddenPool,
			"this token does not carry the %s scope", scope)
	}
	if string(c.Env) != s.env && s.env != "" && s.cfg.Env != "dev" {
		// A staging token presented to the production gateway is not a
		// borderline case; it is the exact thing the promotion gate exists to
		// prevent, and it fails here as well as there.
		return httpx.Errorf(httpx.CodeAgentNotPromoted,
			"this token is scoped to the %s environment and this gateway serves %s", c.Env, s.env)
	}
	if rc.Operation == OpChat || rc.Operation == OpEmbeddings || rc.Operation == OpTokenCount {
		model, pool, problem := s.resolveModel(rc)
		if problem != nil {
			return problem
		}
		rc.Model, rc.Pool = model, pool
		if !c.MayUsePool(pool.Name) {
			if len(c.ModelPools) == 0 {
				// A production token with no entitlements is the normal state
				// of a newly promoted agent: the platform team grants pools
				// separately. Saying so turns a confusing 403 into an action.
				return httpx.Errorf(httpx.CodeForbiddenPool,
					"agent %s has no pool entitlements in %s; the platform team must grant pools before production traffic is permitted",
					c.Subject, c.Env)
			}
			return httpx.Errorf(httpx.CodeForbiddenPool,
				"agent %s is not entitled to pool %q; entitled pools are %s",
				c.Subject, pool.Name, strings.Join(c.ModelPools, ", "))
		}
		rc.Span.SetAttributes(
			telemetry.Attr(telemetry.AttrLogicalModel, model.Name),
			telemetry.Attr(telemetry.AttrPool, pool.Name),
		)
	}
	return nil
}

// resolveModel maps the requested logical model, honouring an explicit pool
// override only when the token entitles the caller to it.
func (s *Server) resolveModel(rc *RequestContext) (config.Model, *Pool, *httpx.Problem) {
	name := ""
	switch {
	case rc.Chat != nil:
		name = rc.Chat.Model
	case rc.Embed != nil:
		name = rc.Embed.Model
	}
	if name == "" {
		return config.Model{}, nil, httpx.NewProblem(httpx.CodeInvalidRequest, "model is required").WithParam("model")
	}
	model, pool, problem := s.router.Resolve(name)
	if problem != nil {
		return config.Model{}, nil, problem
	}
	if override := rc.R.Header.Get(HeaderPool); override != "" {
		p, ok := s.router.Pool(override)
		if !ok {
			return config.Model{}, nil, httpx.Errorf(httpx.CodeInvalidRequest, "unknown pool %q", override)
		}
		if !rc.Claims.MayUsePool(override) {
			return config.Model{}, nil, httpx.Errorf(httpx.CodeForbiddenPool,
				"agent %s is not entitled to pool %q", rc.Claims.Subject, override)
		}
		pool = p
	}
	return model, pool, nil
}

// --- stage 4: admission ---------------------------------------------------

func (s *Server) stageAdmission(_ context.Context, rc *RequestContext) error {
	switch rc.Operation {
	case OpChat, OpTokenCount:
		if rc.Chat == nil || len(rc.Chat.Messages) == 0 {
			return httpx.NewProblem(httpx.CodeInvalidRequest, "messages must contain at least one message").WithParam("messages")
		}
		for i, m := range rc.Chat.Messages {
			switch m.Role {
			case "system", "user", "assistant", "tool", "developer", "function":
			default:
				return httpx.Errorf(httpx.CodeInvalidRequest,
					"messages[%d].role %q is not a recognised role", i, m.Role).WithParam("messages")
			}
		}
		if rc.Chat.Temperature != nil && (*rc.Chat.Temperature < 0 || *rc.Chat.Temperature > 2) {
			return httpx.NewProblem(httpx.CodeInvalidRequest, "temperature must be between 0 and 2").WithParam("temperature")
		}
		if rc.Chat.TopP != nil && (*rc.Chat.TopP <= 0 || *rc.Chat.TopP > 1) {
			return httpx.NewProblem(httpx.CodeInvalidRequest, "top_p must be greater than 0 and at most 1").WithParam("top_p")
		}
		rc.EstimatedInput = provider.EstimateRequestTokens(rc.Chat)
		maxOut := rc.Chat.MaxOutputTokens(s.cfg.Gateway.DefaultMaxTokens)
		if rc.Model.MaxOutput > 0 && maxOut > rc.Model.MaxOutput {
			return httpx.Errorf(httpx.CodeInvalidRequest,
				"max_tokens %d exceeds the maximum output of %d for model %s",
				maxOut, rc.Model.MaxOutput, rc.Model.Name).WithParam("max_tokens")
		}
		if rc.Model.ContextWindow > 0 && rc.EstimatedInput+maxOut > rc.Model.ContextWindow {
			// Refusing here rather than paying a provider to refuse is worth
			// real money at fleet scale, and it gives the caller a precise
			// number instead of a provider's phrasing.
			return httpx.Errorf(httpx.CodeContextTooLarge,
				"estimated %d input tokens plus %d requested output tokens exceeds the %d token context window of %s",
				rc.EstimatedInput, maxOut, rc.Model.ContextWindow, rc.Model.Name)
		}
		if rc.Chat.MaxTokens == nil {
			d := s.cfg.Gateway.DefaultMaxTokens
			rc.Chat.MaxTokens = &d
		}
		rc.Stream = rc.Chat.Stream
	case OpEmbeddings:
		if rc.Embed == nil || len(rc.Embed.Inputs()) == 0 {
			return httpx.NewProblem(httpx.CodeInvalidRequest, "input is required").WithParam("input")
		}
		rc.EstimatedInput = provider.EstimateEmbeddingsTokens(rc.Embed)
	}

	if key := rc.R.Header.Get(HeaderIdempotencyKey); key != "" && !rc.Stream {
		rc.IdempotencyKey = key
		rc.Span.SetAttributes(telemetry.Attr(telemetry.AttrIdempotencyKey, key))
	}
	rc.Span.SetAttributes(
		telemetry.Attr("agentgate.estimated_input_tokens", rc.EstimatedInput),
		telemetry.Attr(telemetry.AttrStream, rc.Stream),
	)
	return nil
}

// --- stage 5: request rate limit -----------------------------------------

func (s *Server) stageRateLimit(ctx context.Context, rc *RequestContext) error {
	limits := s.quotas.Limits(ctx, rc.Claims)
	env, tenant, team, agent := rc.TenantLabels()
	res, err := s.limiter.AllowRequest(ctx, rc.Claims.QuotaKey(), limits)
	if err != nil {
		// The limiter failing is not the caller's fault. Degrading loudly and
		// continuing is the documented behaviour; see the fallback limiter.
		s.log.Error("rate limiter error", "error", err, "request_id", rc.RequestID)
		rc.Degraded = true
		return nil
	}
	s.metrics.RateLimit.Inc(env, tenant, team, agent, string(res.Decision))
	if !res.Allowed() {
		rc.RateLimit = res
		return httpx.Errorf(httpx.CodeRateLimited,
			"%s exceeded %d requests per minute", rc.Claims.Subject, res.Limit).
			WithRetryAfter(retryAfterSeconds(res.RetryAfter))
	}
	return nil
}

// --- stage 6: token quota -------------------------------------------------

func (s *Server) stageQuota(ctx context.Context, rc *RequestContext) error {
	if rc.Operation == OpTokenCount {
		return nil
	}
	limits := s.quotas.Limits(ctx, rc.Claims)
	env, tenant, team, agent := rc.TenantLabels()

	reserve := int64(rc.EstimatedInput)
	if rc.Chat != nil {
		reserve += int64(rc.Chat.MaxOutputTokens(s.cfg.Gateway.DefaultMaxTokens))
	}
	rc.ReservedTokens = reserve

	res, reservation, err := s.limiter.Reserve(ctx, rc.Claims.QuotaKey(), reserve, limits)
	if err != nil {
		s.log.Error("quota reservation error", "error", err, "request_id", rc.RequestID)
		rc.Degraded = true
		return nil
	}
	rc.RateLimit = res
	s.metrics.RateLimit.Inc(env, tenant, team, agent, string(res.Decision))
	rc.Span.SetAttributes(telemetry.Attr(telemetry.AttrQuotaReserved, reserve))

	if !res.Allowed() {
		detail := res.Reason
		if detail == "" {
			detail = "quota exceeded"
		}
		return httpx.Errorf(httpx.CodeQuotaExceeded, "%s: %s", rc.Claims.Subject, detail).
			WithRetryAfter(retryAfterSeconds(res.RetryAfter))
	}
	rc.Reservation = reservation
	if limits.MonthlyTokenBudget > 0 {
		if used, err := s.limiter.Usage(ctx, rc.Claims.QuotaKey()); err == nil {
			s.metrics.QuotaUsedRatio.Set(float64(used)/float64(limits.MonthlyTokenBudget), env, tenant, team, agent)
		}
	}
	return nil
}

// --- stage 7: input guardrail --------------------------------------------

func (s *Server) stageGuardrailInput(ctx context.Context, rc *RequestContext) error {
	if rc.Chat == nil {
		return nil
	}
	if !rc.Pool.Guardrail.Enabled {
		// Recorded rather than silent: "this pool has no content safety
		// policy" is a fact an auditor asks about, and it should be visible on
		// the trace of every request it applies to.
		rc.Span.SetAttributes(telemetry.Attr("agentgate.guardrail.bypassed", true))
		rc.GuardrailHeader = "disabled"
		return nil
	}
	ctx, span := s.tracer.Start(ctx, telemetry.SpanGatewayGuardra,
		telemetry.WithSpanKind(telemetry.KindClient),
		telemetry.WithAttributes(telemetry.Attr("agentgate.guardrail.direction", "input")))
	defer span.End()

	req := guardrails.Request{
		Text: rc.Chat.PromptText(), Direction: guardrails.DirectionInput,
		Pool: rc.Pool.Name, Tenant: rc.Claims.Tenant, AgentID: rc.Claims.AgentID,
		DataClassification: string(rc.Classification),
	}
	start := time.Now()
	raw, err := rc.Pool.GuardrailProvider.Inspect(ctx, req, rc.Pool.Guardrail)
	d := guardrails.Apply(rc.Pool.Guardrail, raw, err)
	s.metrics.GuardrailLat.Observe(time.Since(start).Seconds(), s.env, rc.Pool.Name)
	if err != nil {
		s.metrics.GuardrailErr.Inc(s.env, rc.Pool.Name, "callout_failed")
		span.RecordError(err)
	}
	if d.FailedOpen {
		s.metrics.GuardrailFail.Inc(s.env, rc.Pool.Name, string(rc.Pool.Guardrail.FailureMode))
	}
	rc.GuardrailInput = d
	for _, f := range d.Findings {
		s.metrics.Guardrail.Inc(s.env, rc.Pool.Name, f.Category, string(f.Action))
	}
	span.SetAttributes(
		telemetry.Attr(telemetry.AttrGuardrail, string(d.Action)),
		telemetry.Attr("agentgate.guardrail.findings", len(d.Findings)),
	)

	switch d.Action {
	case guardrails.ActionBlock:
		rc.GuardrailHeader = "blocked:" + d.Category()
		return httpx.Errorf(httpx.CodeGuardrailBlocked,
			"the request was blocked by content safety policy (%s)", d.Category())
	case guardrails.ActionRedact:
		rc.GuardrailHeader = "redacted:" + d.Category()
		if err := s.redactMessages(ctx, rc); err != nil {
			// A redaction that cannot be applied precisely must not be applied
			// approximately: the prompt would either lose content or keep the
			// sensitive value. Refusing is the only safe outcome.
			s.metrics.GuardrailErr.Inc(s.env, rc.Pool.Name, "redaction_failed")
			return httpx.Errorf(httpx.CodeGuardrailBlocked,
				"the request contained content requiring redaction that could not be applied safely (%s)", d.Category())
		}
	default:
		rc.GuardrailHeader = "pass"
	}
	return nil
}

// redactMessages re-scans each message individually and replaces only the ones
// the detector changed.
//
// The first scan runs over the flattened prompt because a violation can
// straddle a message boundary and because one call is cheap. Mapping the
// result back is not: the flattened rendering carries role prefixes and
// newlines, so an offset in it does not correspond to an offset in any
// message. Rewriting the last user turn from the flattened text — the obvious
// shortcut — silently drops every earlier line of a multi-line message and
// leaves the sensitive value in any other message that held it.
//
// So redaction, which is the uncommon branch, pays for a second per-message
// pass. Messages are scanned concurrently under the pool's guardrail timeout,
// because a ten-message prompt against a callout service would otherwise add
// ten round trips to the request.
func (s *Server) redactMessages(ctx context.Context, rc *RequestContext) error {
	type outcome struct {
		index int
		text  string
		err   error
	}
	results := make(chan outcome, len(rc.Chat.Messages))
	pending := 0

	for i := range rc.Chat.Messages {
		text := rc.Chat.Messages[i].Text()
		if text == "" {
			continue
		}
		pending++
		go func(i int, text string) {
			req := guardrails.Request{
				Text: text, Direction: guardrails.DirectionInput,
				Pool: rc.Pool.Name, Tenant: rc.Claims.Tenant, AgentID: rc.Claims.AgentID,
				DataClassification: string(rc.Classification),
			}
			raw, err := rc.Pool.GuardrailProvider.Inspect(ctx, req, rc.Pool.Guardrail)
			d := guardrails.Apply(rc.Pool.Guardrail, raw, err)
			switch {
			case d.Action == guardrails.ActionBlock:
				results <- outcome{index: i, err: fmt.Errorf("message %d is blocked", i)}
			case d.Action == guardrails.ActionRedact && d.Text != "":
				results <- outcome{index: i, text: d.Text}
			default:
				results <- outcome{index: i}
			}
		}(i, text)
	}

	var firstErr error
	replacements := map[int]string{}
	for ; pending > 0; pending-- {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		if r.text != "" {
			replacements[r.index] = r.text
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if len(replacements) == 0 {
		// The flattened scan found something the per-message scan cannot
		// place — a violation straddling a boundary. It is real, and it
		// cannot be redacted without changing message structure.
		return fmt.Errorf("redaction could not be attributed to a message")
	}
	for i, text := range replacements {
		rc.Chat.Messages[i].SetText(text)
	}
	rc.Span.SetAttributes(telemetry.Attr("agentgate.guardrail.redacted_messages", len(replacements)))
	return nil
}

// --- stage 8: cache lookup -----------------------------------------------

func (s *Server) stageCacheLookup(ctx context.Context, rc *RequestContext) error {
	if rc.Chat == nil || rc.Operation != OpChat {
		rc.CacheResult = cache.ResultBypass
		return nil
	}
	if rc.CacheMode == "off" {
		rc.CacheResult, rc.CacheReason = cache.ResultBypass, "disabled by request header"
		return nil
	}
	ok, reason := cache.Cacheable(rc.Pool.Cache, rc.Chat)
	if !ok {
		rc.CacheResult, rc.CacheReason = cache.ResultBypass, reason
		s.metrics.CacheLookups.Inc(s.env, rc.Pool.Name, string(cache.ResultBypass))
		rc.Span.SetAttributes(telemetry.Attr("agentgate.cache.bypass_reason", reason))
		return nil
	}
	if rc.CacheMode == "refresh" {
		rc.CacheResult = cache.ResultRefresh
		s.metrics.CacheLookups.Inc(s.env, rc.Pool.Name, string(cache.ResultRefresh))
		return nil
	}

	ctx, span := s.tracer.Start(ctx, telemetry.SpanGatewayCache)
	defer span.End()

	key := cache.Key(rc.Claims.Tenant, rc.Model.Name, rc.Chat)
	if entry, hit := s.cache.Get(ctx, key); hit {
		rc.CacheResult = cache.ResultHit
		rc.CacheEntry = entry
		// A copy, not the stored pointer: later stages rewrite the model name
		// and may redact the completion, and concurrent hits on one entry
		// would otherwise race and corrupt each other's responses.
		rc.Response = entry.Clone()
		if rc.Response != nil {
			rc.Usage = rc.Response.Usage
		}
		s.metrics.CacheLookups.Inc(s.env, rc.Pool.Name, string(cache.ResultHit))
		span.SetAttributes(
			telemetry.Attr(telemetry.AttrCache, "hit"),
			telemetry.Attr("agentgate.cache.origin_trace_id", entry.TraceID),
		)
		return nil
	}
	rc.CacheResult = cache.ResultMiss
	s.metrics.CacheLookups.Inc(s.env, rc.Pool.Name, string(cache.ResultMiss))
	return nil
}

// --- stage 9: request transformation -------------------------------------

func (s *Server) stageTransformRequest(_ context.Context, rc *RequestContext) error {
	if rc.Chat == nil {
		return nil
	}
	// The caller's logical model name never reaches a provider; each adapter
	// substitutes its own concrete model. Metadata is stripped here because it
	// is gateway context, not model input, and sending it would both cost
	// tokens and leak internal identifiers to a third party.
	rc.Chat.Metadata = nil
	if rc.Chat.User == "" && rc.Claims != nil {
		// A stable, non-identifying per-agent value helps providers with their
		// own abuse detection without exposing a person.
		sum := sha256.Sum256([]byte(rc.Claims.AgentID))
		rc.Chat.User = "agentgate-" + hex.EncodeToString(sum[:8])
	}
	return nil
}

// --- stage 10: routing ----------------------------------------------------

func (s *Server) stageRoute(_ context.Context, rc *RequestContext) error {
	if rc.Response != nil || rc.Operation == OpTokenCount {
		return nil // served from cache; nothing to route
	}
	if rc.Pool.Tier == resilience.PriorityBatch && rc.Priority == resilience.PriorityInteractive {
		// A pool declared as batch does not become interactive because a
		// caller asked nicely; the header can only ever de-prioritise.
		rc.Priority = resilience.PriorityBatch
	}
	rc.Span.SetAttributes(
		telemetry.Attr(telemetry.AttrPriority, string(rc.Priority)),
		telemetry.Attr("agentgate.route.classification", string(rc.Classification)),
	)
	return nil
}

// --- stage 11: invocation -------------------------------------------------

func (s *Server) stageInvoke(ctx context.Context, rc *RequestContext) error {
	if rc.Response != nil || rc.Operation == OpTokenCount {
		return nil
	}
	return s.invoke(ctx, rc)
}

// --- stage 12: response transformation ------------------------------------

func (s *Server) stageTransformResponse(_ context.Context, rc *RequestContext) error {
	if rc.Response == nil {
		return nil
	}
	// The caller sees the logical model it asked for, not the backend that
	// happened to serve it. The concrete model is in a response header and on
	// the span, where an operator needs it and an application does not.
	rc.Response.Model = rc.Model.Name
	if rc.Response.Object == "" {
		rc.Response.Object = "chat.completion"
	}
	if rc.Response.Usage != nil {
		rc.Usage = rc.Response.Usage
	}
	return nil
}

// --- stage 13: output guardrail -------------------------------------------

func (s *Server) stageGuardrailOutput(ctx context.Context, rc *RequestContext) error {
	if rc.Response == nil || !rc.Pool.Guardrail.Enabled || !rc.Pool.ScanOutput {
		return nil
	}
	if rc.CacheResult == cache.ResultHit {
		// Cached content was scanned before it was stored; rescanning would
		// double the cost of the cheapest path on the platform.
		return nil
	}
	req := guardrails.Request{
		Text: rc.Response.FirstText(), Direction: guardrails.DirectionOutput,
		Pool: rc.Pool.Name, Tenant: rc.Claims.Tenant, AgentID: rc.Claims.AgentID,
		DataClassification: string(rc.Classification),
	}
	start := time.Now()
	raw, err := rc.Pool.GuardrailProvider.Inspect(ctx, req, rc.Pool.Guardrail)
	d := guardrails.Apply(rc.Pool.Guardrail, raw, err)
	s.metrics.GuardrailLat.Observe(time.Since(start).Seconds(), s.env, rc.Pool.Name)
	rc.GuardrailOutput = d
	for _, f := range d.Findings {
		s.metrics.Guardrail.Inc(s.env, rc.Pool.Name, f.Category, string(f.Action))
	}
	switch d.Action {
	case guardrails.ActionBlock:
		rc.GuardrailHeader = "blocked:" + d.Category()
		return httpx.Errorf(httpx.CodeGuardrailBlocked,
			"the response was blocked by content safety policy (%s)", d.Category())
	case guardrails.ActionRedact:
		rc.GuardrailHeader = "redacted:" + d.Category()
		if len(rc.Response.Choices) > 0 && rc.Response.Choices[0].Message != nil {
			rc.Response.Choices[0].Message.SetText(d.Text)
		}
	}
	return nil
}

// --- stage 14: cache store ------------------------------------------------

func (s *Server) stageCacheStore(ctx context.Context, rc *RequestContext) error {
	if rc.Response == nil || rc.Chat == nil {
		return nil
	}
	if rc.CacheResult != cache.ResultMiss && rc.CacheResult != cache.ResultRefresh {
		return nil
	}
	if rc.GuardrailOutput.Action == guardrails.ActionBlock || rc.GuardrailOutput.FailedOpen {
		// Never cache a response whose safety verdict is unknown: a fail-open
		// answer stored once would be served confidently for its whole TTL.
		return nil
	}
	key := cache.Key(rc.Claims.Tenant, rc.Model.Name, rc.Chat)
	entry := &cache.Entry{
		Response: rc.Response, Backend: rc.BackendName(),
		Model: rc.Model.Name, TraceID: rc.TraceID, CostUSD: rc.CostUSD,
		CreatedAt: time.Now(), Text: rc.Chat.LastUserText(),
	}
	s.cache.SetWithScope(ctx, key, rc.Claims.Tenant, rc.Pool.Name, entry, rc.Pool.Cache.TTL)
	return nil
}

// quotaLimitsFor is used by the token-count endpoint to report headroom.
func (s *Server) quotaLimitsFor(ctx context.Context, claims *identity.Claims) ratelimit.Limits {
	return s.quotas.Limits(ctx, claims)
}
