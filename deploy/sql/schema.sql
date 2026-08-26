-- AgentGate PostgreSQL schema
--
-- ===========================================================================
-- Storage model: documents with promoted query columns
-- ===========================================================================
--
-- Registry records are stored as JSON documents in a `doc` column, with the
-- handful of keys the platform actually queries on promoted to real, indexed
-- columns alongside. `agents` is the clearest example: `agent_id`, `identity`,
-- `tenant` and `team` are columns because every lookup and every filter uses
-- them; owner, quota, pools, versions, gate snapshots and approvals live inside
-- the document because nothing queries across them.
--
-- Why this rather than a fully normalised schema across agents, versions,
-- gates, checks, approvals and waivers:
--
--   * The promotion gate gains checks. It has eight today and it will have
--     more. In a normalised schema every new check is a migration; here it is
--     an extra object inside an array the application already round-trips.
--   * Gate snapshots are audit evidence and must be stored *verbatim*. A
--     normalised schema re-renders them on read, which means the evidence an
--     auditor sees is a reconstruction rather than a recording.
--   * The query patterns are narrow. Look up one agent by id or identity; list
--     a tenant's or a team's agents; list an agent's promotions newest first;
--     find a credential by client id. Every one of those is served by a
--     promoted column and an index.
--
-- What it costs, stated plainly:
--
--   * No ad-hoc SQL across nested structures. "Which agents have a waived
--     security_review gate" is an application-side scan, not a WHERE clause.
--     Postgres could index into the JSON, but the application also runs against
--     a memory store, and two query implementations with different semantics is
--     worse than one that is occasionally inconvenient.
--   * No database-enforced referential integrity between agents, promotions,
--     credentials and waivers. Those relationships are enforced in the service
--     layer. Foreign keys are deliberately absent so that an agent row can be
--     restored independently of its promotion history during a recovery.
--   * The document is versioned by the application, not by the schema. A
--     structural change to the Go types is a code deploy, and the read path
--     must tolerate documents written by the previous release.
--
-- See docs/adr/0015-registry-storage-model.md for the full argument.
--
-- `usage_records` is the exception. It is flat, columnar and append-only,
-- because chargeback *is* an aggregate-across-rows workload — the exact thing
-- the document model is bad at. It is the same data the usage stream carries
-- (Kafka or Event Hubs in production, an append-only file locally); this table
-- exists so a chargeback report can be produced from the primary database when
-- no warehouse is wired up.
--
-- ===========================================================================
-- Applying this file
-- ===========================================================================
--
-- The registry tables are also created by `SQLStore.Migrate` in
-- internal/registry/sqlstore.go, which runs on every start and is idempotent.
-- This file is the authoritative DDL: it carries the same statements plus the
-- chargeback objects, the comments, and the grants, which the application does
-- not create because the application does not own role management.
--
-- Every statement here is idempotent. Run it against an empty database or an
-- existing one. See README.md in this directory for the migration and rollback
-- procedure.
--
-- Target: PostgreSQL 14 or later.

BEGIN;

SET client_min_messages = warning;

