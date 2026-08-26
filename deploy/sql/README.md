# PostgreSQL schema

`schema.sql` is the authoritative DDL for AgentGate's relational state: the agent registry, the
promotion audit trail, client credential records, attestations, and the chargeback usage table and
its daily rollup view.

Target: **PostgreSQL 14 or later.** Verified against 16.

---

## 1. Applying migrations

There are two paths, and they are not alternatives — they are layers.

### The application path

`SQLStore.Migrate` in `internal/registry/sqlstore.go` creates the five registry tables and their
indexes with `CREATE TABLE IF NOT EXISTS`. It runs on every control-plane start and is idempotent,
which keeps a fresh environment one command away from working. It is what makes
`make run-stack` produce a functioning platform without a separate migration step.

It creates **only** the registry tables. It does not create `usage_records`, the `usage_daily`
view, the comments, or any role or grant, because the application does not own role management and
must not be running as a principal that could.

### The operator path

`schema.sql` is applied by a deployment job, before the services start, by a principal holding the
`agentgate_migrator` role:

```sh
psql "$AGENTGATE_MIGRATOR_DSN" -v ON_ERROR_STOP=1 -f deploy/sql/schema.sql
```

Every statement is idempotent — `CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`,
`CREATE OR REPLACE VIEW`, role creation guarded by a `pg_roles` lookup — so running it twice is a
no-op and running it against an existing database is safe. The whole file is one transaction: it
either applies completely or not at all.

The two paths agree by construction: `schema.sql` carries the registry statements verbatim from
`Migrate`, and the file is a strict superset. If you change one, change the other in the same pull
request, and verify with:

```sh
# Apply the Go statements to one database and schema.sql to another, then diff the catalogs.
psql "$A" -c "\d+ agents"   > /tmp/a.txt
psql "$B" -c "\d+ agents"   > /tmp/b.txt
diff /tmp/a.txt /tmp/b.txt
```

### Adding a migration

There is no migration framework and no version table. That is a deliberate consequence of the
storage model (ADR 0015): the registry documents evolve in the application, not in the schema, so
schema changes are rare and are almost always additive index or column work.

When you do need one:

1. Add the statement to `schema.sql`, guarded so it is idempotent.
2. Add the same statement to `Migrate` if it touches a registry table the application creates.
3. Make it **online**: `CREATE INDEX CONCURRENTLY` for a new index on a large table (note that
   `CONCURRENTLY` cannot run inside the file's transaction — put it in a separate, clearly labelled
   script), `ADD COLUMN ... DEFAULT` without a table rewrite on 14+, never `ALTER COLUMN TYPE` on
   `usage_records`.
4. Make it **backwards compatible for one release**. The old binary must keep working against the
   new schema, because a rolling deployment runs both at once and a rollback runs the old one
   against the new schema for as long as it takes to notice.

---

## 2. Rollback

**The schema is designed so that a rollback is a code rollback, not a database rollback.** That is
the whole strategy, and it works because every schema change is additive.

| Change | Roll back by |
|---|---|
| New index | `DROP INDEX CONCURRENTLY`. Safe at any time; the old binary never knew about it. |
| New nullable column | Leave it. An unused column costs nothing and dropping it is the risky operation, not keeping it. |
| New table (`usage_records`, a future rollup) | Leave it. Revoke the application's grants if the table must be inert. |
| New document field inside `doc` | Nothing to do. The previous release's `encoding/json` ignores unknown fields; that is why the storage model was chosen. |
| Removed or retyped column | **Not a rollback. This is a two-release migration**: release N writes both, release N+1 reads only the new one, release N+2 drops the old. If you find yourself needing to roll one back mid-flight, you have skipped a release. |

Point-in-time recovery is the backstop, not the plan: restoring a managed PostgreSQL to a timestamp
loses every write since that timestamp, including promotion approvals and usage records that will
not be re-emitted. Use it for corruption, not for a bad deployment.

Two things a restore must be reasoned about carefully:

* **Usage records are not idempotent across a restore.** The primary key is `(request_id, ts)`, so
  re-delivery from the usage stream after a restore is de-duplicated. Records the stream has
  already dropped are lost and the affected period must be reconciled against the provider invoice
  rather than assumed correct.
* **Promotion state is not the same as deployment state.** A restore that rolls the registry back
  past a promotion leaves an agent running in production whose version the registry now says is
  only in staging. Its next token refresh will fail with `invalid_grant`. After any restore, run
  `agentctl agents` against the restored registry and compare it against what is actually deployed
  before letting tokens expire.

---

## 3. Retention

Two different regimes, because two different things are being kept.

| Table | Retention | Rationale |
|---|---|---|
| `agents` | **Indefinite.** Never purged. | The agent record is the join key for every historical telemetry and chargeback record. Deleting it orphans everything that ever referred to it. A decommissioned agent is retired by state, not by deletion. |
| `promotions` | **Indefinite.** Append-and-update, never delete. | Audit evidence. The verbatim gate snapshot and the two recorded approvals are the answer to "why was this version allowed into production", asked by an auditor months or years later. The application role holds no `DELETE` on this table. |
| `waivers` | **Indefinite**, including expired ones. | An expired waiver is still the record of an exception that was granted, by whom, and why. Expiry stops it having effect; it does not stop it being evidence. |
| `attestations` | One row per (agent, env), overwritten in place. | Only the most recent observation is an input to the gate. History of attestation method is carried by the promotion snapshots. |
| `credentials` | Retained while the agent exists; a retired credential's row is kept. | Answers "which secret was in use on the day of the incident". Holds only hashes and vault pointers, so retention costs nothing in secret exposure. |
| `usage_records` | **Rolled up, then aged out. 400 days.** | The raw per-request rows are needed to reconcile against a provider invoice and to investigate a cost anomaly. Beyond a full year plus a reconciliation margin, the `usage_daily` rollup carries everything a chargeback dispute needs and the raw rows are a storage bill with no reader. |
| `usage_daily` | A view, so it retains exactly as long as `usage_records`. | If a longer chargeback history is required, materialise the rollup into its own table on a schedule **before** ageing out the raw rows — not after. |

The 400-day figure is 13 months: a full financial year plus a month of margin, so that a
year-end reconciliation can always reach the raw records for every month it covers.

### The retention job

Run it daily, off-peak, as `agentgate_migrator` — the application role deliberately holds no
`DELETE` on `usage_records`, and this is exactly why:

```sql
-- Delete in bounded batches. One unbounded DELETE against a year of usage
-- takes a lock and a lot of WAL, and will be killed by the statement timeout
-- somewhere in the middle, having done half the work and none of the vacuum.
DO $$
DECLARE
    deleted INTEGER;
BEGIN
    LOOP
        DELETE FROM usage_records
        WHERE ctid IN (
            SELECT ctid FROM usage_records
            WHERE ts < now() - INTERVAL '400 days'
            LIMIT 10000
        );
        GET DIAGNOSTICS deleted = ROW_COUNT;
        EXIT WHEN deleted = 0;
        COMMIT;
    END LOOP;
END
$$;
```

If `usage_records` grows past roughly a hundred million rows, replace the batched delete with
**monthly range partitioning on `ts`**. Dropping a partition is instant and generates no WAL, where
deleting the same rows generates a great deal of both. Partitioning is not the starting point
because it complicates the primary key and the local development story for a table that most
deployments will never grow that large.

### Before anything is deleted

Confirm the monthly chargeback export for the affected period has been produced and accepted. A
usage record deleted before its month has been billed is a month that cannot be billed. The export
is the precondition for the retention job, not a parallel activity.
