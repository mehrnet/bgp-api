# bgp-api

Public IP-to-BGP/allocation/geofeed lookup API for `bgp-api.mehrnet.com`.
The service is a Go HTTP server using `pgx` and PostgreSQL. Cloudflare is used
as a proxied DNS, TLS, WAF, and optional edge-cache layer; it does not run the
application or store the database.

The public static frontend for `https://bgp.mehrnet.com` is maintained in
[`mehrnet/bgp`](https://github.com/mehrnet/bgp).

## Run the API

```sh
export DATABASE_URL='postgres://bgp_api:change-me@127.0.0.1:5432/bgp_api'
export CORS_ALLOWED_ORIGINS_JSON='["https://your-frontend.example"]'
go run ./cmd/bgp-api
```

`LISTEN_ADDR` defaults to `127.0.0.1:3102`; `POSTGRES_MAX_CONNECTIONS`
defaults to `8`. Set `ORIGIN_AUTH_TOKEN` only when an upstream proxy injects the same
`X-BGP-API-Origin-Token` header. The API provides `GET /v1/ip?query=:ip`,
`GET /v1/me`, `GET /v1/prefix?prefix=:cidr`,
`GET /v1/range?start=:ip&end=:ip&kind=allocations|routes`,
`GET /v1/asn?query=:asn`, and `GET /v1/health`.
Resource endpoints accept `limit` (1-100) and the `cursor` returned by a
previous response; responses contain `next_cursor` when a following page exists.
`GET /v1/asn` additionally accepts `page` (1-100000) for numbered route
pagination. This mode returns `routes.page`, `routes.total_pages`, and
`routes.total_items`; do not combine `page` with `cursor`.
For a canonical IPv4 range from `/0` through `/16`, `/v1/range` returns a
generated `mode: "summary"` response instead of enumerating objects. The
summary is built with the daily dataset and includes bounded `/0`, `/8`, or
`/16` bucket totals plus top country and ASN record facets. Non-CIDR ranges,
IPv6 ranges, and narrower IPv4 ranges return `mode: "records"` with normal
cursor pagination.
Successful lookup/resource responses include `meta.dataset` with the active
release tag, producer build timestamp, activation timestamp, and producer
source commit. `/v1/health` exposes the same dataset object.
`/v1/health` also exposes `build.version`, `build.commit`, and
`build.built_at` for the running API binary, so binary and dataset provenance
can be checked independently.
`GET /v1/me` has the identical lookup
response schema and uses the Cloudflare client address only when the incoming
connection is from a published Cloudflare address range; otherwise it uses the
raw peer address. When Cloudflare Pseudo IPv4 overwrites the standard client
header, the API prefers Cloudflare's preserved IPv6 client header.

Every API response is derived exclusively from the locally imported daily
dataset. The API makes no runtime requests to RIRs, RDAP, RIPEstat, or any
other upstream service.

For production, build one static binary locally:

```sh
go build -o bin/bgp-api ./cmd/bgp-api
```

## PostgreSQL Dataset Sync

The producer publishes `mehrnet_bgp_postgres.sql.gz`, a compressed PostgreSQL
`COPY` snapshot, alongside static Linux API binaries built from the same source
commit. The dump includes `lookup_prefixes` for low-latency point lookups plus
normalized `allocation_objects`, `route_objects`, and `autnums` tables for
range, CIDR, and ASN resources, plus generated `range_summaries` for broad
IPv4 range resources.

```sh
export DATABASE_URL='postgres://bgp_api:change-me@127.0.0.1:5432/bgp_api'
./scripts/sync-postgres.sh
```

The synchronizer uses one PostgreSQL database. It imports the release into a
versioned `bgp_YYYYMMDD_HHMM` schema, validates it, atomically repoints the
stable `public.lookup_prefixes`, `public.allocation_objects`,
`public.route_objects`, `public.autnums`, and `public.range_summaries` views, records the active dataset
metadata in `public.bgp_api_dataset`, then removes the old schema. After a
successful dataset swap, it installs the matching `bgp-api-linux-$arch` binary
when the release provides one and restarts `bgp-api`. If import or validation
fails, the existing public views and running API binary are left untouched.

It requires `curl`, `jq`, `gzip`, `install`, `psql`, `sha256sum`, `tar`, and
`uname`. The public GitHub API is sufficient for its daily run; set
`BGP_API_GITHUB_TOKEN` only to raise the GitHub API rate limit. Set
`BGP_API_SYNC_BINARY=0` to disable binary installation, `BGP_API_BINARY_PATH` to
change the install path, or `BGP_API_SERVICE_NAME` to change the restarted
systemd service.

Run it after the GitHub producer build with a lock to prevent overlapping
imports. Keep `DATABASE_URL` in a root-readable environment file. The
recommended production schedule is the systemd timer in `deploy/`, which runs
daily at 06:00 UTC, after the 04:00 UTC producer run, and writes sync output to
journald. It compares the latest published GitHub release tag with the active
dataset and verifies that the release timestamp is not later than the server's
NTP-synchronized UTC clock before importing.

```sh
sudo cp deploy/bgp-api-postgres-sync.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now bgp-api-postgres-sync.timer
```

## Cloudflare

Enable the orange-cloud proxy for `bgp-api.mehrnet.com`. Configure a WAF rate
limit on `GET /v1/ip`, and optionally cache successful lookup responses with
a TTL shorter than the daily data-update window. Protect the Go origin with a
firewall and Authenticated Origin Pulls.

The production unit and reverse-proxy templates are in `deploy/`. The Caddy
proxy injects the origin token, so the Go process remains inaccessible without
the proxy header even on the loopback interface.

## Monitoring

Use systemd's journal as the source of truth for API and sync logs. The API
logs to stdout/stderr under `bgp-api.service`; scheduled syncs run under
`bgp-api-postgres-sync.service`.

```sh
journalctl -u bgp-api -f
journalctl -u bgp-api-postgres-sync.service --no-pager
systemctl list-timers bgp-api-postgres-sync.timer
systemctl status bgp-api
```

## Producer Releases

GitHub Actions runs daily at 04:00 UTC and supports manual dispatch. It only
builds and publishes release assets. Each release contains:

- `mehrnet_bgp.tar.gz`: normal SQLite database.
- `mehrnet_bgp_indexed.tar.gz`: SQLite database with lookup indexes.
- `mehrnet_bgp_postgres.sql.gz`: PostgreSQL snapshot for the next versioned schema.
- `mehrnet_bgp_postgres.patch.<base-release>.sql.gz`: logical PostgreSQL delta
  from the named indexed-dataset release. It is a `psql` script containing
  `COPY` staging data and set-based deletions/inserts, so it updates existing
  PostgreSQL indexes instead of rebuilding them.
- `postgres-patch-manifest.json`: patch format and exact base/target release
  contract. A patch may only be applied when the local active release matches
  `base_release` exactly.
- `bgp-api-linux-amd64.tar.gz`: static Linux amd64 API server binary.
- `bgp-api-linux-arm64.tar.gz`: static Linux arm64 API server binary.
- `SHA256SUMS.txt`: hashes for every release asset.

Assets over GitHub's release limit are split into numbered `.part-*` files.
Download every part and concatenate them in order before extracting or
decompressing the asset. The current synchronizer still uses the full snapshot;
patch application is deliberately introduced separately after the patch assets
have been validated in a release.

Run the Go test suite with:

```sh
go test ./...
```

The response contract and source-data limitations are documented in
[docs/data-contract.md](docs/data-contract.md).

## Retained Data

The daily producer keeps the API response compact while preserving useful
source fields for advanced lookups:

- allocation range, RIR, country, netname, status, allocation date, created and
  last-modified timestamps
- route prefix, origin ASN, route source, maintainer, organization, and
  description
- aut-num name, organization, country/status, source, maintainer, and
  description
- direct RPSL `abuse-c` or `abuse-mailbox` values when they exist on the source
  object
- geofeed country, region, and city

The API intentionally does not add currency, language, timezone, proxy/VPN, or
security reputation fields because those would require separate commercial or
runtime data sources and would make the response less focused on BGP.