-- ---------------------------------------------------------------------------
-- Registry: agents
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS agents (
    agent_id     TEXT PRIMARY KEY,
    identity     TEXT NOT NULL UNIQUE,
    tenant       TEXT NOT NULL,
    team         TEXT NOT NULL,
    doc          TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  agents IS
    'One row per registered agent. The full registry.Agent document, including every version and its gate snapshot, is in doc.';
COMMENT ON COLUMN agents.agent_id IS
    'Platform-assigned stable id, agt_<20 hex>. The join key for telemetry, quota and chargeback.';
COMMENT ON COLUMN agents.identity IS
    'agent://<tenant>/<team>/<name>. Unique: two agents cannot claim the same identity.';
COMMENT ON COLUMN agents.tenant IS
    'Promoted from the identity URI so tenant filtering is an indexed predicate.';
COMMENT ON COLUMN agents.team IS
    'Promoted from the identity URI. Must equal owner.team inside doc; enforced by the service, not the schema.';
COMMENT ON COLUMN agents.doc IS
    'registry.Agent as JSON. Application-versioned; the read path tolerates documents written by the previous release.';

CREATE INDEX IF NOT EXISTS agents_tenant_team_idx ON agents (tenant, team);

-- ---------------------------------------------------------------------------
-- Registry: promotions
-- ---------------------------------------------------------------------------
--
-- Append-and-update, never delete. A promotion request is the audit record of
-- one attempt, including the verbatim gate snapshot and every recorded
-- approval. Deleting one destroys the answer to "why was this version allowed
-- into production", which is the question this platform exists to be able to
-- answer.

CREATE TABLE IF NOT EXISTS promotions (
    id           TEXT PRIMARY KEY,
    agent_id     TEXT NOT NULL,
    state        TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    doc          TEXT NOT NULL
);

COMMENT ON TABLE  promotions IS
    'One row per promotion attempt. Audit evidence: retained, never deleted.';
COMMENT ON COLUMN promotions.state IS
    'pending | approved | rejected | applied | expired | blocked. Promoted for the pending-approvals query.';
COMMENT ON COLUMN promotions.doc IS
    'registry.PromotionRequest as JSON, including the gate snapshot verbatim and every approval with actor and timestamp.';

CREATE INDEX IF NOT EXISTS promotions_agent_idx ON promotions (agent_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS promotions_state_idx ON promotions (state);

-- ---------------------------------------------------------------------------
-- Registry: credentials
-- ---------------------------------------------------------------------------
--
-- Only the SHA-256 hash of a client secret is stored here. The secret itself
-- lives in the enterprise vault and is returned to the requester exactly once.
-- Nothing in this table can be replayed as a credential.

CREATE TABLE IF NOT EXISTS credentials (
    client_id   TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    doc         TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

COMMENT ON TABLE  credentials IS
    'Client credential records. Contains secret hashes and vault pointers only, never a usable secret.';
COMMENT ON COLUMN credentials.doc IS
    'identity.ClientCredential as JSON: client_id, secret_hash, vault version, created_at, expires_at, retired.';

-- Rotation looks up an agent's credentials, and the secret-rotation-overdue
-- alert scans by age. Neither is served by the primary key alone.
CREATE INDEX IF NOT EXISTS credentials_agent_idx    ON credentials (agent_id);
CREATE INDEX IF NOT EXISTS credentials_created_idx  ON credentials (created_at);

-- ---------------------------------------------------------------------------
-- Registry: waivers
-- ---------------------------------------------------------------------------
--
-- A waiver is a recorded exception to one promotion gate, with an owner and an
-- expiry. Expired waivers are retained: "this gate was waived last quarter, by
-- whom, and why" is an audit question, not a housekeeping one.

CREATE TABLE IF NOT EXISTS waivers (
    id         TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL,
    gate       TEXT NOT NULL,
    doc        TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

COMMENT ON TABLE  waivers IS
    'Time-boxed promotion gate exceptions. Retained after expiry as audit evidence.';
COMMENT ON COLUMN waivers.gate IS
    'The gate name waived, e.g. security_review. Matches registry.Gate* constants.';

CREATE INDEX IF NOT EXISTS waivers_agent_idx   ON waivers (agent_id);
-- Supports the "which exceptions are still in force" report and the expiry sweep.
CREATE INDEX IF NOT EXISTS waivers_expires_idx ON waivers (expires_at);

-- ---------------------------------------------------------------------------
-- Registry: attestations
-- ---------------------------------------------------------------------------
--
-- The last observed authentication method per agent per environment. It is the
-- evidence behind the identity_attested promotion gate: an agent that has only
-- ever presented a client secret cannot be promoted to production.

CREATE TABLE IF NOT EXISTS attestations (
    agent_id    TEXT NOT NULL,
    env         TEXT NOT NULL,
    attestation TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (agent_id, env)
);

COMMENT ON TABLE  attestations IS
    'Last observed attestation per agent per environment. Input to the identity_attested gate.';
COMMENT ON COLUMN attestations.attestation IS
    'workload-identity | client-secret | developer.';

-- ---------------------------------------------------------------------------
-- Chargeback: usage_records
-- ---------------------------------------------------------------------------
--
-- One immutable row per completed request, matching cost.Record. Flat and
-- columnar rather than a document, because this is the one workload that
-- aggregates across rows.
--
-- Nothing updates or deletes a row here; the application only inserts. See the
-- grants at the foot of this file, which enforce that at the role level rather
-- than by convention.

CREATE TABLE IF NOT EXISTS usage_records (
    -- Identity of the call.
    ts                       TIMESTAMPTZ  NOT NULL,
    request_id               TEXT         NOT NULL,
    trace_id                 TEXT         NOT NULL,

    -- Ownership and attribution. Every one of these comes from the verified
    -- token, never from anything the agent reported about itself.
    tenant                   TEXT         NOT NULL,
    team                     TEXT         NOT NULL,
    agent_id                 TEXT         NOT NULL,
    agent_identity           TEXT         NOT NULL,
    agent_version            TEXT         NOT NULL,
    env                      TEXT         NOT NULL,
    cost_center              TEXT         NOT NULL,

    -- What was called.
    logical_model            TEXT         NOT NULL,
    pool                     TEXT         NOT NULL,
    provider                 TEXT         NOT NULL,
    backend_model            TEXT         NOT NULL,
    operation                TEXT         NOT NULL,

    -- Consumption.
    input_tokens             BIGINT       NOT NULL,
    output_tokens            BIGINT       NOT NULL,
    cached_tokens            BIGINT       NOT NULL DEFAULT 0,

    -- Money. NUMERIC, not DOUBLE PRECISION: a chargeback figure that does not
    -- reconcile to the cent because of binary floating point is a dispute
    -- nobody can win.
    unit_cost_input_per_1m   NUMERIC(18, 6) NOT NULL DEFAULT 0,
    unit_cost_output_per_1m  NUMERIC(18, 6) NOT NULL DEFAULT 0,
    cost_usd                 NUMERIC(18, 8) NOT NULL DEFAULT 0,
    savings_usd              NUMERIC(18, 8) NOT NULL DEFAULT 0,

    -- Behaviour.
    cache                    TEXT         NOT NULL DEFAULT '',
    attempts                 INTEGER      NOT NULL DEFAULT 0,
    billable                 BOOLEAN      NOT NULL DEFAULT TRUE,
    estimated                BOOLEAN      NOT NULL DEFAULT FALSE,
    unpriced                 BOOLEAN      NOT NULL DEFAULT FALSE,

    -- Outcome.
    status_code              INTEGER      NOT NULL DEFAULT 0,
    error_code               TEXT         NOT NULL DEFAULT '',
    duration_ms              DOUBLE PRECISION NOT NULL DEFAULT 0,
    ttft_ms                  DOUBLE PRECISION NOT NULL DEFAULT 0,

    -- The gateway retries its own write on a transient failure, so a duplicate
    -- (request_id, ts) is possible and must be idempotent rather than doubly
    -- charged. request_id alone is not the key: a caller may supply its own
    -- request id, and two callers may supply the same one.
    PRIMARY KEY (request_id, ts)
);

COMMENT ON TABLE  usage_records IS
    'Immutable per-request usage and cost. Append-only; the application role holds INSERT and SELECT only.';
COMMENT ON COLUMN usage_records.cost_center IS
    'The chargeback key, from the token. UNATTRIBUTED marks a request that reached metering without one, which is a defect made visible rather than a rounding error hidden.';
COMMENT ON COLUMN usage_records.billable IS
    'FALSE for a cache hit. A non-billable record carries its avoided cost in savings_usd instead of cost_usd.';
COMMENT ON COLUMN usage_records.estimated IS
    'TRUE when the provider did not report usage and the gateway counted the completion itself. Reconciliation treats these separately; they are never charged as if they had been measured.';
COMMENT ON COLUMN usage_records.unpriced IS
    'TRUE when the backend had no price entry. The record is zero-cost and marked, rather than silently free.';
COMMENT ON COLUMN usage_records.cached_tokens IS
    'The provider''s own prompt-cache hit count, distinct from the gateway response cache and billed at a different rate.';

-- The chargeback report groups by day and cost centre, so that is the leading
-- index. Postgres reads it backwards for descending scans, so one index serves
-- both directions.
CREATE INDEX IF NOT EXISTS usage_records_cc_ts_idx    ON usage_records (cost_center, ts);
-- Per-agent drill-down from the fleet view.
CREATE INDEX IF NOT EXISTS usage_records_agent_ts_idx ON usage_records (agent_id, ts);
-- The anomaly detector scans a team's recent spend; the fleet view sums a
-- team's month to date.
CREATE INDEX IF NOT EXISTS usage_records_team_ts_idx  ON usage_records (env, team, ts);
-- Retention deletes by age, and the monthly export scans by age.
CREATE INDEX IF NOT EXISTS usage_records_ts_idx       ON usage_records (ts);
-- Reconciliation against the provider's own invoice is per backend per day.
-- Partial: only priced, billable rows appear on an invoice, and excluding the
-- rest keeps the index small on a table dominated by cache hits.
CREATE INDEX IF NOT EXISTS usage_records_backend_ts_idx
    ON usage_records (backend_model, ts)
    WHERE billable AND NOT unpriced;

-- ---------------------------------------------------------------------------
-- Chargeback: usage_daily
-- ---------------------------------------------------------------------------
--
-- The daily rollup the chargeback report and the monthly export read. It is a
-- view rather than a materialised table so that a late-arriving record — the
-- usage sink drains asynchronously and a record can land after midnight —
-- corrects the rollup instead of being missed by it.
--
-- Grouped to match cost.Record.Key() plus the day bucket, so a row here is the
-- SQL equivalent of a cost.Rollup with period='day'.
--
-- If this becomes slow at fleet scale, the change is a materialised view
-- refreshed on a schedule with a lookback window wide enough to absorb late
-- arrivals. Do not reach for that before measuring: the composite index above
-- serves a month of one cost centre comfortably.

CREATE OR REPLACE VIEW usage_daily AS
SELECT
    date_trunc('day', ts)                        AS day,
    env,
    tenant,
    team,
    agent_id,
    cost_center,
    backend_model,
    count(*)                                     AS requests,
    sum(input_tokens)                            AS input_tokens,
    sum(output_tokens)                           AS output_tokens,
    sum(cached_tokens)                           AS cached_tokens,
    sum(cost_usd)                                AS cost_usd,
    sum(savings_usd)                             AS savings_usd,
    count(*) FILTER (WHERE NOT billable)         AS cache_hits,
    count(*) FILTER (WHERE error_code <> '')     AS errors,
    -- Surfaced so a chargeback figure can be qualified rather than quietly
    -- averaged with measured data. A cost centre whose spend is largely
    -- estimated or unpriced needs to know that before it disputes the bill.
    count(*) FILTER (WHERE estimated)            AS estimated_records,
    count(*) FILTER (WHERE unpriced)             AS unpriced_records
FROM usage_records
GROUP BY 1, 2, 3, 4, 5, 6, 7;

COMMENT ON VIEW usage_daily IS
    'Daily chargeback rollup. A view, not a materialised table, so late-arriving usage records correct the rollup rather than being missed by it.';

-- ---------------------------------------------------------------------------
-- Roles and grants
-- ---------------------------------------------------------------------------
--
-- Three roles, least privilege, split by what each service actually does:
--
--   agentgate_app       control plane. Reads and writes the registry. Inserts
--                       usage records. Cannot UPDATE or DELETE a usage record,
--                       so "immutable" is enforced by the database rather than
--                       promised by the code.
--   agentgate_readonly  fleet service, dashboards, ad-hoc analysis. SELECT
--                       only, everywhere.
--   agentgate_migrator  owns the schema. Used only by the migration step, never
--                       by a running service.
--
-- Roles are NOLOGIN group roles. Real login users are granted membership and
-- are created outside this file, because their credentials come from the vault
-- and must not appear in a repository.
--
-- Adapt the two managed-service notes below for your platform:
--   * Azure Database for PostgreSQL: grant membership to the workload's Entra
--     principal rather than creating a password login.
--   * Amazon RDS: use `rds_iam` membership on the login user so the service
--     authenticates with an IAM token.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentgate_migrator') THEN
        CREATE ROLE agentgate_migrator NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentgate_app') THEN
        CREATE ROLE agentgate_app NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'agentgate_readonly') THEN
        CREATE ROLE agentgate_readonly NOLOGIN;
    END IF;
END
$$;

-- Nobody gets to create objects in public by default.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT  USAGE  ON SCHEMA public TO agentgate_app, agentgate_readonly, agentgate_migrator;
GRANT  CREATE ON SCHEMA public TO agentgate_migrator;

-- Registry: the control plane owns these records for their whole lifecycle.
GRANT SELECT, INSERT, UPDATE, DELETE
    ON agents, credentials, attestations
    TO agentgate_app;

-- Promotions and waivers are audit evidence. The application inserts and
-- updates state and approvals; it may not delete. Removing evidence requires
-- the migrator role and a change record.
GRANT SELECT, INSERT, UPDATE
    ON promotions, waivers
    TO agentgate_app;

-- Usage records are immutable. INSERT and SELECT, nothing else. This is the
-- grant that makes "append-only" a property of the database rather than a
-- convention in the code.
GRANT SELECT, INSERT
    ON usage_records
    TO agentgate_app;

GRANT SELECT ON usage_daily TO agentgate_app;

-- The fleet service and every analyst read, and only read.
GRANT SELECT
    ON agents, promotions, waivers, attestations, usage_records, usage_daily
    TO agentgate_readonly;

-- Deliberately withheld from agentgate_readonly: the credentials table. It
-- holds secret hashes and vault pointers. A read-only analytics role has no
-- business seeing either, and a dashboard query that accidentally joins to it
-- should fail rather than succeed.

-- Anything the migrator creates later inherits the same shape.
ALTER DEFAULT PRIVILEGES FOR ROLE agentgate_migrator IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE ON TABLES TO agentgate_app;
ALTER DEFAULT PRIVILEGES FOR ROLE agentgate_migrator IN SCHEMA public
    GRANT SELECT ON TABLES TO agentgate_readonly;

COMMIT;
