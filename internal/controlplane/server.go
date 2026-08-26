// Package controlplane implements the trust plane: agent registration,
// workload identity issuance and token exchange, the promotion gate, and the
// credential lifecycle.
//
// It is deliberately off the request path. The gateway verifies tokens with a
// cached key set and enforces on signed claims, so a control-plane outage
// stops new agents from being issued identities but does not stop the agents
// already running from working. That separation is the single most important
// availability property of the platform.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/httpx"
	"github.com/agentgate/agentgate/internal/identity"
	"github.com/agentgate/agentgate/internal/registry"
	"github.com/agentgate/agentgate/internal/telemetry"
	"github.com/agentgate/agentgate/internal/version"
)

// Options configures the control-plane server.
type Options struct {
	Env     string
	Service *registry.Service
	Store   registry.Store
	Issuer  *identity.Issuer
	Logger  *slog.Logger
	Metrics *telemetry.Instruments
	Tracer  *telemetry.Tracer
	// SubjectVerifiers verify platform-issued subject tokens during token
	// exchange, keyed by issuer. One entry per trusted runtime: the Kubernetes
	// API server, the cloud's managed-identity issuer, the SPIFFE trust
	// domain.
	SubjectVerifiers map[string]*identity.Verifier
	// AdminVerifier authenticates human and CI callers to the management API.
	AdminVerifier *identity.Verifier
	// AllowAnonymousAdmin is for local development only.
	AllowAnonymousAdmin bool
	TokenTTL            time.Duration
}

// Server is the control-plane HTTP server.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New builds the control-plane server.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.TokenTTL <= 0 {
		o.TokenTTL = 15 * time.Minute
	}
	s := &Server{opts: o}
	s.mux = s.routes()
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// OIDC discovery and key publication. The gateway reads these; publishing
	// standard documents means an enterprise identity provider or an API
	// management layer can also validate AgentGate tokens without bespoke
	// integration.
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)

	mux.HandleFunc("POST /oauth2/token", s.handleToken)

	mux.HandleFunc("POST /api/v1/agents", s.handleRegister)
	mux.HandleFunc("GET /api/v1/agents", s.handleListAgents)
	mux.HandleFunc("GET /api/v1/agents/{id}", s.handleGetAgent)
	mux.HandleFunc("POST /api/v1/agents/{id}/credentials", s.handleIssueCredential)
	mux.HandleFunc("POST /api/v1/agents/{id}/credentials/{clientID}/rotate", s.handleRotateCredential)
	mux.HandleFunc("POST /api/v1/agents/{id}/quarantine", s.handleQuarantine)

	mux.HandleFunc("POST /api/v1/promotions", s.handleRequestPromotion)
	mux.HandleFunc("GET /api/v1/promotions", s.handleListPromotions)
	mux.HandleFunc("GET /api/v1/promotions/{id}", s.handleGetPromotion)
	mux.HandleFunc("POST /api/v1/promotions/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /api/v1/waivers", s.handleWaiver)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version.Get()})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	return mux
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]any{}
	_, err := s.opts.Store.ListAgents(r.Context(), registry.Filter{})
	checks["registry"] = err == nil
	checks["signing_key"] = s.opts.Issuer.ActiveKeyID() != ""
	status := http.StatusOK
	if err != nil || s.opts.Issuer.ActiveKeyID() == "" {
		status = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, status, map[string]any{"status": statusWord(status), "checks": checks})
}

func statusWord(code int) string {
	if code == http.StatusOK {
		return "ready"
	}
	return "not_ready"
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	base := externalBase(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"jwks_uri":                              base + "/.well-known/jwks.json",
		"token_endpoint":                        base + "/oauth2/token",
		"grant_types_supported":                 []string{"client_credentials", "urn:ietf:params:oauth:grant-type:token-exchange"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "none"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported": []string{
			identity.ScopeInvoke, identity.ScopeEmbed, identity.ScopeRegister,
			identity.ScopePromote, identity.ScopeApprove, identity.ScopeReadFleet,
		},
	})
}

