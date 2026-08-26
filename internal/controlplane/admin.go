package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/google/uuid"
)

func base64URLDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// caller is the authenticated principal behind a management API call.
type caller struct {
	Subject string
	Scopes  []string
	Actor   string
}

func (c caller) has(scope string) bool {
	for _, s := range c.Scopes {
		if s == scope || s == identity.ScopeAdmin {
			return true
		}
	}
	return false
}

// authenticate identifies the caller of a management endpoint.
//
// Registration is called by CI, promotion by an engineer or a pipeline, and
// approval by a human. All three are authenticated the same way and
// distinguished by scope, so the audit record always names a principal rather
// than "the system".
func (s *Server) authenticate(r *http.Request, scope string) (caller, *httpx.Problem) {
	if s.opts.AllowAnonymousAdmin {
		actor := r.Header.Get("x-agentgate-actor")
		if actor == "" {
			actor = "anonymous@localdev"
		}
		return caller{Subject: actor, Actor: actor, Scopes: []string{identity.ScopeAdmin}}, nil
	}
	raw, ok := identity.BearerToken(r.Header)
	if !ok || s.opts.AdminVerifier == nil {
		return caller{}, httpx.NewProblem(httpx.CodeUnauthenticated, "a bearer token is required")
	}
	claims, err := s.opts.AdminVerifier.Verify(r.Context(), raw)
	if err != nil {
		return caller{}, httpx.NewProblem(httpx.CodeUnauthenticated, "the presented token could not be verified")
	}
	c := caller{Subject: claims.Subject, Scopes: claims.Scopes, Actor: claims.Subject}
	if claims.OwnerEmail != "" {
		c.Actor = claims.OwnerEmail
	}
	if !c.has(scope) {
		return caller{}, httpx.Errorf(httpx.CodeForbiddenPool, "this token does not carry the %s scope", scope)
	}
	return c, nil
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return httpx.Errorf(httpx.CodeInvalidRequest, "request body is invalid: %v", err)
	}
	return nil
}

func writeErr(w http.ResponseWriter, err error) {
	var p *httpx.Problem
	if errors.As(err, &p) {
		httpx.WriteProblem(w, p, "", "")
		return
	}
	switch {
	case errors.Is(err, registry.ErrNotFound):
		httpx.WriteProblem(w, httpx.NewProblem(httpx.CodeNotFound, err.Error()), "", "")
	case errors.Is(err, registry.ErrConflict):
		httpx.WriteProblem(w, httpx.NewProblem(httpx.CodeConflict, err.Error()), "", "")
	case errors.Is(err, registry.ErrNotPromoted):
		httpx.WriteProblem(w, httpx.NewProblem(httpx.CodeAgentNotPromoted, err.Error()), "", "")
	default:
		httpx.WriteProblem(w, httpx.Errorf(httpx.CodeInvalidRequest, "%v", err), "", "")
	}
}

// --- registration ---------------------------------------------------------

