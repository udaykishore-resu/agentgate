package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
	"github.com/google/uuid"
)

// ChangeSink receives a change record for a production promotion. The
// ServiceNow implementation POSTs it; the logging implementation prints it for
// the documented manual path. Either way the same payload is the evidence.
type ChangeSink interface {
	Submit(ctx context.Context, payload map[string]any) (ref string, err error)
}

// LogChangeSink is the fallback when no workflow integration is configured. It
// is not a degraded mode to be embarrassed about: a promotion still cannot
// happen without two recorded approvals, and the change payload is still
// produced verbatim for a human to file.
type LogChangeSink struct{ Logger *slog.Logger }

// Submit logs the change payload and returns a manual reference.
func (l LogChangeSink) Submit(_ context.Context, payload map[string]any) (string, error) {
	ref := "MANUAL-" + uuid.NewString()[:8]
	l.Logger.Info("change record requires manual filing", "ref", ref, "payload", payload)
	return ref, nil
}

// ServiceOptions configures the registration service.
type ServiceOptions struct {
	Store            Store
	Issuer           *identity.Issuer
	Secrets          identity.SecretStore
	Signals          SignalSource
	Change           ChangeSink
	Gate             GateConfig
	ApprovalWindow   time.Duration
	SecretLifetime   time.Duration
	DefaultTokenTTL  time.Duration
	Logger           *slog.Logger
	OnGateEvaluation func(from, to identity.Environment, gate, result string)
	Now              func() time.Time
}

// Service implements registration, credential issuance, token exchange and the
// promotion gate.
type Service struct {
	opts ServiceOptions
}

