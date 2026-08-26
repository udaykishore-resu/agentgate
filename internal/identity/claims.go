package identity

import (
	"errors"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the AgentGate access-token claim set. Every field the gateway
// enforces on is in the token, signed, so the traffic plane never has to call
// the control plane on the request path. That is what keeps the gateway's own
// availability independent of the control plane's.
type Claims struct {
	jwt.RegisteredClaims

	AgentID      string      `json:"agent_id"`
	Tenant       string      `json:"tenant"`
	Team         string      `json:"team"`
	AgentName    string      `json:"agent_name"`
	AgentVersion string      `json:"agent_version"`
	Env          Environment `json:"env"`
	CostCenter   string      `json:"cost_center"`
	OwnerEmail   string      `json:"owner_email,omitempty"`
	Runtime      string      `json:"runtime,omitempty"`
	Framework    string      `json:"framework,omitempty"`
	Scopes       []string    `json:"scopes"`
	ModelPools   []string    `json:"model_pools"`
	Attestation  Attestation `json:"attestation"`
	// DataClassification is the highest classification this agent is cleared
	// to send. It selects the guardrail failure mode and gates which backends
	// the request may be routed to.
	DataClassification string `json:"data_classification,omitempty"`
}

// Errors returned by claim validation. They are deliberately coarse: the
// caller learns that authentication failed, not which check failed, while the
// span and the log record carry the precise reason.
var (
	ErrMissingClaim    = errors.New("token is missing a required claim")
	ErrIdentityInvalid = errors.New("token subject is not a valid agent identity")
	ErrEnvInvalid      = errors.New("token environment is not a known environment")
)

// Identity returns the agent identity carried by the subject claim.
func (c *Claims) Identity() (AgentIdentity, error) { return ParseAgentIdentity(c.Subject) }

// Validate checks the AgentGate-specific claims. Signature, issuer, audience
// and expiry are checked by the verifier before this runs.
func (c *Claims) Validate() error {
	id, err := c.Identity()
	if err != nil {
		return ErrIdentityInvalid
	}
	if c.AgentID == "" || c.AgentVersion == "" {
		return ErrMissingClaim
	}
	if !c.Env.Valid() {
		return ErrEnvInvalid
	}
	// Ownership must be internally consistent: the subject URI and the flat
	// claims are both used downstream and a mismatch would let an agent be
	// charged to one team and rate-limited as another.
	if c.Tenant != id.Tenant || c.Team != id.Team || c.AgentName != id.Name {
		return ErrIdentityInvalid
	}
	// No cost centre means no chargeback attribution, which is not allowed to
	// reach production.
	if c.CostCenter == "" && c.Env == EnvProd {
		return ErrMissingClaim
	}
	return nil
}

// HasScope reports whether the token carries a capability.
func (c *Claims) HasScope(scope string) bool {
	return slices.Contains(c.Scopes, scope) || slices.Contains(c.Scopes, ScopeAdmin)
}

// MayUsePool reports whether the token entitles the agent to a pool.
func (c *Claims) MayUsePool(pool string) bool {
	if len(c.ModelPools) == 0 {
		return false
	}
	return slices.Contains(c.ModelPools, pool) || slices.Contains(c.ModelPools, "*")
}

// QuotaKey is the rate-limit and quota bucket key. Keying on the agent rather
// than the version means a deployment does not reset an agent's budget.
func (c *Claims) QuotaKey() string {
	return c.Tenant + ":" + c.Team + ":" + c.AgentName + ":" + string(c.Env)
}

// TimeToExpiry reports how long the token remains valid.
func (c *Claims) TimeToExpiry(now time.Time) time.Duration {
	if c.ExpiresAt == nil {
		return 0
	}
	return c.ExpiresAt.Time.Sub(now)
}
