# ADR 0015 — Store registry records as JSON documents with promoted query columns

**Status:** Accepted
**Date:** 2026-08-26
**Deciders:** Platform lead, client data architecture, client audit liaison
**Consulted:** SRE, the client's DBA function, two consuming teams
**Affects:** `internal/registry/sqlstore.go`, `internal/registry/store.go`, `deploy/sql/schema.sql`,
the promotion audit trail, `docs/09-migration.md`

---

## Context

The registry holds the agent inventory: who owns each agent, what versions exist, which environment
each version is allowed in, and the evidence behind every promotion. It is the join key for the
whole platform — telemetry attribution, quota, chargeback and the promotion gate all resolve
through it.

Four properties of the data shape the decision.

**It is deeply nested and it is read as a whole.** A `registry.Agent` carries an `Owner`, a `Quota`,
two pool lists, a label map, and an array of `Version` records — each of which carries a
`GateResult`, which carries an array of `GateCheck`. A `PromotionRequest` carries a `GateResult` and
an array of `Approval`. Almost every read wants the entire structure: the fleet view renders it, the
gate evaluates against it, the CLI prints it.

**The nested shape changes.** The promotion gate has eight automated checks today. It had five when
the platform was designed and it will have more: a data-residency check, a model-allowlist check, a
dependency-freshness check are all foreseeable and none is scheduled. Each new check adds a field
to a structure inside an array inside an array.

**The gate snapshot is audit evidence.** `Version.LastGate` and `PromotionRequest.Gate` exist so
that "why was this version allowed into production" is answerable months later. An auditor asking
that question is entitled to see what was actually evaluated, not a re-rendering of it.

**The query patterns are narrow and known.** Get an agent by id. Get an agent by identity URI. List
a tenant's or a team's agents. List an agent's promotions, newest first. Find a credential by client
id. List an agent's waivers. That is the complete set, and it has not grown since the first
iteration.

There is also a second implementation to keep honest. `MemoryStore` — file-backed, used for local
development and every unit test — satisfies the same `registry.Store` interface as `SQLStore`. Two
implementations of the same interface must produce the same answers, or the tests stop being
evidence about production behaviour.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | A gate gaining a check must not require a database migration | Decisive |
| 2 | Gate snapshots must be stored verbatim and be reconstructible exactly | Decisive |
| 3 | The memory store and the SQL store must have identical semantics | High |
| 4 | The known query patterns must be served by an index, not a scan | High |
| 5 | Ad-hoc analytical SQL across the registry | Low |
| 6 | Database-enforced referential integrity | Low |

Drivers 5 and 6 are weighted low deliberately, and that weighting is the substance of this
decision. It is argued in Consequences rather than assumed here.

## Options Considered

### Option A — Fully normalised relational schema

Tables for `agents`, `agent_versions`, `gate_results`, `gate_checks`, `approvals`, `waivers`,
`credentials`, `attestations`, with foreign keys between them.

| Pros | Cons |
|---|---|
| Ad-hoc SQL across any nesting level: "which agents have a waived `security_review` gate" is a `WHERE` clause | **Every new gate check is a migration**, in an environment where a production migration is a change record and a review |
| Referential integrity enforced by the database | A gate snapshot is reassembled by join on read, so what the auditor sees is a **reconstruction**, not a recording |
| Familiar to any DBA; the client's data architecture function would recognise it immediately | Reading one agent is a six-way join for data that is always read whole |
| Efficient partial updates | The Go structs and the tables drift, and an ORM or a hand-written mapper becomes a permanent maintenance surface |
| Column-level constraints and types | The memory store cannot reproduce join semantics, so the two implementations diverge — driver 3 fails |

Rejected on drivers 1, 2 and 3. Driver 2 is the one that settles it: an audit record that is
reassembled on read is a record of the current schema's interpretation of the past, not a record of
the past.

### Option B — JSON documents with query keys promoted to indexed columns

One row per record. A `doc` column holding the serialised Go struct, plus real columns for exactly
the keys that are queried: `agent_id`, `identity`, `tenant`, `team` on `agents`; `agent_id`,
`state`, `requested_at` on `promotions`; and so on.

| Pros | Cons |
|---|---|
| A new gate check is a code change and nothing else | **No ad-hoc SQL across nested structures.** Cross-agent questions about gate contents are application-side scans |
| The gate snapshot is stored **byte-for-byte as evaluated** and read back the same way | **No database-enforced referential integrity** between agents, promotions, credentials and waivers |
| Every known query is a promoted column and an index | The document is versioned by the application; the read path must tolerate documents written by the previous release |
| The memory store serialises the same struct, so semantics are identical by construction | A DBA cannot answer a question about the data without reading Go code |
| One serialisation format across both stores and the HTTP API | Partial updates are read-modify-write |