type registerRequest struct {
	registry.RegistrationRequest
	// IssueCredential asks for a client secret. Omitted by pipelines that use
	// federated workload identity, which is every pipeline that can.
	IssueCredential bool `json:"issue_credential,omitempty"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	c, problem := s.authenticate(r, identity.ScopeRegister)
	if problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	ctx, span := s.trace(r.Context(), telemetry.SpanCPRegister)
	defer span.End()

	var req registerRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.opts.Service.Register(ctx, req.RegistrationRequest, c.Actor, req.IssueCredential)
	if err != nil {
		writeErr(w, err)
		return
	}
	span.SetAttributes(
		telemetry.Attr(telemetry.AttrAgentID, res.Agent.AgentID),
		telemetry.Attr(telemetry.AttrAgentIdentity, res.Agent.IdentityS),
		telemetry.Attr(telemetry.AttrDeploymentEnv, res.Env),
	)
	s.opts.Logger.Info("agent registered",
		"agent", res.Agent.IdentityS, "version", res.Version, "env", res.Env,
		"actor", c.Actor, "warnings", len(res.Warnings))
	if res.ClientSecret != "" {
		// The secret is returned exactly once and never persisted in a form
		// that can be read back.
		w.Header().Set("Cache-Control", "no-store")
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	if _, problem := s.authenticate(r, identity.ScopeReadFleet); problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	agents, err := s.opts.Store.ListAgents(r.Context(), registry.Filter{
		Tenant: r.URL.Query().Get("tenant"),
		Team:   r.URL.Query().Get("team"),
		Env:    identity.Environment(r.URL.Query().Get("env")),
		Search: r.URL.Query().Get("q"),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"agents": agents, "count": len(agents)})
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	if _, problem := s.authenticate(r, identity.ScopeReadFleet); problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	agent, err := s.opts.Store.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, agent)
}

func (s *Server) handleIssueCredential(w http.ResponseWriter, r *http.Request) {
	c, problem := s.authenticate(r, identity.ScopeRegister)
	if problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	agent, err := s.opts.Store.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	cred, secret, err := s.opts.Service.IssueCredential(r.Context(), agent)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.opts.Logger.Warn("client secret issued; federated workload identity is preferred",
		"agent", agent.IdentityS, "client_id", cred.ClientID, "actor", c.Actor)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"client_id": cred.ClientID, "client_secret": secret,
		"expires_at": cred.ExpiresAt,
		"warning":    "this secret is shown once; prefer token exchange with federated workload identity",
	})
}

func (s *Server) handleRotateCredential(w http.ResponseWriter, r *http.Request) {
	c, problem := s.authenticate(r, identity.ScopeRegister)
	if problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	overlap := 24 * time.Hour
	if v := r.URL.Query().Get("overlap"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			overlap = d
		}
	}
	cred, secret, err := s.opts.Service.RotateCredential(r.Context(),
		r.PathValue("id"), r.PathValue("clientID"), overlap)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.opts.Logger.Info("client secret rotated",
		"agent_id", r.PathValue("id"), "old_client_id", r.PathValue("clientID"),
		"new_client_id", cred.ClientID, "overlap", overlap, "actor", c.Actor)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"client_id": cred.ClientID, "client_secret": secret,
		"previous_valid_until": time.Now().Add(overlap),
	})
}

func (s *Server) handleQuarantine(w http.ResponseWriter, r *http.Request) {
	c, problem := s.authenticate(r, identity.ScopeAdmin)
	if problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	var req struct {
		Version string               `json:"version"`
		Env     identity.Environment `json:"env"`
		Reason  string               `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Reason == "" {
		writeErr(w, httpx.NewProblem(httpx.CodeInvalidRequest, "reason is required; a quarantine without a recorded reason cannot be reviewed"))
		return
	}
	if err := s.opts.Service.Quarantine(r.Context(), r.PathValue("id"), req.Version, req.Env, req.Reason, c.Actor); err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"quarantined": true, "actor": c.Actor})
}

// --- promotion ------------------------------------------------------------

