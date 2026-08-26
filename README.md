# bgp-api

Public IP-to-BGP, allocation, ASN, and geofeed lookup API for
`bgp-api.mehrnet.com`. The Go server reads one immutable, pre-indexed bbolt
file at a time. Domain deployments keep a second immutable slot available
during updates. Releases also include a pre-indexed SQLite database for third-party users
and offline inspection. The API does not build indexes, run migrations, or
contact upstream services while answering requests.

The public frontend is maintained in
[`mehrnet/bgp`](https://github.com/mehrnet/bgp).

## Install

The unattended installer supports Linux `amd64` and `arm64` hosts running
systemd. It installs a statically linked API binary and the latest verified
database, then binds the primary service to `127.0.0.1:3102`. Go is not
required.

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --mode bare
```

To configure Caddy and HTTPS, point a DNS record at the server first:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- \
  --mode bare --domain bgp-api.example.com
```

Add `--auto-update` to either install command to check for a new verified
release every day at exactly 06:00 UTC.

Check the service:

```sh
active_slot="$(cat /var/lib/bgp-api/active-slot 2>/dev/null || printf primary)"
api_port=3102
[ "$active_slot" = secondary ] && api_port=3103
curl "http://127.0.0.1:$api_port/v1/health"
curl "http://127.0.0.1:$api_port/v1/ip?query=1.1.1.1"
```

## Update

Updates download the immutable database and matching binary, verify both
against `SHA256SUMS.txt`, and validate the new database before activation. On
domain deployments with an active Caddy proxy, the updater starts the new
database in the inactive slot (`127.0.0.1:3102` or `127.0.0.1:3103`), checks its
health and dataset tag, then performs a graceful Caddy reload. Only after the
switch does it drain and stop the old service and remove its database. Public
requests therefore keep flowing while the immutable file changes.
This removes the planned maintenance window; it cannot protect against a host,
kernel, Caddy, or network failure.

If Caddy is not available, blue/green mode is disabled automatically. If the
host cannot hold both database files, or `BGP_API_BLUE_GREEN=0` is set, the
updater stops both slots and replaces the primary file with a short maintenance
window. The inactive slot is always disposable; no database indexes are built
on the server.

```sh
sudo /srv/bgp-api/install.sh --update
```

For a manual root crontab (`sudo crontab -e`):

```cron
CRON_TZ=UTC
0 6 * * * /usr/bin/systemctl start bgp-api-sync.service
```

The sync start time, finish time, release tag, and elapsed seconds are written
to the systemd journal:

```sh
journalctl -u bgp-api-sync
journalctl -u bgp-api -f
```

## Uninstall

This removes the API service, database, automatic update job, installed binary,
and managed Caddy configuration. It leaves shared system packages installed.

```sh
sudo /srv/bgp-api/install.sh --uninstall
```

The current installer can also remove an installation when its local copy is
unavailable:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --uninstall
```

## API

All resource endpoints are `GET` requests:

| Endpoint | Purpose |
| --- | --- |
| `/v1/ip?query=1.1.1.1` | Most-specific route, allocation, and geofeed lookup |
| `/v1/ip?query=1.1.1.1&details=full` | IP lookup with matching source records |
| `/v1/me` | Lookup for the client address seen by the API |
| `/v1/prefix?prefix=1.1.1.0/24` | Records overlapping a CIDR prefix |
| `/v1/range?start=1.1.1.0&end=1.1.1.255` | Records overlapping an inclusive range |
| `/v1/asn?query=AS13335` | ASN identity and registered routes |
| `/v1/health` | Service, binary, and active dataset metadata |

Prefix, range, and ASN record responses support `limit` from 1 through 100 and
the returned `cursor`. ASN routes also support numbered `page` pagination; do
not combine `page` and `cursor`.

Canonical IPv4 ranges from `/0` through `/16` use precomputed summary records.
Narrow IPv4 ranges, IPv6 ranges, and non-CIDR ranges return source records with
pagination. Record scans are concurrency-limited and reject unexpectedly broad
queries instead of exhausting a small host.

Every response comes from the release dataset. The API never performs live
RIR, RDAP, geolocation, or routing requests.

## Configuration

The installed service reads `/etc/bgp-api/bgp-api.env`. Relevant variables are:

| Variable | Default | Purpose |
| --- | --- | --- |
| `BGP_API_DATABASE_PATH` | `/var/lib/bgp-api/primary.bbolt` | Backward-compatible database variable |
| `BGP_API_PRIMARY_DATABASE_PATH` | `/var/lib/bgp-api/primary.bbolt` | Primary immutable slot |
| `BGP_API_SECONDARY_DATABASE_PATH` | `/var/lib/bgp-api/secondary.bbolt` | Secondary immutable slot |
| `LISTEN_ADDR` | `127.0.0.1:3102` | Primary-slot HTTP listener |
| `BGP_API_BLUE_GREEN` | `auto` | Update mode: `auto`, `1`, or `0` |
| `CORS_ALLOWED_ORIGINS_JSON` | empty | JSON array of browser origins |
| `ORIGIN_AUTH_TOKEN` | empty | Required proxy-to-origin token |
| `TRUSTED_PROXY_CIDRS` | loopback | Peers allowed to supply client-IP headers |
| `GOMAXPROCS` | Go default | Runtime CPU limit |
| `GOMEMLIMIT` | Go default | Soft Go heap limit |

The installer sets `GOMAXPROCS=2` and `GOMEMLIMIT=384MiB`, which targets a
2-vCPU, 2-GiB server while leaving memory for the kernel page cache. The
database is memory-mapped read-only; cached database pages are reclaimable and
are not Go heap allocations.

The primary and secondary systemd services use ports `3102` and `3103`. Only
the active slot is enabled at boot; the updater changes that enablement when it
switches slots. `active-slot` in the data directory records which slot Caddy
serves.

## Run From Source

```sh
export BGP_API_DATABASE_PATH="$PWD/mehrnet_bgp.bbolt"
export CORS_ALLOWED_ORIGINS_JSON='["https://bgp.mehrnet.com"]'
go run ./cmd/bgp-api
```

Build a static binary:

```sh
CGO_ENABLED=0 go build -trimpath -o bin/bgp-api ./cmd/bgp-api
```

Validate a database without starting the HTTP listener:

```sh
BGP_API_DATABASE_PATH="$PWD/mehrnet_bgp.bbolt" \
BGP_API_VALIDATE_ONLY=1 ./bin/bgp-api
```

## Dataset Production

The daily workflow runs at 04:00 UTC and can also be dispatched manually. Five
RIR parsers run as a matrix, geofeeds are fetched once, and the final database
job runs a second matrix for the release formats:

| Format | Asset | Purpose |
| --- | --- | --- |
| bbolt | `mehrnet_bgp_bbolt.tar.zst` | Production API runtime |
| SQLite | `mehrnet_bgp_sqlite.tar.zst` | Third-party users and offline inspection |

Static Linux binaries build in parallel with dataset preparation.

Each release contains:

- `mehrnet_bgp_bbolt.tar.zst`
- `mehrnet_bgp_sqlite.tar.zst`
- `bgp-api-linux-amd64.tar.gz`
- `bgp-api-linux-arm64.tar.gz`
- `SHA256SUMS.txt`

Build the same database locally from `final_data/`:

```sh
go run ./cmd/build-bbolt-dataset \
  -input final_data \
  -output mehrnet_bgp.bbolt \
  -release-tag local \
  -built-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -source-commit "$(git rev-parse HEAD)"
```

Build the SQLite release database from the same `final_data/` directory:

```sh
go run ./cmd/build-sqlite-dataset \
  -input final_data \
  -output mehrnet_bgp.db \
  -release-tag local \
  -built-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -source-commit "$(git rev-parse HEAD)"
go run ./cmd/build-prefix-index mehrnet_bgp.db
go run ./cmd/build-range-summaries mehrnet_bgp.db
go run ./cmd/validate-database -indexed mehrnet_bgp.db
```

The SQLite asset is shipped ready to query; consumers do not need to rebuild
indexes or range summaries.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

The response contract is documented in [`docs/data-contract.md`](docs/data-contract.md).
The frontend's complete public contract is published as
[`openapi.json`](https://bgp.mehrnet.com/openapi.json).
