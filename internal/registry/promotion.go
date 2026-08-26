package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
	"github.com/google/uuid"
)

// Gate names. These strings appear in audit records, metrics and the CI
// output an engineer reads when a promotion is refused, so they are stable.
const (
	GateRegistrationComplete = "registration_complete"
	GateIdentityAttested     = "identity_attested"
	GateTelemetryHealthy     = "telemetry_healthy"
	GateErrorBudget          = "error_budget"
	GateGuardrailClean       = "guardrail_clean"
	GateQuotaDeclared        = "quota_declared"
	GateCostProjection       = "cost_projection"
	GateSecurityReview       = "security_review"
)

// CheckStatus is the outcome of one gate.
type CheckStatus string

// Gate outcomes.
const (
	CheckPass   CheckStatus = "pass"
	CheckFail   CheckStatus = "fail"
	CheckWaived CheckStatus = "waived"
	CheckSkip   CheckStatus = "skipped"
)

// GateCheck is one evaluated gate with the evidence behind it.
type GateCheck struct {
	Name     string      `json:"name"`
	Status   CheckStatus `json:"status"`
	Detail   string      `json:"detail"`
	Observed string      `json:"observed,omitempty"`
	Required string      `json:"required,omitempty"`
	WaiverID string      `json:"waiver_id,omitempty"`
}

// GateResult is the full evaluation snapshot for one promotion attempt. It is
// stored verbatim so that "why was this allowed into production" is answerable
// months later without re-running anything.
type GateResult struct {
	AgentID     string               `json:"agent_id"`
	Version     string               `json:"version"`
	From        identity.Environment `json:"from"`
	To          identity.Environment `json:"to"`
	Checks      []GateCheck          `json:"checks"`
	Passed      bool                 `json:"passed"`
	EvaluatedAt time.Time            `json:"evaluated_at"`
	Evaluator   string               `json:"evaluator"`
}

// Failed returns the names of gates that did not pass.
func (g GateResult) Failed() []string {
	var out []string
	for _, c := range g.Checks {
		if c.Status == CheckFail {
			out = append(out, c.Name)
		}
	}
	return out
}

// AgentSignals is the observed behaviour of one agent version in one
// environment, supplied by the observability plane. The registry deliberately
// does not compute these: the plane that owns whether telemetry is trustworthy
// is the plane that answers whether it is good enough to promote.
type AgentSignals struct {
	RequestCount            int64                `json:"request_count"`
	TelemetryCompleteness   float64              `json:"telemetry_completeness"` // 0..1
	SuccessRatio            float64              `json:"success_ratio"`          // 0..1 over the SLO window
	SuccessObjective        float64              `json:"success_objective"`
	CriticalGuardrailHits   int64                `json:"critical_guardrail_hits"`
	ObservedAttestation     identity.Attestation `json:"observed_attestation"`
	ProjectedMonthlyCostUSD float64              `json:"projected_monthly_cost_usd"`
	TeamMonthlyBudgetUSD    float64              `json:"team_monthly_budget_usd"`
	TeamTokenEnvelopePerMin int64                `json:"team_token_envelope_per_min"`
	SecurityReviewRef       string               `json:"security_review_ref,omitempty"`
	SecurityReviewExpiresAt *time.Time           `json:"security_review_expires_at,omitempty"`
	// WindowSeconds rather than a Go duration, so the JSON is readable by a
	// consumer that is not written in Go.
	WindowSeconds int64 `json:"window_seconds"`
}

// Window returns the observation window.
func (s AgentSignals) Window() time.Duration { return time.Duration(s.WindowSeconds) * time.Second }

// SignalSource supplies AgentSignals to the gate.
type SignalSource interface {
	Signals(ctx context.Context, agentID string, env identity.Environment) (AgentSignals, error)
}

