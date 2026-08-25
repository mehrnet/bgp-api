#!/usr/bin/env bash
set -euo pipefail

: "${POSTGRES_SCHEMA:?POSTGRES_SCHEMA is required}"

initdb --pgdata="$PGDATA" --auth-local=trust --auth-host=trust
printf '%s\n' 'host all all 0.0.0.0/0 trust' 'host all all ::0/0 trust' >> "$PGDATA/pg_hba.conf"
# The marker block is removed before the final image is written. The database
# therefore starts with PostgreSQL's normal durable settings at runtime.
cat >> "$PGDATA/postgresql.conf" <<'CONF'
# MehrNet snapshot build settings begin
listen_addresses = '127.0.0.1'
fsync = off
synchronous_commit = off
full_page_writes = off
autovacuum = off
checkpoint_timeout = '30min'
max_wal_size = '8GB'
min_wal_size = '2GB'
maintenance_work_mem = '1GB'
# MehrNet snapshot build settings end
CONF
pg_ctl -D "$PGDATA" -w start
psql --username postgres --dbname postgres --tuples-only --no-align <<'SQL'
SHOW fsync;
SHOW autovacuum;
SHOW max_wal_size;
SHOW checkpoint_timeout;
SHOW maintenance_work_mem;
SQL
psql --username postgres --dbname postgres --set ON_ERROR_STOP=1 <<'SQL'
CREATE ROLE bgp_api LOGIN;
CREATE DATABASE bgp_api OWNER postgres;
SQL
gzip -dc /tmp/dataset.sql.gz | psql --username postgres --dbname bgp_api --set ON_ERROR_STOP=1 --quiet
psql --username postgres --dbname bgp_api --set ON_ERROR_STOP=1 --set "dataset_schema=$POSTGRES_SCHEMA" --file /tmp/bootstrap.sql
sed -i '/^# MehrNet snapshot build settings begin$/,/^# MehrNet snapshot build settings end$/d' "$PGDATA/postgresql.conf"
pg_ctl -D "$PGDATA" -m fast -w stop
