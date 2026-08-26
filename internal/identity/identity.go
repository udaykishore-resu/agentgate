// Package identity implements agent workload identity: the identity URI, the
// claim set carried in an access token, issuance by the control plane, and
// verification at the gateway.
//
// The central design point is that an agent's identity is per-version and
// short-lived. A conventional service identity answers "which service is
// calling"; an agent identity has to answer "which version of which agent,
// owned by which team, charged to which cost centre, permitted in which
// environment" — because that is what quota, chargeback and the promotion gate
// are enforced against. See docs/05-identity.md.
package identity

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// AgentIdentity is the stable identity of an agent, independent of version and
// environment:
//
//	agent://<tenant>/<team>/<name>
type AgentIdentity struct {
	Tenant string
	Team   string
	Name   string
}

var segmentRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// ParseAgentIdentity parses and validates an agent identity URI. Segments are
// restricted to DNS-label characters so an identity can safely appear in a
// metric label, a Kubernetes object name and a SPIFFE ID without escaping.
func ParseAgentIdentity(s string) (AgentIdentity, error) {
	u, err := url.Parse(s)
	if err != nil {
		return AgentIdentity{}, fmt.Errorf("invalid agent identity %q: %w", s, err)
	}
	if u.Scheme != "agent" {
		return AgentIdentity{}, fmt.Errorf("invalid agent identity %q: scheme must be agent://", s)
	}
	tenant := u.Host
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if tenant == "" || len(parts) != 2 {
		return AgentIdentity{}, fmt.Errorf("invalid agent identity %q: want agent://tenant/team/name", s)
	}
	id := AgentIdentity{Tenant: tenant, Team: parts[0], Name: parts[1]}
	for label, v := range map[string]string{"tenant": id.Tenant, "team": id.Team, "name": id.Name} {
		if !segmentRE.MatchString(v) {
			return AgentIdentity{}, fmt.Errorf("invalid agent identity %q: %s segment %q is not a DNS label", s, label, v)
		}
	}
	return id, nil
}

// String renders the identity URI.
func (a AgentIdentity) String() string {
	return fmt.Sprintf("agent://%s/%s/%s", a.Tenant, a.Team, a.Name)
}

// IsZero reports whether the identity is unset.
func (a AgentIdentity) IsZero() bool { return a.Tenant == "" && a.Team == "" && a.Name == "" }

// Environment is a deployment environment in the promotion chain.
type Environment string

// The promotion chain, in order.
const (
	EnvDev     Environment = "dev"
	EnvStaging Environment = "staging"
	EnvProd    Environment = "prod"
)

// Valid reports whether e is a known environment.
func (e Environment) Valid() bool {
	switch e {
	case EnvDev, EnvStaging, EnvProd:
		return true
	}
	return false
}

// Previous returns the environment an agent must be promoted from to reach e.
func (e Environment) Previous() (Environment, bool) {
	switch e {
	case EnvStaging:
		return EnvDev, true
	case EnvProd:
		return EnvStaging, true
	default:
		return "", false
	}
}

// Attestation records how an agent proved who it is. Federated workload
// identity is strictly stronger than a shared secret and the promotion gate
// requires it for production.
type Attestation string

// Attestation kinds.
const (
	AttestWorkloadIdentity Attestation = "workload-identity"
	AttestClientSecret     Attestation = "client-secret"
	AttestDeveloper        Attestation = "developer"
)

// Scopes are the capabilities an access token may carry.
const (
	ScopeInvoke    = "models:invoke"
	ScopeEmbed     = "models:embed"
	ScopeRegister  = "agents:register"
	ScopePromote   = "agents:promote"
	ScopeApprove   = "agents:approve"
	ScopeReadFleet = "fleet:read"
	ScopeAdmin     = "platform:admin"
)