func (s *Server) handleRequestPromotion(w http.ResponseWriter, r *http.Request) {
	c, problem := s.authenticate(r, identity.ScopePromote)
	if problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	ctx, span := s.trace(r.Context(), telemetry.SpanCPPromote)
	defer span.End()

	var req struct {
		AgentID string               `json:"agent_id"`
		Version string               `json:"version"`
		To      identity.Environment `json:"to"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	promotion, err := s.opts.Service.RequestPromotion(ctx, req.AgentID, req.Version, req.To, c.Actor)
	if err != nil {
		writeErr(w, err)
		return
	}
	span.SetAttributes(
		telemetry.Attr(telemetry.AttrAgentID, req.AgentID),
		telemetry.Attr(telemetry.AttrPromotedState, string(promotion.State)),
	)
	if promotion.State == registry.PromotionBlocked {
		// The gate refusing is a normal, expected outcome with a body the
		// caller can act on, not a server error. The response carries both the
		// stable error code, so a pipeline can branch on it, and the full gate
		// evaluation, so an engineer can see exactly what to fix.
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"code":         httpx.CodePromotionGateBlocked,
			"title":        "Promotion gate blocked",
			"detail":       "the automated gate did not pass; see failed_gates and promotion.gate.checks",
			"failed_gates": promotion.Gate.Failed(),
			"promotion":    promotion,
		})
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, promotion)
}

func (s *Server) handleListPromotions(w http.ResponseWriter, r *http.Request) {
	if _, problem := s.authenticate(r, identity.ScopeReadFleet); problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	list, err := s.opts.Store.ListPromotions(r.Context(),
		r.URL.Query().Get("agent_id"), registry.PromotionState(r.URL.Query().Get("state")))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"promotions": list, "count": len(list)})
}

func (s *Server) handleGetPromotion(w http.ResponseWriter, r *http.Request) {
	if _, problem := s.authenticate(r, identity.ScopeReadFleet); problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	p, err := s.opts.Store.GetPromotion(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	c, problem := s.authenticate(r, identity.ScopeApprove)
	if problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	var req struct {
		Role     registry.ApproverRole `json:"role"`
		Approved bool                  `json:"approved"`
		Comment  string                `json:"comment,omitempty"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Role != registry.RoleOwner && req.Role != registry.RolePlatform {
		writeErr(w, httpx.NewProblem(httpx.CodeInvalidRequest, "role must be owning_team or platform"))
		return
	}
	promotion, err := s.opts.Service.Approve(r.Context(), r.PathValue("id"), registry.Approval{
		Actor: c.Actor, Role: req.Role, Approved: req.Approved, Comment: req.Comment,
	})
	if err != nil {
		// The two-party rules produce errors that are decisions, not faults;
		// the caller gets the promotion record alongside the reason.
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"error": err.Error(), "promotion": promotion,
		})
		return
	}
	s.opts.Logger.Info("promotion decision recorded",
		"promotion_id", promotion.ID, "actor", c.Actor, "role", req.Role,
		"approved", req.Approved, "state", promotion.State)
	httpx.WriteJSON(w, http.StatusOK, promotion)
}

func (s *Server) handleWaiver(w http.ResponseWriter, r *http.Request) {
	c, problem := s.authenticate(r, identity.ScopeAdmin)
	if problem != nil {
		httpx.WriteProblem(w, problem, "", "")
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		Gate    string `json:"gate"`
		Reason  string `json:"reason"`
		Days    int    `json:"days"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Reason == "" || req.AgentID == "" || req.Gate == "" {
		writeErr(w, httpx.NewProblem(httpx.CodeInvalidRequest, "agent_id, gate and reason are required"))
		return
	}
	if req.Days <= 0 || req.Days > 90 {
		// An exception without an expiry is a policy change made quietly.
		req.Days = 30
	}
	waiver := registry.Waiver{
		ID: "wvr_" + uuid.NewString(), AgentID: req.AgentID, Gate: req.Gate,
		Reason: req.Reason, GrantedBy: c.Actor, GrantedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().AddDate(0, 0, req.Days),
	}
	if err := s.opts.Store.PutWaiver(r.Context(), waiver); err != nil {
		writeErr(w, err)
		return
	}
	s.opts.Logger.Warn("promotion gate waiver granted",
		"agent_id", req.AgentID, "gate", req.Gate, "actor", c.Actor, "expires", waiver.ExpiresAt)
	httpx.WriteJSON(w, http.StatusCreated, waiver)
}

// RemoteSignals reads promotion signals from the fleet service over HTTP.
type RemoteSignals struct {
	BaseURL string
	Client  *http.Client
}

// Signals implements registry.SignalSource against the observability plane.
func (r RemoteSignals) Signals(ctx context.Context, agentID string, env identity.Environment) (registry.AgentSignals, error) {
	if r.Client == nil {
		r.Client = &http.Client{Timeout: 10 * time.Second}
	}
	url := r.BaseURL + "/api/v1/signals/" + agentID + "?env=" + string(env)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return registry.AgentSignals{}, err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		// A promotion gate that cannot read its signals must refuse, not
		// assume. Returning the error blocks the promotion with a clear cause.
		return registry.AgentSignals{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return registry.AgentSignals{}, errors.New("fleet service returned " + resp.Status)
	}
	var out registry.AgentSignals
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.AgentSignals{}, err
	}
	return out, nil
}
