#!/bin/sh
# Runs once, on first initialisation of the postgres volume.
#
# The control plane and fleetview share the `agentgate` database (created by
# POSTGRES_DB) but own separate schemas, so a fleetview migration can never
# alter the registry tables that the promotion gate audits. Langfuse gets its
# own database because it runs its own migrations on a schedule we do not
# control.
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-'SQL'
    CREATE SCHEMA IF NOT EXISTS controlplane;
    CREATE SCHEMA IF NOT EXISTS fleetview;

    -- pgcrypto: gen_random_uuid() for record ids and digest() for the
    -- content-hash columns on cached/usage records.
    CREATE EXTENSION IF NOT EXISTS pgcrypto;
    -- pg_stat_statements: query-level attribution when a chargeback rollup
    -- starts costing more than the traffic it accounts for.
    CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

    COMMENT ON SCHEMA controlplane IS 'Agent registry, promotion records, token exchange audit (SPEC 1.3/1.4)';
    COMMENT ON SCHEMA fleetview IS 'Fleet inventory, SLO state, usage rollups (SPEC 4.5/6)';
SQL

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<-'SQL'
    SELECT 'CREATE DATABASE langfuse'
    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'langfuse')\gexec
SQL
