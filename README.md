# bgp-api

Public IP-to-BGP/allocation/geofeed lookup API for `bgp-api.mehrnet.com`.
The service is a Go HTTP server using `pgx` and PostgreSQL. Cloudflare is used
as a proxied DNS, TLS, WAF, and optional edge-cache layer; it does not run the
application or store the database.

## Run the API

```sh
export DATABASE_URL='postgres://bgp_api:change-me@127.0.0.1:5432/bgp_api'
export CORS_ALLOWED_ORIGINS_JSON='["https://your-frontend.example"]'
go run ./cmd/bgp-api
```

`LISTEN_ADDR` defaults to `127.0.0.1:3102`; `POSTGRES_MAX_CONNECTIONS`
defaults to `8`. Set `ORIGIN_AUTH_TOKEN` only when an upstream proxy injects the same
`X-BGP-API-Origin-Token` header. The API provides `GET /v1/ip/:ip` and
`GET /v1/health`.

For production, build one static binary:

```sh
go build -o bin/bgp-api ./cmd/bgp-api
```

## PostgreSQL Dataset Sync

The producer publishes `mehrnet_bgp_postgres.sql.gz`, a compressed PostgreSQL
`COPY` snapshot of `lookup_prefixes`, the only table queried by the API.

```sh
export DATABASE_URL='postgres://bgp_api:change-me@127.0.0.1:5432/bgp_api'
./scripts/sync-postgres.sh
```

The synchronizer uses one PostgreSQL database. It imports the release into a
versioned `bgp_YYYYMMDD_HHMM` schema, validates it, atomically repoints the
stable `public.lookup_prefixes` view, then removes the old schema. The Go API
keeps serving through the swap without a restart.

It requires `curl`, `jq`, `gzip`, `psql`, and `sha256sum`. The public GitHub
API is sufficient for its daily run; set `BGP_API_GITHUB_TOKEN` only to raise
the GitHub API rate limit.

Run it after the GitHub producer build with a lock to prevent overlapping
imports. Keep `DATABASE_URL` in a root-readable environment file.

```cron
0 5 * * * flock -n /var/lock/bgp-api-postgres-sync.lock sh -lc 'set -a; . /etc/bgp-api/postgres.env; set +a; cd /srv/bgp-api && ./scripts/sync-postgres.sh' >> /var/log/bgp-api-postgres-sync.log 2>&1
```

## Cloudflare

Enable the orange-cloud proxy for `bgp-api.mehrnet.com`. Configure a WAF rate
limit on `GET /v1/ip/*`, and optionally cache successful lookup responses with
a TTL shorter than the daily data-update window. Protect the Go origin with a
firewall and Authenticated Origin Pulls.

The production unit and reverse-proxy templates are in `deploy/`. The Caddy
proxy injects the origin token, so the Go process remains inaccessible without
the proxy header even on the loopback interface.

## Producer Releases

GitHub Actions runs daily at 04:00 UTC and supports manual dispatch. It only
builds and publishes release assets. Each release contains:

- `mehrnet_bgp.tar.gz`: normal SQLite database.
- `mehrnet_bgp_indexed.tar.gz`: SQLite database with lookup indexes.
- `mehrnet_bgp_postgres.sql.gz`: PostgreSQL snapshot for the next versioned schema.
- `SHA256SUMS.txt`: hashes for every release asset.

Assets over GitHub's release limit are split into numbered `.part-*` files.
Download every part and concatenate them in order before extracting or
decompressing the asset.

Run the Go test suite with:

```sh
go test ./...
```

The response contract and source-data limitations are documented in
[docs/data-contract.md](docs/data-contract.md).