// NewService builds the service.
func NewService(o ServiceOptions) *Service {
	if o.ApprovalWindow <= 0 {
		o.ApprovalWindow = 72 * time.Hour
	}
	if o.SecretLifetime <= 0 {
		o.SecretLifetime = 90 * 24 * time.Hour
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.OnGateEvaluation == nil {
		o.OnGateEvaluation = func(identity.Environment, identity.Environment, string, string) {}
	}
	if (o.Gate == GateConfig{}) {
		o.Gate = DefaultGateConfig()
	}
	return &Service{opts: o}
}

// RegistrationResult is returned to CI after a successful registration.
type RegistrationResult struct {
	Agent        *Agent `json:"agent"`
	Version      string `json:"version"`
	Env          string `json:"env"`
	State        State  `json:"state"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// Warnings surface problems that do not block registration but will block
	// promotion, so a team learns about them one deployment early instead of
	// on the day they try to ship to production.
	Warnings []string `json:"warnings,omitempty"`
}

// Register records an agent version on deployment. It is idempotent on
// (identity, env, version): re-running a pipeline must not create a second
// agent or a second credential.
func (s *Service) Register(ctx context.Context, req RegistrationRequest, actor string, wantCredential bool) (*RegistrationResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	id, _ := identity.ParseAgentIdentity(req.Identity)
	now := s.opts.Now()

	agent, err := s.opts.Store.GetAgentByIdentity(ctx, req.Identity)
	switch {
	case err == nil:
		// Ownership and classification are mutable, but a change of owning
		// team is a re-registration, not an update: chargeback history would
		// otherwise silently move between cost centres.
		if agent.Owner.Team != req.Owner.Team {
			return nil, fmt.Errorf("%w: agent %s is owned by %s; transferring ownership requires a platform action",
				ErrConflict, req.Identity, agent.Owner.Team)
		}
		agent.Owner = req.Owner
		agent.DisplayName = req.DisplayName
		agent.Description = req.Description
		agent.Runtime = req.Runtime
		agent.Framework = req.Framework
		agent.DataClassification = req.DataClassification
		agent.RequestedPools = req.RequestedPools
		agent.Quota = req.Quota
		if req.SecurityReviewRef != "" {
			agent.SecurityReviewRef = req.SecurityReviewRef
		}
		if req.Labels != nil {
			agent.Labels = req.Labels
		}
	case errors.Is(err, ErrNotFound):
		agent = &Agent{
			AgentID:            "agt_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20],
			Identity:           id,
			IdentityS:          id.String(),
			DisplayName:        req.DisplayName,
			Description:        req.Description,
			Owner:              req.Owner,
			Runtime:            req.Runtime,
			Framework:          req.Framework,
			DataClassification: req.DataClassification,
			RequestedPools:     req.RequestedPools,
			Quota:              req.Quota,
			SecurityReviewRef:  req.SecurityReviewRef,
			CreatedAt:          now,
			Labels:             req.Labels,
		}
	default:
		return nil, err
	}

	state := StateRegistered
	if req.Env == identity.EnvDev {
		// Dev needs no gate: an engineer must be able to run their agent
		// against the gateway the same day they write it, or they will build
		// something that bypasses it.
		state = StateActive
	}
	existing, found := agent.Find(req.Env, req.Version)
	v := Version{
		Version:      req.Version,
		Env:          req.Env,
		State:        state,
		Image:        req.Image,
		CommitSHA:    req.CommitSHA,
		RegisteredAt: now,
		RegisteredBy: actor,
	}
	if found {
		v.State = existing.State
		v.RegisteredAt = existing.RegisteredAt
		v.PromotedAt, v.PromotedBy, v.LastGate = existing.PromotedAt, existing.PromotedBy, existing.LastGate
	}
	agent.Upsert(v)
	if err := s.opts.Store.PutAgent(ctx, agent); err != nil {
		return nil, err
	}

	res := &RegistrationResult{Agent: agent, Version: req.Version, Env: string(req.Env), State: v.State}
	if len(agent.GrantedPools) == 0 && req.Env == identity.EnvProd {
		res.Warnings = append(res.Warnings, "no pools have been granted by the platform team yet; production traffic will be refused with forbidden_pool")
	}
	if agent.SecurityReviewRef == "" {
		res.Warnings = append(res.Warnings, "no security review reference is linked; the production promotion gate will block")
	}

	if wantCredential {
		cred, secret, err := s.IssueCredential(ctx, agent)
		if err != nil {
			return nil, err
		}
		res.ClientID, res.ClientSecret = cred.ClientID, secret
		res.Warnings = append(res.Warnings,
			"a client secret was issued; prefer federated workload identity, which the production promotion gate requires")
	}
	return res, nil
}

// IssueCredential mints a client credential, storing only its hash in the
// registry and the secret itself in the vault. The plaintext is returned once.
func (s *Service) IssueCredential(ctx context.Context, agent *Agent) (identity.ClientCredential, string, error) {
	clientID := "cli_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	cred, secret, err := identity.NewClientSecret(clientID, s.opts.SecretLifetime)
	if err != nil {
		return identity.ClientCredential{}, "", err
	}
	if s.opts.Secrets != nil {
		if version, err := s.opts.Secrets.Put(ctx, cred.SecretName(), []byte(secret)); err == nil {
			cred.Version = version
		} else {
			return identity.ClientCredential{}, "", fmt.Errorf("store secret: %w", err)
		}
	}
	if err := s.opts.Store.PutCredential(ctx, agent.AgentID, cred); err != nil {
		return identity.ClientCredential{}, "", err
	}
	return cred, secret, nil
}

// RotateCredential issues a new secret while leaving the previous one valid
// for the overlap window, so a running agent picks up the new secret on its
// next restart rather than at the instant of rotation.
func (s *Service) RotateCredential(ctx context.Context, agentID, clientID string, overlap time.Duration) (identity.ClientCredential, string, error) {
	old, owner, err := s.opts.Store.GetCredential(ctx, clientID)
	if err != nil {
		return identity.ClientCredential{}, "", err
	}
	if owner != agentID {
		return identity.ClientCredential{}, "", fmt.Errorf("%w: credential %s does not belong to %s", ErrConflict, clientID, agentID)
	}
	agent, err := s.opts.Store.GetAgent(ctx, agentID)
	if err != nil {
		return identity.ClientCredential{}, "", err
	}
	cred, secret, err := s.IssueCredential(ctx, agent)
	if err != nil {
		return identity.ClientCredential{}, "", err
	}
	old.ExpiresAt = s.opts.Now().Add(overlap)
	if err := s.opts.Store.PutCredential(ctx, agentID, old); err != nil {
		return identity.ClientCredential{}, "", err
	}
	return cred, secret, nil
}

// ErrNotPromoted is returned when a version is not allowed in an environment.
var ErrNotPromoted = errors.New("agent version is not promoted for this environment")

// MintToken issues an access token for a registered, promoted version.
func (s *Service) MintToken(ctx context.Context, agentID, version string, env identity.Environment, attestation identity.Attestation, grant string) (string, *identity.Claims, error) {
	agent, err := s.opts.Store.GetAgent(ctx, agentID)
	if err != nil {
		return "", nil, err
	}
	v, ok := agent.Find(env, version)
	if !ok || (v.State != StateActive && env != identity.EnvDev) {
		return "", nil, fmt.Errorf("%w: %s %s in %s", ErrNotPromoted, agent.IdentityS, version, env)
	}
	if v.State == StateQuarantined {
		return "", nil, fmt.Errorf("%w: version is quarantined", ErrNotPromoted)
	}
	pools := agent.PoolsGranted(env)
	scopes := []string{identity.ScopeInvoke, identity.ScopeEmbed}

	tok, claims, err := s.opts.Issuer.Mint(identity.TokenRequest{
		AgentID:            agent.AgentID,
		Identity:           agent.Identity,
		AgentVersion:       version,
		Env:                env,
		CostCenter:         agent.Owner.CostCenter,
		OwnerEmail:         agent.Owner.Email,
		Runtime:            agent.Runtime,
		Framework:          agent.Framework,
		Scopes:             scopes,
		ModelPools:         pools,
		Attestation:        attestation,
		DataClassification: string(agent.DataClassification),
		TTL:                s.opts.DefaultTokenTTL,
	}, grant)
	if err != nil {
		return "", nil, err
	}
	if err := s.opts.Store.RecordAttestation(ctx, agent.AgentID, env, attestation); err != nil {
		s.opts.Logger.Warn("could not record attestation", "agent_id", agent.AgentID, "error", err)
	}
	return tok, claims, nil
}

// RequestPromotion evaluates the gate and opens a promotion request.
func (s *Service) RequestPromotion(ctx context.Context, agentID, version string, to identity.Environment, requestedBy string) (*PromotionRequest, error) {
	from, ok := to.Previous()
	if !ok {
		return nil, fmt.Errorf("%s is not a promotion target", to)
	}
	agent, err := s.opts.Store.GetAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if _, ok := agent.Find(from, version); !ok {
		return nil, fmt.Errorf("%w: version %s has never been registered in %s", ErrNotFound, version, from)
	}
	waivers, err := s.opts.Store.ListWaivers(ctx, agentID)
	if err != nil {
		return nil, err
	}
	gate, err := Evaluate(ctx, s.opts.Gate, agent, version, from, to, s.opts.Signals, waivers, s.opts.Now())
	if err != nil {
		return nil, err
	}
	for _, c := range gate.Checks {
		s.opts.OnGateEvaluation(from, to, c.Name, string(c.Status))
	}

	req := NewPromotionRequest(agent, version, from, to, requestedBy, gate, s.opts.ApprovalWindow)

	// Record the gate snapshot against the version whether or not it passed:
	// a blocked promotion is exactly the evidence an engineer needs.
	if v, ok := agent.Find(from, version); ok {
		v.LastGate = &gate
		agent.Upsert(v)
	}
	// The target environment's version record carries the outcome, so the fleet
	// view shows an agent waiting for approval or blocked by a gate rather than
	// showing nothing at all until someone chases the promotion by hand.
	if !gate.Passed || RequiredApprovals(to) > 0 {
		src, _ := agent.Find(from, version)
		target, ok := agent.Find(to, version)
		if !ok {
			target = Version{
				Version: version, Env: to, Image: src.Image, CommitSHA: src.CommitSHA,
				RegisteredAt: s.opts.Now(), RegisteredBy: requestedBy,
			}
		}
		if gate.Passed {
			target.State = StatePendingPromotion
		} else {
			target.State = StateBlocked
			target.Notes = "blocked by promotion gate: " + strings.Join(gate.Failed(), ", ")
		}
		target.LastGate = &gate
		agent.Upsert(target)
	}

	if gate.Passed && RequiredApprovals(to) == 0 {
		if err := s.apply(ctx, agent, req); err != nil {
			return nil, err
		}
	} else if gate.Passed && s.opts.Change != nil {
		ref, err := s.opts.Change.Submit(ctx, req.ChangePayload(agent))
		if err != nil {
			s.opts.Logger.Warn("change record submission failed; promotion continues with manual evidence",
				"promotion_id", req.ID, "error", err)
		} else {
			req.ChangeRef = ref
		}
	}
	if err := s.opts.Store.PutAgent(ctx, agent); err != nil {
		return nil, err
	}
	if err := s.opts.Store.PutPromotion(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// Approve records a human decision and applies the promotion once the
// two-party rule is satisfied.
func (s *Service) Approve(ctx context.Context, promotionID string, a Approval) (*PromotionRequest, error) {
	req, err := s.opts.Store.GetPromotion(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	if err := req.Record(a, s.opts.Now()); err != nil {
		_ = s.opts.Store.PutPromotion(ctx, req)
		return req, err
	}
	agent, err := s.opts.Store.GetAgent(ctx, req.AgentID)
	if err != nil {
		return nil, err
	}
	if req.State == PromotionApproved {
		if err := s.apply(ctx, agent, req); err != nil {
			return nil, err
		}
		if err := s.opts.Store.PutAgent(ctx, agent); err != nil {
			return nil, err
		}
	}
	if err := s.opts.Store.PutPromotion(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// apply moves the version into the target environment and retires whatever was
// active there. Retiring the previous version rather than deleting it is what
// makes an emergency rollback a state change instead of a redeploy.
func (s *Service) apply(ctx context.Context, agent *Agent, req *PromotionRequest) error {
	now := s.opts.Now()
	if prev, ok := agent.VersionIn(req.To, StateActive); ok && prev.Version != req.Version {
		prev.State = StateRetired
		prev.RetiredAt = &now
		agent.Upsert(prev)
	}
	v, ok := agent.Find(req.To, req.Version)
	if !ok {
		src, _ := agent.Find(req.From, req.Version)
		v = Version{Version: req.Version, Env: req.To, Image: src.Image, CommitSHA: src.CommitSHA, RegisteredAt: now}
	}
	v.State = StateActive
	v.PromotedAt = &now
	v.PromotedBy = approverList(req)
	gate := req.Gate
	v.LastGate = &gate
	agent.Upsert(v)

	req.State = PromotionApplied
	req.AppliedAt = &now
	s.opts.Logger.Info("promotion applied",
		"agent", agent.IdentityS, "version", req.Version, "to", req.To,
		"promotion_id", req.ID, "change_ref", req.ChangeRef)
	_ = ctx
	return nil
}

func approverList(req *PromotionRequest) string {
	var names []string
	for _, a := range req.Approvals {
		if a.Approved {
			names = append(names, a.Actor)
		}
	}
	if len(names) == 0 {
		return "automatic"
	}
	return strings.Join(names, ",")
}

// Quarantine forcibly disables a version. This is the platform's emergency
// stop for a runaway agent and it takes effect at the next token refresh,
// which is why token lifetimes are short.
func (s *Service) Quarantine(ctx context.Context, agentID, version string, env identity.Environment, reason, actor string) error {
	agent, err := s.opts.Store.GetAgent(ctx, agentID)
	if err != nil {
		return err
	}
	v, ok := agent.Find(env, version)
	if !ok {
		return fmt.Errorf("%w: version %s in %s", ErrNotFound, version, env)
	}
	v.State = StateQuarantined
	v.Notes = fmt.Sprintf("quarantined by %s at %s: %s", actor, s.opts.Now().Format(time.RFC3339), reason)
	agent.Upsert(v)
	s.opts.Logger.Warn("agent version quarantined",
		"agent", agent.IdentityS, "version", version, "env", env, "actor", actor, "reason", reason)
	return s.opts.Store.PutAgent(ctx, agent)
}

// StaticSignals is a SignalSource for local development and tests, where no
// observability plane is running. It is refused in production by config
// validation: promoting against fabricated signals is worse than not gating.
type StaticSignals struct{ S AgentSignals }

// Signals returns the fixed signal set.
func (s StaticSignals) Signals(context.Context, string, identity.Environment) (AgentSignals, error) {
	return s.S, nil
}
