package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
)

// Errors returned by a Store.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Filter narrows a list query.
type Filter struct {
	Tenant string
	Team   string
	Env    identity.Environment
	State  State
	Search string
}

// Store persists the agent inventory and the promotion audit trail.
//
// Two implementations ship: MemoryStore, which is file-backed and is what the
// local stack and the tests use, and SQLStore, which is what production runs
// against Postgres. Both satisfy the same tests.
type Store interface {
	PutAgent(ctx context.Context, a *Agent) error
	GetAgent(ctx context.Context, agentID string) (*Agent, error)
	GetAgentByIdentity(ctx context.Context, id string) (*Agent, error)
	ListAgents(ctx context.Context, f Filter) ([]*Agent, error)
	DeleteAgent(ctx context.Context, agentID string) error

	PutPromotion(ctx context.Context, p *PromotionRequest) error
	GetPromotion(ctx context.Context, id string) (*PromotionRequest, error)
	ListPromotions(ctx context.Context, agentID string, state PromotionState) ([]*PromotionRequest, error)

	PutCredential(ctx context.Context, agentID string, c identity.ClientCredential) error
	GetCredential(ctx context.Context, clientID string) (identity.ClientCredential, string, error)
	ListCredentials(ctx context.Context) ([]CredentialRecord, error)

	PutWaiver(ctx context.Context, w Waiver) error
	ListWaivers(ctx context.Context, agentID string) ([]Waiver, error)

	// RecordAttestation notes how an agent most recently authenticated, which
	// the promotion gate reads.
	RecordAttestation(ctx context.Context, agentID string, env identity.Environment, a identity.Attestation) error
	Attestation(ctx context.Context, agentID string, env identity.Environment) (identity.Attestation, error)

	Close() error
}

// CredentialRecord ties a credential to its agent for rotation reporting.
type CredentialRecord struct {
	AgentID    string                    `json:"agent_id"`
	Credential identity.ClientCredential `json:"credential"`
}

type attestKey struct {
	agentID string
	env     identity.Environment
}

// MemoryStore is an in-process Store with optional JSON file persistence.
// Persistence exists so that a local stack, a CI run or a demo survives a
// restart; it is not a production storage engine and config validation refuses
// it in production.
type MemoryStore struct {
	path string

	// loadedAt is the modification time of the snapshot this process last
	// read. In the local stack the control plane writes the snapshot and the
	// fleet view reads it, so a reader that never re-reads shows an empty
	// fleet forever. Production runs both against SQLStore and shares no file;
	// this is a development affordance and is documented as one.
	loadedAt time.Time

	mu          sync.RWMutex
	agents      map[string]*Agent
	byIdentity  map[string]string
	promotions  map[string]*PromotionRequest
	credentials map[string]CredentialRecord
	waivers     map[string][]Waiver
	attest      map[attestKey]identity.Attestation
}

type memorySnapshot struct {
	Agents      map[string]*Agent            `json:"agents"`
	Promotions  map[string]*PromotionRequest `json:"promotions"`
	Credentials map[string]CredentialRecord  `json:"credentials"`
	Waivers     map[string][]Waiver          `json:"waivers"`
	Attest      map[string]string            `json:"attestations"`
}

// NewMemoryStore creates a store. An empty path disables persistence.
func NewMemoryStore(path string) (*MemoryStore, error) {
	s := &MemoryStore{
		path:        path,
		agents:      map[string]*Agent{},
		byIdentity:  map[string]string{},
		promotions:  map[string]*PromotionRequest{},
		credentials: map[string]CredentialRecord{},
		waivers:     map[string][]Waiver{},
		attest:      map[attestKey]identity.Attestation{},
	}
	if path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var snap memorySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("parse registry snapshot: %w", err)
	}
	if snap.Agents != nil {
		s.agents = snap.Agents
	}
	if snap.Promotions != nil {
		s.promotions = snap.Promotions
	}
	if snap.Credentials != nil {
		s.credentials = snap.Credentials
	}
	if snap.Waivers != nil {
		s.waivers = snap.Waivers
	}
	for k, v := range snap.Attest {
		parts := strings.SplitN(k, "|", 2)
		if len(parts) == 2 {
			s.attest[attestKey{agentID: parts[0], env: identity.Environment(parts[1])}] = identity.Attestation(v)
		}
	}
	for id, a := range s.agents {
		if err := a.hydrate(); err != nil {
			return nil, err
		}
		s.byIdentity[a.IdentityS] = id
	}
	if info, err := os.Stat(path); err == nil {
		s.loadedAt = info.ModTime()
	}
	return s, nil
}

