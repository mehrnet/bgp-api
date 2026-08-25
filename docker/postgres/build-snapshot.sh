#!/usr/bin/env bash
set -euo pipefail

: "${POSTGRES_SCHEMA:?POSTGRES_SCHEMA is required}"

initdb --pgdata="$PGDATA" --auth-local=trust --auth-host=trust
pg_ctl -D "$PGDATA" -o "-c listen_addresses=127.0.0.1" -w start
psql --username postgres --dbname postgres --set ON_ERROR_STOP=1 <<'SQL'
CREATE ROLE bgp_api LOGIN;
CREATE DATABASE bgp_api OWNER postgres;
SQL
gzip -dc /tmp/dataset.sql.gz | psql --username postgres --dbname bgp_api --set ON_ERROR_STOP=1 --quiet
psql --username postgres --dbname bgp_api --set ON_ERROR_STOP=1 --set "dataset_schema=$POSTGRES_SCHEMA" --file /tmp/bootstrap.sql
pg_ctl -D "$PGDATA" -m fast -w stop