### Option C — JSONB with GIN indexes and expression indexes into the document

Option B, but use PostgreSQL's `jsonb` type and index into the document rather than promoting
columns.

| Pros | Cons |
|---|---|
| Ad-hoc SQL across nested structures becomes possible after all | **`MemoryStore` cannot implement `jsonb` containment queries**, so the two stores diverge exactly where driver 3 forbids it |
| No migration for a new field, and it is still queryable | Binds the platform to PostgreSQL, which the `Store` interface exists to avoid |
| Expression indexes give precise coverage where needed | A query written against the JSON structure is a schema dependency that no type checker enforces — the worst of both worlds |
| | GIN index maintenance cost on a write-heavy path, for queries nobody has asked for yet |

Rejected, but it is the closest alternative and it is the first thing to reach for if driver 5 ever
gains weight. Note that it is **available incrementally**: the `doc` column can be migrated from
`TEXT` to `JSONB` and expression-indexed without changing the application, as long as the
application keeps not depending on it.

### Option D — A document database

MongoDB, Cosmos DB, or similar.

| Pros | Cons |
|---|---|
| The document model natively | A second database technology in an estate that already runs PostgreSQL for everything else |
| Native nested querying and indexing | A new platform component to review, operate, back up, patch and page on |
| | Transactional guarantees vary by product and configuration, and the promotion flow needs them |
| | The client's managed PostgreSQL is already approved, provisioned and monitored |

Rejected quickly. The operational and review cost of a second datastore is not remotely repaid by a
storage model that PostgreSQL serves adequately.

## Decision

**Registry records are stored as one JSON document per row, with the keys the platform actually
queries promoted to real, indexed columns alongside.**

| Table | Promoted columns | What stays in the document |
|---|---|---|
| `agents` | `agent_id` (PK), `identity` (unique), `tenant`, `team`, `created_at`, `updated_at` | Owner, quota, requested and granted pools, every version, every gate snapshot, labels |
| `promotions` | `id` (PK), `agent_id`, `state`, `requested_at` | The gate result verbatim, every approval with actor and timestamp, the change reference |
| `credentials` | `client_id` (PK), `agent_id`, `created_at` | Secret hash, vault version, expiry, retired flag |
| `waivers` | `id` (PK), `agent_id`, `gate`, `expires_at` | Reason, granting actor, granted timestamp |
| `attestations` | `agent_id`, `env` (composite PK), `attestation`, `observed_at` | — fully columnar; there is no document to keep |

Specifics that make it implementable:

- **`tenant` and `team` are derived from the identity URI on write**, not stored twice by the
  caller. The service enforces that `owner.team` inside the document matches, and refuses a change
  of owning team as a conflict rather than an update.
- **Filters that the promoted columns cannot serve — `env`, `state`, `q` — are applied in the
  application**, identically for both store implementations. That is what keeps driver 3 satisfied,
  and it is also why a filter change never needs a migration.
- **`doc` is `TEXT`, not `JSONB`.** The application never queries into it, and typing it as `JSONB`
  would invite exactly the coupling this decision avoids. The migration to `JSONB` remains available
  and is a single `ALTER COLUMN`.
- **Foreign keys are deliberately absent.** The relationships are enforced in the service layer, and
  their absence means an `agents` row can be restored independently of its promotion history during
  a recovery.
- **`usage_records` is explicitly not part of this decision.** Chargeback is an
  aggregate-across-rows workload — the exact thing the document model is bad at — so it is a flat,
  columnar, append-only table. Choosing one model for the registry does not mean choosing it for
  everything.

### The audit implication

Two properties follow, and both are load-bearing rather than incidental.

**Gate snapshots are stored verbatim.** `GateResult` is serialised as evaluated, including every
`GateCheck` with its status, its `observed` and `required` values, and any waiver id. When an
auditor asks why version 2.4.1 was allowed into production in August, what comes back is the
document that was written in August, not a rendering of it through today's schema. A gate that has
since gained a ninth check does not retroactively acquire a ninth row in an old snapshot — the old
snapshot has eight, which is the truth.

**The promotion trail is append-only.** `PromotionRequest` rows are inserted and updated as state
and approvals accumulate; they are never deleted. `deploy/sql/schema.sql` enforces this at the role
level rather than by convention: `agentgate_app` holds `SELECT`, `INSERT` and `UPDATE` on
`promotions` and `waivers`, and no `DELETE`. Removing evidence requires the migrator role and a
change record, which is the friction it should have. `waivers` are retained after expiry for the
same reason: an expired exception is still the record that an exception was granted, by whom, and
why.

## Consequences

### Positive

- **A new gate check ships as a code change.** No migration, no change record for the database, no
  window. That directly shortens the loop between "we should also check X before production" and
  "we check X before production", which is the loop this platform exists to make short.
