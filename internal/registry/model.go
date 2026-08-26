// Package registry holds the agent inventory: who owns each agent, what
// versions exist, which environment each version is allowed in, and the
// evidence trail behind every promotion.
//
// The registry is the join key for the whole platform. Telemetry attribution,
// quota, chargeback and the promotion gate all resolve through it, which is
// why registration is a precondition for identity issuance rather than a
// nice-to-have that teams fill in later.
package registry

import (
	"fmt"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
)

// State is the lifecycle state of one agent version in one environment.
type State string

// Version lifecycle states.
const (
	StateRegistered       State = "registered"        // known, not yet deployed anywhere
	StateActive           State = "active"            // serving traffic in this environment
	StatePendingPromotion State = "pending_promotion" // gate evaluated, awaiting approval
	StateBlocked          State = "blocked"           // gate failed
	StateRetired          State = "retired"           // superseded or withdrawn
	StateQuarantined      State = "quarantined"       // forcibly disabled by the platform
)

// DataClassification is the client's data-handling tier. It selects the
// guardrail failure mode and constrains which backends may serve the request.
type DataClassification string

// Data classifications, ordered least to most sensitive.
const (
	ClassPublic       DataClassification = "public"
	ClassInternal     DataClassification = "internal"
	ClassConfidential DataClassification = "confidential"
	ClassRestricted   DataClassification = "restricted"
)

// Rank orders classifications so a backend can be checked as "at least as
// permissive as required".
func (d DataClassification) Rank() int {
	switch d {
	case ClassPublic:
		return 0
	case ClassInternal:
		return 1
	case ClassConfidential:
		return 2
	case ClassRestricted:
		return 3
	default:
		return 2
	}
}

// Owner is the accountable team. Every field is required before an agent can
// be promoted: an agent whose owner cannot be paged is an agent that will page
// the platform team instead.
type Owner struct {
	Team       string `json:"team"`
	Email      string `json:"email"`
	OnCall     string `json:"oncall"`
	CostCenter string `json:"cost_center"`
	Manager    string `json:"manager,omitempty"`
}

// Quota is the agent's declared consumption envelope.
type Quota struct {
	TokensPerMinute    int64 `json:"tokens_per_minute"`
	RequestsPerMinute  int64 `json:"requests_per_minute"`
	MonthlyTokenBudget int64 `json:"monthly_token_budget"`
	MaxConcurrent      int64 `json:"max_concurrent,omitempty"`
}

// Version is one deployable version of an agent in one environment.
type Version struct {
	Version      string               `json:"version"`
	Env          identity.Environment `json:"env"`
	State        State                `json:"state"`
	Image        string               `json:"image,omitempty"`
	CommitSHA    string               `json:"commit_sha,omitempty"`
	RegisteredAt time.Time            `json:"registered_at"`
	RegisteredBy string               `json:"registered_by,omitempty"`
	PromotedAt   *time.Time           `json:"promoted_at,omitempty"`
	PromotedBy   string               `json:"promoted_by,omitempty"`
	RetiredAt    *time.Time           `json:"retired_at,omitempty"`
	// LastGate is the most recent gate evaluation for this version, retained
	// so an auditor can reconstruct why it was allowed into an environment.
	LastGate *GateResult `json:"last_gate,omitempty"`
	Notes    string      `json:"notes,omitempty"`
}

// Key uniquely identifies a version within an agent.
func (v Version) Key() string { return string(v.Env) + "/" + v.Version }