func externalBase(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		if f := r.Header.Get("X-Forwarded-Proto"); f != "" {
			scheme = f
		} else {
			scheme = "http"
		}
	}
	host := r.Host
	if f := r.Header.Get("X-Forwarded-Host"); f != "" {
		host = f
	}
	return scheme + "://" + host
}

func (s *Server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	// A short cache is deliberate: long enough to keep the gateway from
	// hammering the endpoint, short enough that a key rotation propagates in
	// minutes rather than hours.
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.WriteJSON(w, http.StatusOK, s.opts.Issuer.JWKS())
}

// --- token endpoint -------------------------------------------------------

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	IssuedAt    int64  `json:"issued_at"`
	AgentID     string `json:"agent_id"`
	Env         string `json:"env"`
	Version     string `json:"agent_version"`
}

// handleToken implements both supported grants.
//
// The token-exchange grant (RFC 8693) is the preferred path: the agent's
// runtime already proves who it is, and the control plane converts that proof
// into an AgentGate identity without a shared secret ever existing. The
// client-credentials grant exists for runtimes that cannot do that, and every
// token it issues is marked with a weaker attestation that the production
// promotion gate refuses.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := s.trace(r.Context(), telemetry.SpanCPTokenExchange)
	defer span.End()
	start := time.Now()

	if err := r.ParseForm(); err != nil {
		s.tokenError(w, span, "invalid_request", "the request body could not be parsed", http.StatusBadRequest, "")
		return
	}
	grant := r.PostFormValue("grant_type")
	env := identity.Environment(defaultStr(r.PostFormValue("env"), string(identity.EnvDev)))
	requestedVersion := r.PostFormValue("agent_version")

	var (
		agentID     string
		attestation identity.Attestation
		err         error
	)

	switch grant {
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		subjectToken := r.PostFormValue("subject_token")
		agentIdentity := r.PostFormValue("agent_identity")
		if subjectToken == "" || agentIdentity == "" {
			s.tokenError(w, span, "invalid_request", "subject_token and agent_identity are required", http.StatusBadRequest, grant)
			return
		}
		claims, verr := s.verifySubject(ctx, subjectToken)
		if verr != nil {
			s.opts.Logger.Info("subject token rejected", "error", verr)
			s.tokenError(w, span, "invalid_grant", "the subject token could not be verified", http.StatusBadRequest, grant)
			return
		}
		agent, gerr := s.opts.Store.GetAgentByIdentity(ctx, agentIdentity)
		if gerr != nil {
			s.tokenError(w, span, "invalid_grant", "the agent is not registered", http.StatusBadRequest, grant)
			return
		}
		// The workload's own subject must correspond to the agent it claims to
		// be. Without this check any workload in the cluster could obtain any
		// agent's identity, which would make the whole model decorative.
		if !subjectMatchesAgent(claims.Subject, agent) {
			s.opts.Logger.Warn("subject token does not correspond to the requested agent",
				"subject", claims.Subject, "agent", agentIdentity)
			s.tokenError(w, span, "invalid_grant", "the subject token does not correspond to this agent", http.StatusForbidden, grant)
			return
		}
		agentID, attestation = agent.AgentID, identity.AttestWorkloadIdentity

	case "client_credentials":
		clientID, clientSecret := r.PostFormValue("client_id"), r.PostFormValue("client_secret")
		if clientID == "" || clientSecret == "" {
			if u, p, ok := r.BasicAuth(); ok {
				clientID, clientSecret = u, p
			}
		}
		if clientID == "" || clientSecret == "" {
			s.tokenError(w, span, "invalid_request", "client_id and client_secret are required", http.StatusBadRequest, grant)
			return
		}
		cred, owner, cerr := s.opts.Store.GetCredential(ctx, clientID)
		if cerr != nil || !cred.Verify(clientSecret) {
			// One message for both failures: telling a caller that the client
			// exists but the secret is wrong is an enumeration oracle.
			s.tokenError(w, span, "invalid_client", "client authentication failed", http.StatusUnauthorized, grant)
			return
		}
		agentID, attestation = owner, identity.AttestClientSecret

	default:
		s.tokenError(w, span, "unsupported_grant_type",
			"supported grants are client_credentials and urn:ietf:params:oauth:grant-type:token-exchange",
			http.StatusBadRequest, grant)
		return
	}

	agent, err := s.opts.Store.GetAgent(ctx, agentID)
	if err != nil {
		s.tokenError(w, span, "invalid_grant", "the agent is not registered", http.StatusBadRequest, grant)
		return
	}
	agentVersion := requestedVersion
	if agentVersion == "" {
		if v, ok := agent.VersionIn(env, registry.StateActive); ok {
			agentVersion = v.Version
		}
	}
	if agentVersion == "" {
		s.tokenError(w, span, "invalid_grant",
			fmt.Sprintf("no active version of this agent is promoted for %s", env), http.StatusForbidden, grant)
		return
	}

	token, claims, err := s.opts.Service.MintToken(ctx, agentID, agentVersion, env, attestation, grant)
	if err != nil {
		code := "invalid_grant"
		status := http.StatusForbidden
		if !errors.Is(err, registry.ErrNotPromoted) {
			code, status = "server_error", http.StatusInternalServerError
		}
		s.tokenError(w, span, code, err.Error(), status, grant)
		return
	}

	s.metric(func(m *telemetry.Instruments) {
		m.TokenExchange.Inc(s.opts.Env, "success", grant)
		m.TokenExchangeLat.Observe(time.Since(start).Seconds(), s.opts.Env)
	})
	span.SetAttributes(
		telemetry.Attr(telemetry.AttrAgentID, agentID),
		telemetry.Attr("agentgate.grant_type", grant),
		telemetry.Attr("agentgate.attestation", string(attestation)),
	)
	// A token is a credential: it must never be cached by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpx.WriteJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token, TokenType: "Bearer",
		ExpiresIn: int(time.Until(claims.ExpiresAt.Time).Seconds()),
		Scope:     strings.Join(claims.Scopes, " "), IssuedAt: claims.IssuedAt.Unix(),
		AgentID: claims.AgentID, Env: string(claims.Env), Version: claims.AgentVersion,
	})
}