// refresh re-reads the snapshot when another process has written it since this
// one last looked. It is a no-op when persistence is disabled, and costs one
// stat call on a read path that is not the request path.
func (s *MemoryStore) refresh() {
	if s.path == "" {
		return
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.mu.RLock()
	fresh := !info.ModTime().After(s.loadedAt)
	s.mu.RUnlock()
	if fresh {
		return
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var snap memorySnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return
	}
	byIdentity := map[string]string{}
	for id, a := range snap.Agents {
		if err := a.hydrate(); err != nil {
			// A snapshot written by a newer version may carry a record this
			// binary cannot read. Skipping it is better than discarding the
			// whole inventory.
			continue
		}
		byIdentity[a.IdentityS] = id
	}
	attest := map[attestKey]identity.Attestation{}
	for k, v := range snap.Attest {
		if parts := strings.SplitN(k, "|", 2); len(parts) == 2 {
			attest[attestKey{agentID: parts[0], env: identity.Environment(parts[1])}] = identity.Attestation(v)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if snap.Agents != nil {
		s.agents, s.byIdentity = snap.Agents, byIdentity
	}
	if snap.Promotions != nil {
		s.promotions = snap.Promotions
	}
	if snap.Credentials != nil {
		s.credentials = snap.Credentials
	}
	if snap.Waivers != nil {
		s.waivers = snap.Waivers
	}
	if len(attest) > 0 {
		s.attest = attest
	}
	s.loadedAt = info.ModTime()
}

func (a *Agent) hydrate() error {
	if a.IdentityS == "" {
		return fmt.Errorf("agent %s has no identity", a.AgentID)
	}
	id, err := identity.ParseAgentIdentity(a.IdentityS)
	if err != nil {
		return err
	}
	a.Identity = id
	return nil
}

func (s *MemoryStore) flushLocked() error {
	if s.path == "" {
		return nil
	}
	attest := map[string]string{}
	for k, v := range s.attest {
		attest[k.agentID+"|"+string(k.env)] = string(v)
	}
	raw, err := json.MarshalIndent(memorySnapshot{
		Agents: s.agents, Promotions: s.promotions,
		Credentials: s.credentials, Waivers: s.waivers, Attest: attest,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if info, err := os.Stat(s.path); err == nil {
		s.loadedAt = info.ModTime()
	}
	return nil
}

// PutAgent inserts or replaces an agent record.
func (s *MemoryStore) PutAgent(_ context.Context, a *Agent) error {
	if err := a.hydrate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byIdentity[a.IdentityS]; ok && existing != a.AgentID {
		return fmt.Errorf("%w: identity %s already registered as %s", ErrConflict, a.IdentityS, existing)
	}
	a.UpdatedAt = time.Now().UTC()
	clone := *a
	s.agents[a.AgentID] = &clone
	s.byIdentity[a.IdentityS] = a.AgentID
	return s.flushLocked()
}

// GetAgent returns an agent by id.
func (s *MemoryStore) GetAgent(_ context.Context, agentID string) (*Agent, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[agentID]
	if !ok {
		return nil, fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	clone := *a
	return &clone, nil
}

// GetAgentByIdentity returns an agent by its identity URI.
func (s *MemoryStore) GetAgentByIdentity(ctx context.Context, id string) (*Agent, error) {
	s.refresh()
	s.mu.RLock()
	agentID, ok := s.byIdentity[id]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: identity %s", ErrNotFound, id)
	}
	return s.GetAgent(ctx, agentID)
}

// ListAgents returns agents matching a filter, sorted by identity.
func (s *MemoryStore) ListAgents(_ context.Context, f Filter) ([]*Agent, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Agent
	for _, a := range s.agents {
		if f.Tenant != "" && a.Identity.Tenant != f.Tenant {
			continue
		}
		if f.Team != "" && a.Identity.Team != f.Team {
			continue
		}
		if f.Search != "" && !strings.Contains(strings.ToLower(a.IdentityS+" "+a.DisplayName), strings.ToLower(f.Search)) {
			continue
		}
		if f.Env != "" || f.State != "" {
			match := false
			for _, v := range a.Versions {
				if f.Env != "" && v.Env != f.Env {
					continue
				}
				if f.State != "" && v.State != f.State {
					continue
				}
				match = true
				break
			}
			if !match {
				continue
			}
		}
		clone := *a
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IdentityS < out[j].IdentityS })
	return out, nil
}

// DeleteAgent removes an agent. Registration history is expected to be
// retained elsewhere; this exists for test cleanup and de-registration.
func (s *MemoryStore) DeleteAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[agentID]
	if !ok {
		return fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	delete(s.byIdentity, a.IdentityS)
	delete(s.agents, agentID)
	return s.flushLocked()
}

// PutPromotion stores a promotion request.
func (s *MemoryStore) PutPromotion(_ context.Context, p *PromotionRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *p
	s.promotions[p.ID] = &clone
	return s.flushLocked()
}

// GetPromotion returns a promotion request by id.
func (s *MemoryStore) GetPromotion(_ context.Context, id string) (*PromotionRequest, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.promotions[id]
	if !ok {
		return nil, fmt.Errorf("%w: promotion %s", ErrNotFound, id)
	}
	clone := *p
	return &clone, nil
}

// ListPromotions returns promotion requests, newest first.
func (s *MemoryStore) ListPromotions(_ context.Context, agentID string, state PromotionState) ([]*PromotionRequest, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*PromotionRequest
	for _, p := range s.promotions {
		if agentID != "" && p.AgentID != agentID {
			continue
		}
		if state != "" && p.State != state {
			continue
		}
		clone := *p
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	return out, nil
}

// PutCredential stores a client credential record.
func (s *MemoryStore) PutCredential(_ context.Context, agentID string, c identity.ClientCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.credentials[c.ClientID] = CredentialRecord{AgentID: agentID, Credential: c}
	return s.flushLocked()
}

// GetCredential returns a credential and its owning agent id.
func (s *MemoryStore) GetCredential(_ context.Context, clientID string) (identity.ClientCredential, string, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.credentials[clientID]
	if !ok {
		return identity.ClientCredential{}, "", fmt.Errorf("%w: client %s", ErrNotFound, clientID)
	}
	return r.Credential, r.AgentID, nil
}

// ListCredentials returns every credential for rotation reporting.
func (s *MemoryStore) ListCredentials(_ context.Context) ([]CredentialRecord, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CredentialRecord, 0, len(s.credentials))
	for _, r := range s.credentials {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Credential.ClientID < out[j].Credential.ClientID })
	return out, nil
}

// PutWaiver records a gate exception.
func (s *MemoryStore) PutWaiver(_ context.Context, w Waiver) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waivers[w.AgentID] = append(s.waivers[w.AgentID], w)
	return s.flushLocked()
}

// ListWaivers returns the waivers for an agent.
func (s *MemoryStore) ListWaivers(_ context.Context, agentID string) ([]Waiver, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Waiver{}, s.waivers[agentID]...), nil
}

// RecordAttestation notes the strongest attestation seen for an agent.
func (s *MemoryStore) RecordAttestation(_ context.Context, agentID string, env identity.Environment, a identity.Attestation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attest[attestKey{agentID: agentID, env: env}] = a
	return nil
}

// Attestation returns the last recorded attestation.
func (s *MemoryStore) Attestation(_ context.Context, agentID string, env identity.Environment) (identity.Attestation, error) {
	s.refresh()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attest[attestKey{agentID: agentID, env: env}], nil
}

// Close flushes any pending state.
func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}