// Waiver records an accepted exception to a gate, with an owner and an expiry
// so exceptions cannot quietly become permanent.
type Waiver struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Gate      string    `json:"gate"`
	Reason    string    `json:"reason"`
	GrantedBy string    `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Active reports whether the waiver is still in force.
func (w Waiver) Active(now time.Time) bool { return now.Before(w.ExpiresAt) }

// GateConfig holds the thresholds the gate enforces.
type GateConfig struct {
	MinRequests           int64
	MinTelemetryComplete  float64
	MinSuccessRatio       float64
	MaxCriticalGuardrails int64
	RequireAttestation    bool
	RequireSecurityReview bool
}

// DefaultGateConfig matches SPEC.md section 1.4.
func DefaultGateConfig() GateConfig {
	return GateConfig{
		MinRequests:           100,
		MinTelemetryComplete:  0.95,
		MaxCriticalGuardrails: 0,
		RequireAttestation:    true,
		RequireSecurityReview: true,
	}
}

// Evaluate runs every automated gate for a promotion from the version's
// current environment to the target environment.
//
// All gates run even after one fails. An engineer given one blocker at a time
// makes one fix at a time, and each round trip costs a deployment cycle.
func Evaluate(
	ctx context.Context,
	cfg GateConfig,
	agent *Agent,
	version string,
	from, to identity.Environment,
	signals SignalSource,
	waivers []Waiver,
	now time.Time,
) (GateResult, error) {
	res := GateResult{
		AgentID: agent.AgentID, Version: version, From: from, To: to,
		EvaluatedAt: now, Evaluator: "agentgate-controlplane",
	}

	sig, err := signals.Signals(ctx, agent.AgentID, from)
	if err != nil {
		return res, fmt.Errorf("gather signals: %w", err)
	}

	waiverFor := func(gate string) (Waiver, bool) {
		for _, w := range waivers {
			if w.Gate == gate && w.AgentID == agent.AgentID && w.Active(now) {
				return w, true
			}
		}
		return Waiver{}, false
	}
	add := func(name string, ok bool, detail, observed, required string) {
		st := CheckPass
		if !ok {
			st = CheckFail
			if w, has := waiverFor(name); has {
				st = CheckWaived
				res.Checks = append(res.Checks, GateCheck{
					Name: name, Status: st, Detail: "waived: " + w.Reason,
					Observed: observed, Required: required, WaiverID: w.ID,
				})
				return
			}
		}
		res.Checks = append(res.Checks, GateCheck{
			Name: name, Status: st, Detail: detail, Observed: observed, Required: required,
		})
	}

	// 1. Registration completeness.
	missing := missingOwnerFields(agent)
	add(GateRegistrationComplete, len(missing) == 0,
		fmt.Sprintf("owner, on-call, cost centre and data classification must be present; missing: %v", missing),
		fmt.Sprintf("%d missing", len(missing)), "0 missing")

	// 2. Identity attestation observed in the source environment.
	attested := !cfg.RequireAttestation || sig.ObservedAttestation == identity.AttestWorkloadIdentity
	if to != identity.EnvProd {
		attested = sig.ObservedAttestation != ""
	}
	add(GateIdentityAttested, attested,
		"the agent must have authenticated with federated workload identity in the source environment",
		string(sig.ObservedAttestation), string(identity.AttestWorkloadIdentity))

	// 3. Telemetry health. This is the gate that makes the observability plane
	// load-bearing rather than decorative.
	minComplete := cfg.MinTelemetryComplete
	telemetryOK := sig.RequestCount >= cfg.MinRequests && sig.TelemetryCompleteness >= minComplete
	add(GateTelemetryHealthy, telemetryOK,
		"traces must be complete and correctly attributed over the last 24 hours",
		fmt.Sprintf("%.3f completeness over %d requests", sig.TelemetryCompleteness, sig.RequestCount),
		fmt.Sprintf(">= %.2f completeness over >= %d requests", minComplete, cfg.MinRequests))

	// 4. Error budget in the source environment.
	objective := sig.SuccessObjective
	if objective == 0 {
		objective = cfg.MinSuccessRatio
	}
	budgetOK := objective == 0 || sig.SuccessRatio >= objective
	add(GateErrorBudget, budgetOK,
		"the agent's own success rate must meet its objective in the source environment",
		fmt.Sprintf("%.4f", sig.SuccessRatio), fmt.Sprintf(">= %.4f", objective))

	// 5. Guardrails.
	add(GateGuardrailClean, sig.CriticalGuardrailHits <= cfg.MaxCriticalGuardrails,
		"no unresolved critical guardrail violations in the last 7 days",
		fmt.Sprintf("%d", sig.CriticalGuardrailHits), fmt.Sprintf("<= %d", cfg.MaxCriticalGuardrails))

	// 6. Declared quota within the team envelope.
	quotaOK := sig.TeamTokenEnvelopePerMin == 0 || agent.Quota.TokensPerMinute <= sig.TeamTokenEnvelopePerMin
	add(GateQuotaDeclared, quotaOK,
		"requested tokens per minute must fit inside the team's allocated envelope",
		fmt.Sprintf("%d tpm", agent.Quota.TokensPerMinute),
		fmt.Sprintf("<= %d tpm", sig.TeamTokenEnvelopePerMin))

	// 7. Cost projection.
	costOK := sig.TeamMonthlyBudgetUSD == 0 || sig.ProjectedMonthlyCostUSD <= sig.TeamMonthlyBudgetUSD
	add(GateCostProjection, costOK,
		"projected monthly spend must fit inside the team's budget",
		fmt.Sprintf("$%.2f projected", sig.ProjectedMonthlyCostUSD),
		fmt.Sprintf("<= $%.2f", sig.TeamMonthlyBudgetUSD))

	// 8. Security review, production only.
	if to == identity.EnvProd && cfg.RequireSecurityReview {
		ref := agent.SecurityReviewRef
		if ref == "" {
			ref = sig.SecurityReviewRef
		}
		valid := ref != "" && (sig.SecurityReviewExpiresAt == nil || sig.SecurityReviewExpiresAt.After(now))
		observed := ref
		if ref == "" {
			observed = "none"
		}
		add(GateSecurityReview, valid,
			"production requires a linked, unexpired security review reference",
			observed, "a valid CHG or RITM reference")
	} else {
		res.Checks = append(res.Checks, GateCheck{
			Name: GateSecurityReview, Status: CheckSkip,
			Detail: "security review is required for production promotions only",
		})
	}

	res.Passed = true
	for _, c := range res.Checks {
		if c.Status == CheckFail {
			res.Passed = false
		}
	}
	return res, nil
}

func missingOwnerFields(a *Agent) []string {
	var missing []string
	if a.Owner.Team == "" {
		missing = append(missing, "owner.team")
	}
	if a.Owner.Email == "" {
		missing = append(missing, "owner.email")
	}
	if a.Owner.OnCall == "" {
		missing = append(missing, "owner.oncall")
	}
	if a.Owner.CostCenter == "" {
		missing = append(missing, "owner.cost_center")
	}
	if a.DataClassification == "" {
		missing = append(missing, "data_classification")
	}
	return missing
}

// ApproverRole distinguishes the two parties required for a production
// promotion.
type ApproverRole string

// Approver roles.
const (
	RoleOwner    ApproverRole = "owning_team"
	RolePlatform ApproverRole = "platform"
)

// Approval is one recorded human decision.
type Approval struct {
	Actor    string       `json:"actor"`
	Role     ApproverRole `json:"role"`
	Approved bool         `json:"approved"`
	Comment  string       `json:"comment,omitempty"`
	At       time.Time    `json:"at"`
}

// PromotionState is the lifecycle of a promotion request.
type PromotionState string

// Promotion request states.
const (
	PromotionPending  PromotionState = "pending"
	PromotionApproved PromotionState = "approved"
	PromotionRejected PromotionState = "rejected"
	PromotionApplied  PromotionState = "applied"
	PromotionExpired  PromotionState = "expired"
	PromotionBlocked  PromotionState = "blocked"
)

// PromotionRequest is the auditable record of one promotion attempt.
type PromotionRequest struct {
	ID          string               `json:"id"`
	AgentID     string               `json:"agent_id"`
	Identity    string               `json:"identity"`
	Version     string               `json:"version"`
	From        identity.Environment `json:"from"`
	To          identity.Environment `json:"to"`
	RequestedBy string               `json:"requested_by"`
	RequestedAt time.Time            `json:"requested_at"`
	ExpiresAt   time.Time            `json:"expires_at"`
	Gate        GateResult           `json:"gate"`
	Approvals   []Approval           `json:"approvals"`
	State       PromotionState       `json:"state"`
	ChangeRef   string               `json:"change_ref,omitempty"`
	AppliedAt   *time.Time           `json:"applied_at,omitempty"`
}

// NewPromotionRequest builds a request with a bounded approval window.
func NewPromotionRequest(agent *Agent, version string, from, to identity.Environment, requestedBy string, gate GateResult, window time.Duration) *PromotionRequest {
	now := time.Now().UTC()
	state := PromotionPending
	if !gate.Passed {
		state = PromotionBlocked
	}
	return &PromotionRequest{
		ID:          "prm_" + uuid.NewString(),
		AgentID:     agent.AgentID,
		Identity:    agent.IdentityS,
		Version:     version,
		From:        from,
		To:          to,
		RequestedBy: requestedBy,
		RequestedAt: now,
		ExpiresAt:   now.Add(window),
		Gate:        gate,
		State:       state,
	}
}

// RequiredApprovals reports how many distinct human approvals the target
// environment needs. Production requires two, from different people in
// different roles; lower environments require none.
func RequiredApprovals(to identity.Environment) int {
	if to == identity.EnvProd {
		return 2
	}
	return 0
}

// Errors returned by the approval flow.
var (
	ErrSelfApproval    = fmt.Errorf("the requester may not approve their own promotion")
	ErrDuplicateActor  = fmt.Errorf("this actor has already recorded a decision")
	ErrNotPending      = fmt.Errorf("promotion request is not pending")
	ErrPromotionExpiry = fmt.Errorf("promotion request has expired")
	ErrRoleSatisfied   = fmt.Errorf("this approver role is already satisfied")
)

// Record adds an approval or rejection, enforcing the two-party rule.
func (p *PromotionRequest) Record(a Approval, now time.Time) error {
	if p.State != PromotionPending {
		return ErrNotPending
	}
	if now.After(p.ExpiresAt) {
		p.State = PromotionExpired
		return ErrPromotionExpiry
	}
	if a.Actor == p.RequestedBy {
		return ErrSelfApproval
	}
	for _, existing := range p.Approvals {
		if existing.Actor == a.Actor {
			return ErrDuplicateActor
		}
		if existing.Role == a.Role && existing.Approved && a.Approved {
			return ErrRoleSatisfied
		}
	}
	a.At = now
	p.Approvals = append(p.Approvals, a)

	if !a.Approved {
		p.State = PromotionRejected
		return nil
	}
	approved := 0
	roles := map[ApproverRole]bool{}
	for _, ap := range p.Approvals {
		if ap.Approved {
			approved++
			roles[ap.Role] = true
		}
	}
	if approved >= RequiredApprovals(p.To) && roles[RoleOwner] && roles[RolePlatform] {
		p.State = PromotionApproved
	}
	return nil
}

// ChangePayload renders the promotion as an enterprise change record. The
// field names match a stock ServiceNow change request so the integration is a
// mapping exercise rather than a translation layer, and the same payload is
// printed for the manual path when the integration is disabled.
func (p *PromotionRequest) ChangePayload(agent *Agent) map[string]any {
	failed := p.Gate.Failed()
	return map[string]any{
		"short_description": fmt.Sprintf("Promote %s %s to %s", agent.IdentityS, p.Version, p.To),
		"description": fmt.Sprintf(
			"Agent: %s\nVersion: %s\nOwner: %s <%s>\nOn-call: %s\nCost centre: %s\nData classification: %s\nGate: %d checks, %d failed\nPromotion request: %s",
			agent.IdentityS, p.Version, agent.Owner.Team, agent.Owner.Email, agent.Owner.OnCall,
			agent.Owner.CostCenter, agent.DataClassification, len(p.Gate.Checks), len(failed), p.ID),
		"assignment_group": "AI Platform",
		"category":         "Application",
		"risk":             riskFor(agent.DataClassification),
		"type":             "standard",
		"u_agent_id":       agent.AgentID,
		"u_cost_center":    agent.Owner.CostCenter,
		"u_promotion_id":   p.ID,
		"u_gate_passed":    p.Gate.Passed,
		"u_gate_failures":  failed,
	}
}

func riskFor(c DataClassification) string {
	switch c {
	case ClassRestricted:
		return "high"
	case ClassConfidential:
		return "moderate"
	default:
		return "low"
	}
}