// subjectMatchesAgent checks that a platform-issued workload identity
// corresponds to the agent being requested.
//
// The mapping is intentionally simple and explicit: the agent name must appear
// as a component of the workload's subject. Real deployments bind this to the
// exact service-account or managed-identity object id recorded at
// registration; that binding is the field to tighten first when the client's
// identity model is settled.
func subjectMatchesAgent(subject string, agent *registry.Agent) bool {
	if subject == "" {
		return false
	}
	if binding, ok := agent.Labels["workload_subject"]; ok && binding != "" {
		return binding == subject
	}
	return strings.Contains(subject, agent.Identity.Name)
}

func (s *Server) verifySubject(ctx context.Context, token string) (*identity.Claims, error) {
	if len(s.opts.SubjectVerifiers) == 0 {
		return nil, errors.New("no trusted subject token issuers are configured")
	}
	iss, err := unverifiedIssuer(token)
	if err != nil {
		return nil, err
	}
	v, ok := s.opts.SubjectVerifiers[iss]
	if !ok {
		return nil, fmt.Errorf("issuer %q is not trusted for token exchange", iss)
	}
	return v.Verify(ctx, token)
}

// unverifiedIssuer reads the issuer claim before verification, purely to pick
// which trusted verifier to use. Nothing is trusted on the basis of it.
func unverifiedIssuer(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("subject token is not a JWT")
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.Iss == "" {
		return "", errors.New("subject token has no issuer claim")
	}
	return claims.Iss, nil
}

func (s *Server) tokenError(w http.ResponseWriter, span *telemetry.Span, code, desc string, status int, grant string) {
	s.metric(func(m *telemetry.Instruments) { m.TokenExchange.Inc(s.opts.Env, "failure", grant) })
	span.SetStatus(telemetry.StatusError, code)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (s *Server) metric(fn func(*telemetry.Instruments)) {
	if s.opts.Metrics != nil {
		fn(s.opts.Metrics)
	}
}

func (s *Server) trace(ctx context.Context, name string) (context.Context, *telemetry.Span) {
	if s.opts.Tracer == nil {
		return ctx, nil
	}
	return s.opts.Tracer.Start(ctx, name, telemetry.WithSpanKind(telemetry.KindServer))
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
