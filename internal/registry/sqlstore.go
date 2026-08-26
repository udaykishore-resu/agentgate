package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agentgate/agentgate/internal/identity"
)

// SQLStore is the production Store, backed by PostgreSQL.
//
// Records are stored as JSON documents with the query keys promoted to
// columns. The alternative — a fully normalised schema across agents,
// versions, gates, checks, approvals and waivers — buys join flexibility the
// platform does not need and costs a migration every time the gate gains a
// check. The document is versioned by the application, the columns carry the
// indexes, and the audit records are append-only. See
// docs/adr/0015-registry-storage-model.md.
//
// The driver is registered by the binary, not by this package, so that the
// database dependency is a deployment choice rather than a compile-time one.
// deploy/sql/schema.sql holds the DDL.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore wraps an already-open database handle. The caller configures
// pooling, since the right pool size depends on the deployment topology.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

// OpenSQLStore opens a database using a driver the binary has registered.
func OpenSQLStore(driver, dsn string, maxOpen, maxIdle int, connLifetime time.Duration) (*SQLStore, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(connLifetime)
	return &SQLStore{db: db}, nil
}

// Migrate applies the schema. It is idempotent and safe to run on every start,
// which keeps a fresh environment one command away from working.
func (s *SQLStore) Migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agents (
			agent_id     TEXT PRIMARY KEY,
			identity     TEXT NOT NULL UNIQUE,
			tenant       TEXT NOT NULL,
			team         TEXT NOT NULL,
			doc          TEXT NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE INDEX IF NOT EXISTS agents_tenant_team_idx ON agents (tenant, team)`,
		`CREATE TABLE IF NOT EXISTS promotions (
			id           TEXT PRIMARY KEY,
			agent_id     TEXT NOT NULL,
			state        TEXT NOT NULL,
			requested_at TIMESTAMPTZ NOT NULL,
			doc          TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS promotions_agent_idx ON promotions (agent_id, requested_at DESC)`,
		`CREATE INDEX IF NOT EXISTS promotions_state_idx ON promotions (state)`,
		`CREATE TABLE IF NOT EXISTS credentials (
			client_id   TEXT PRIMARY KEY,
			agent_id    TEXT NOT NULL,
			doc         TEXT NOT NULL,
			created_at  TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS waivers (
			id         TEXT PRIMARY KEY,
			agent_id   TEXT NOT NULL,
			gate       TEXT NOT NULL,
			doc        TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS waivers_agent_idx ON waivers (agent_id)`,
		`CREATE TABLE IF NOT EXISTS attestations (
			agent_id    TEXT NOT NULL,
			env         TEXT NOT NULL,
			attestation TEXT NOT NULL,
			observed_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (agent_id, env)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// PutAgent inserts or updates an agent.
func (s *SQLStore) PutAgent(ctx context.Context, a *Agent) error {
	if err := a.hydrate(); err != nil {
		return err
	}
	a.UpdatedAt = time.Now().UTC()
	doc, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agents (agent_id, identity, tenant, team, doc, updated_at)
		VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (agent_id) DO UPDATE
		SET identity=EXCLUDED.identity, tenant=EXCLUDED.tenant, team=EXCLUDED.team,
		    doc=EXCLUDED.doc, updated_at=now()`,
		a.AgentID, a.IdentityS, a.Identity.Tenant, a.Identity.Team, string(doc))
	if err != nil && strings.Contains(err.Error(), "agents_identity_key") {
		return fmt.Errorf("%w: identity %s is already registered", ErrConflict, a.IdentityS)
	}
	return err
}

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var doc string
	if err := row.Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var a Agent
	if err := json.Unmarshal([]byte(doc), &a); err != nil {
		return nil, err
	}
	if err := a.hydrate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAgent returns an agent by id.
func (s *SQLStore) GetAgent(ctx context.Context, agentID string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT doc FROM agents WHERE agent_id=$1`, agentID))
}

// GetAgentByIdentity returns an agent by identity URI.
func (s *SQLStore) GetAgentByIdentity(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT doc FROM agents WHERE identity=$1`, id))
}

// ListAgents returns agents matching a filter.
func (s *SQLStore) ListAgents(ctx context.Context, f Filter) ([]*Agent, error) {
	q := `SELECT doc FROM agents WHERE 1=1`
	var args []any
	if f.Tenant != "" {
		args = append(args, f.Tenant)
		q += fmt.Sprintf(" AND tenant=$%d", len(args))
	}
	if f.Team != "" {
		args = append(args, f.Team)
		q += fmt.Sprintf(" AND team=$%d", len(args))
	}
	q += " ORDER BY identity"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		// Env, state and search are applied in the application so the same
		// semantics hold for both stores and so a filter change does not need
		// a migration.
		if !matchesFilter(a, f) {
			continue
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func matchesFilter(a *Agent, f Filter) bool {
	if f.Search != "" && !strings.Contains(strings.ToLower(a.IdentityS+" "+a.DisplayName), strings.ToLower(f.Search)) {
		return false
	}
	if f.Env == "" && f.State == "" {
		return true
	}
	for _, v := range a.Versions {
		if f.Env != "" && v.Env != f.Env {
			continue
		}
		if f.State != "" && v.State != f.State {
			continue
		}
		return true
	}
	return false
}

// DeleteAgent removes an agent.
func (s *SQLStore) DeleteAgent(ctx context.Context, agentID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE agent_id=$1`, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutPromotion inserts or updates a promotion request.
func (s *SQLStore) PutPromotion(ctx context.Context, p *PromotionRequest) error {
	doc, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO promotions (id, agent_id, state, requested_at, doc)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET state=EXCLUDED.state, doc=EXCLUDED.doc`,
		p.ID, p.AgentID, string(p.State), p.RequestedAt, string(doc))
	return err
}

// GetPromotion returns a promotion request.
func (s *SQLStore) GetPromotion(ctx context.Context, id string) (*PromotionRequest, error) {
	var doc string
	if err := s.db.QueryRowContext(ctx, `SELECT doc FROM promotions WHERE id=$1`, id).Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var p PromotionRequest
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPromotions returns promotion requests newest first.
func (s *SQLStore) ListPromotions(ctx context.Context, agentID string, state PromotionState) ([]*PromotionRequest, error) {
	q := `SELECT doc FROM promotions WHERE 1=1`
	var args []any
	if agentID != "" {
		args = append(args, agentID)
		q += fmt.Sprintf(" AND agent_id=$%d", len(args))
	}
	if state != "" {
		args = append(args, string(state))
		q += fmt.Sprintf(" AND state=$%d", len(args))
	}
	q += " ORDER BY requested_at DESC LIMIT 500"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*PromotionRequest
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var p PromotionRequest
		if err := json.Unmarshal([]byte(doc), &p); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// PutCredential stores a credential record.
func (s *SQLStore) PutCredential(ctx context.Context, agentID string, c identity.ClientCredential) error {
	doc, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO credentials (client_id, agent_id, doc, created_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (client_id) DO UPDATE SET doc=EXCLUDED.doc`,
		c.ClientID, agentID, string(doc), c.CreatedAt)
	return err
}

// GetCredential returns a credential and its owning agent.
func (s *SQLStore) GetCredential(ctx context.Context, clientID string) (identity.ClientCredential, string, error) {
	var doc, agentID string
	err := s.db.QueryRowContext(ctx, `SELECT doc, agent_id FROM credentials WHERE client_id=$1`, clientID).Scan(&doc, &agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ClientCredential{}, "", ErrNotFound
		}
		return identity.ClientCredential{}, "", err
	}
	var c identity.ClientCredential
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		return identity.ClientCredential{}, "", err
	}
	return c, agentID, nil
}

// ListCredentials returns every credential.
func (s *SQLStore) ListCredentials(ctx context.Context) ([]CredentialRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id, doc FROM credentials ORDER BY client_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []CredentialRecord
	for rows.Next() {
		var agentID, doc string
		if err := rows.Scan(&agentID, &doc); err != nil {
			return nil, err
		}
		var c identity.ClientCredential
		if err := json.Unmarshal([]byte(doc), &c); err != nil {
			return nil, err
		}
		out = append(out, CredentialRecord{AgentID: agentID, Credential: c})
	}
	return out, rows.Err()
}

// PutWaiver records a gate exception.
func (s *SQLStore) PutWaiver(ctx context.Context, w Waiver) error {
	doc, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO waivers (id, agent_id, gate, doc, expires_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET doc=EXCLUDED.doc, expires_at=EXCLUDED.expires_at`,
		w.ID, w.AgentID, w.Gate, string(doc), w.ExpiresAt)
	return err
}

// ListWaivers returns the waivers for an agent.
func (s *SQLStore) ListWaivers(ctx context.Context, agentID string) ([]Waiver, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc FROM waivers WHERE agent_id=$1`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Waiver
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var w Waiver
		if err := json.Unmarshal([]byte(doc), &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GrantedAt.After(out[j].GrantedAt) })
	return out, rows.Err()
}

// RecordAttestation notes how an agent authenticated.
func (s *SQLStore) RecordAttestation(ctx context.Context, agentID string, env identity.Environment, a identity.Attestation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO attestations (agent_id, env, attestation, observed_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (agent_id, env) DO UPDATE SET attestation=EXCLUDED.attestation, observed_at=now()`,
		agentID, string(env), string(a))
	return err
}

// Attestation returns the last recorded attestation.
func (s *SQLStore) Attestation(ctx context.Context, agentID string, env identity.Environment) (identity.Attestation, error) {
	var a string
	err := s.db.QueryRowContext(ctx,
		`SELECT attestation FROM attestations WHERE agent_id=$1 AND env=$2`, agentID, string(env)).Scan(&a)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return identity.Attestation(a), err
}

// Close closes the database handle.
func (s *SQLStore) Close() error { return s.db.Close() }