- **The audit answer is a recording, not a reconstruction.** This is the property that would be
  hardest to retrofit and the easiest to lose without noticing.
- **The two store implementations cannot drift.** Both serialise the same Go structs and both apply
  the same application-side filters. A registry test passing against `MemoryStore` is evidence about
  `SQLStore`, which is what makes the test suite worth running.
- **Every known query is served by an index.** Six access patterns, six indexes, no scans.
- **One serialisation format everywhere.** The document in the database, the value in the memory
  store's snapshot file, and the body on the HTTP API are the same JSON. There is no mapping layer
  to keep in step and no place for a field to be silently dropped.
- **Recovery is simpler.** A row is self-contained. Restoring one agent does not require restoring
  a consistent set of six joined tables.

### Negative

- **No ad-hoc SQL across nested structures.** "Which agents currently have a waived
  `security_review` gate", "how many promotions failed on `telemetry_healthy` this quarter", "which
  versions were approved by a given person" are all application-side scans rather than `WHERE`
  clauses. Today those questions are answered by `agentctl promotions` and by the fleet view; when
  someone wants one this ADR did not anticipate, they will write Go rather than SQL, and they will
  be annoyed about it.
- **No database-enforced referential integrity.** A promotion row can name an `agent_id` that no
  longer exists. Nothing at the database level prevents it; only the service layer does, and a
  future bug there produces orphans that no constraint will catch. This is a real reduction in
  safety and it is accepted knowingly.
- **A DBA cannot answer questions about the data.** Understanding what is in `doc` requires reading
  `internal/registry/model.go`. In an estate where the DBA function is a genuine operational
  resource, that removes them from the loop.
- **Schema evolution is an application concern with no compiler support.** The read path must
  tolerate documents written by the previous release, because a rolling deployment runs both at once
  and a rollback runs the old binary against documents the new one wrote. `encoding/json` ignores
  unknown fields and zero-values missing ones, which handles the additive case silently — but a
  renamed field will silently read as empty rather than failing, and there is no type checker that
  will catch it. **Adding a field is safe; renaming or repurposing one is a two-release migration.**
- **Partial updates are read-modify-write.** Recording one approval rewrites the whole promotion
  document. Concurrent approvals on the same promotion are a last-writer-wins race. The window is
  small and the two-party rule makes a collision unlikely, but it is a real race and it is not
  guarded by a version column today.
- **Document size is unbounded in principle.** An agent with hundreds of versions, each carrying a
  gate snapshot, produces a large row that is read whole on every access. Nothing prunes it. At
  current fleet sizes this is theoretical; at ten times the size it becomes a retention question.

### Neutral

- Storage overhead from repeating JSON keys on every row is real and immaterial at this data
  volume. PostgreSQL's TOAST compression handles the larger documents without configuration.
- The promoted columns duplicate values that also appear inside the document. They are written from
  the same struct in the same statement, so they cannot disagree.
- This decision says nothing about the usage and chargeback tables, which are normalised precisely
  because their workload is different.

### What this forecloses

- **Reporting tools pointed straight at the registry database.** A BI tool cannot usefully read
  `doc`. Reporting goes through the fleet API, which is the intended path anyway.
- **Database-level constraints on registry invariants** — that a version's environment is one of
  three, that a quota is positive, that an approval names a known role. All of those are Go
  validation today. Getting them into the database means Option A.
- Getting either back costs the normalisation work in Option A plus a migration of existing
  documents. It is bounded but not small, and the audit property in driver 2 would have to be
  preserved some other way — most plausibly by keeping the gate snapshot as a document even in a
  normalised schema, which is an admission that this decision was right for that part at least.

## Revisit when

1. **Three or more distinct people ask a cross-agent question about document contents within one
   quarter.** That is driver 5 gaining weight with evidence rather than by assertion. The response
   is Option C, incrementally: migrate `doc` to `JSONB` and add expression indexes for the specific
   questions, keeping the application's own queries on the promoted columns so driver 3 still holds.
2. **A production incident is caused by orphaned rows** that a foreign key would have prevented.
   One occurrence is a service-layer bug to fix; a second is evidence that driver 6 was
   underweighted.
3. **The agents table exceeds roughly ten thousand rows, or a single document exceeds a megabyte.**
   Either means reading an agent whole has stopped being cheap, and versions should be split into
   their own table — at which point the gate snapshot stays a document inside that table, because
   driver 2 does not change.
4. **Concurrent approvals collide in production.** The fix is small — an optimistic-concurrency
   version column on `agents` and `promotions` — and it should be done the first time it happens,
   not preemptively.
5. **The `Store` interface acquires a query method the promoted columns cannot serve** without an
   application-side scan over the whole table. That is the point at which the storage model is
   fighting the access pattern rather than fitting it.