// Agent is a registered agent and all of its versions.
type Agent struct {
	AgentID   string                 `json:"agent_id"`
	Identity  identity.AgentIdentity `json:"-"`
	IdentityS string                 `json:"identity"`

	DisplayName        string             `json:"display_name"`
	Description        string             `json:"description,omitempty"`
	Owner              Owner              `json:"owner"`
	Runtime            string             `json:"runtime"`
	Framework          string             `json:"framework,omitempty"`
	DataClassification DataClassification `json:"data_classification"`
	RequestedPools     []string           `json:"requested_pools"`
	GrantedPools       []string           `json:"granted_pools"`
	Quota              Quota              `json:"quota"`
	Versions           []Version          `json:"versions"`

	SecurityReviewRef string            `json:"security_review_ref,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	Labels            map[string]string `json:"labels,omitempty"`
}

// VersionIn returns the version currently in a given state for an environment.
func (a *Agent) VersionIn(env identity.Environment, state State) (Version, bool) {
	for _, v := range a.Versions {
		if v.Env == env && v.State == state {
			return v, true
		}
	}
	return Version{}, false
}

// Find returns a specific version in a specific environment.
func (a *Agent) Find(env identity.Environment, version string) (Version, bool) {
	for _, v := range a.Versions {
		if v.Env == env && v.Version == version {
			return v, true
		}
	}
	return Version{}, false
}

// Upsert inserts or replaces a version record.
func (a *Agent) Upsert(v Version) {
	for i := range a.Versions {
		if a.Versions[i].Env == v.Env && a.Versions[i].Version == v.Version {
			a.Versions[i] = v
			return
		}
	}
	a.Versions = append(a.Versions, v)
}

// PoolsGranted returns the effective pool entitlement, falling back to the
// requested set only in non-production where a platform grant is not required.
func (a *Agent) PoolsGranted(env identity.Environment) []string {
	if len(a.GrantedPools) > 0 {
		return a.GrantedPools
	}
	if env == identity.EnvProd {
		return nil
	}
	return a.RequestedPools
}

// RegistrationRequest is what CI submits on deployment.
type RegistrationRequest struct {
	Identity           string               `json:"identity"`
	DisplayName        string               `json:"display_name"`
	Description        string               `json:"description,omitempty"`
	Owner              Owner                `json:"owner"`
	Runtime            string               `json:"runtime"`
	Framework          string               `json:"framework,omitempty"`
	DataClassification DataClassification   `json:"data_classification"`
	RequestedPools     []string             `json:"requested_pools"`
	Quota              Quota                `json:"quota"`
	Version            string               `json:"version"`
	Env                identity.Environment `json:"env"`
	Image              string               `json:"image,omitempty"`
	CommitSHA          string               `json:"commit_sha,omitempty"`
	SecurityReviewRef  string               `json:"security_review_ref,omitempty"`
	Labels             map[string]string    `json:"labels,omitempty"`
}

// Validate checks a registration request. The error text is returned to CI, so
// it names the field and says what good looks like.
func (r *RegistrationRequest) Validate() error {
	id, err := identity.ParseAgentIdentity(r.Identity)
	if err != nil {
		return err
	}
	var missing []string
	if r.DisplayName == "" {
		missing = append(missing, "display_name")
	}
	if r.Owner.Team == "" {
		missing = append(missing, "owner.team")
	}
	if r.Owner.Email == "" {
		missing = append(missing, "owner.email")
	}
	if r.Owner.OnCall == "" {
		missing = append(missing, "owner.oncall")
	}
	if r.Owner.CostCenter == "" {
		missing = append(missing, "owner.cost_center")
	}
	if r.Runtime == "" {
		missing = append(missing, "runtime")
	}
	if r.DataClassification == "" {
		missing = append(missing, "data_classification")
	}
	if r.Version == "" {
		missing = append(missing, "version")
	}
	if len(r.RequestedPools) == 0 {
		missing = append(missing, "requested_pools")
	}
	if len(missing) > 0 {
		return fmt.Errorf("registration is missing required fields: %s", strings.Join(missing, ", "))
	}
	if !r.Env.Valid() {
		return fmt.Errorf("env must be one of dev, staging, prod")
	}
	if r.Owner.Team != id.Team {
		return fmt.Errorf("owner.team %q does not match the team segment %q in %s", r.Owner.Team, id.Team, r.Identity)
	}
	if r.Quota.TokensPerMinute <= 0 || r.Quota.RequestsPerMinute <= 0 {
		return fmt.Errorf("quota.tokens_per_minute and quota.requests_per_minute must be positive")
	}
	return nil
}
