# bgp-api

Public IP-to-BGP/allocation/geofeed lookup API for `bgp-api.mehrnet.com`.
The service is a Go HTTP server using `pgx` and PostgreSQL. Cloudflare is used
as a proxied DNS, TLS, WAF, and optional edge-cache layer; it does not run the
application or store the database.

The public static frontend for `https://bgp.mehrnet.com` is maintained in
[`mehrnet/bgp`](https://github.com/mehrnet/bgp).

## Quick Install

The unattended installer has two explicit deployment modes:

| Mode | Bootstrap | Hosts | Free space | Best use |
| --- | --- | --- | --- | --- |
| `docker` | Pre-indexed PostgreSQL image | Linux amd64 | 12 GiB | Fastest installation and simplest operation |
| `bare` | Logical snapshot restored into host PostgreSQL | Linux amd64/arm64 with systemd | 18 GiB | Host-managed PostgreSQL and native systemd service |

Docker mode is the default, but production commands should include `--mode`
so their intent is unambiguous. Both modes verify release checksums, bind the
API only to `127.0.0.1:3102`, and use sequential patches for daily updates.
Root access is required. Ports 80 and 443 are required only for Caddy setup.

### Localhost only

Docker mode downloads the pre-indexed database and starts it directly:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --mode docker
```

Bare mode installs PostgreSQL, imports and indexes the release snapshot, then
installs the static API binary and systemd service:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --mode bare
```

Check it immediately:

```sh
curl http://127.0.0.1:3102/v1/health
curl 'http://127.0.0.1:3102/v1/ip?query=1.1.1.1'
```

### Domain and HTTPS

Point the domain's DNS record to the server first. The domain variant installs
Caddy, obtains TLS certificates, keeps the API bound to localhost, and creates
a private origin token shared by Caddy and the API:

Choose either mode and pass the API hostname:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --mode docker --domain bgp-api.example.com
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --mode bare --domain bgp-api.example.com
```

The Caddy setup supports direct traffic and Cloudflare-proxied DNS. Keep TCP
ports 80 and 443 reachable so Caddy can provision and renew certificates.
Add `--auto-update` to either command to install the daily update job at exactly
06:00 UTC:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --mode docker --domain bgp-api.example.com --auto-update
```

### Update

The installer remembers its mode, so the short command is sufficient:

```sh
sudo /srv/bgp-api/install.sh --update
```

Automation may state the mode explicitly and will fail safely if it does not
match the installed deployment:

```sh
sudo /srv/bgp-api/install.sh --mode docker --update
sudo /srv/bgp-api/install.sh --mode bare --update
```

Normal updates verify the new release, apply every sequential PostgreSQL patch,
and update the matching API image or static binary. Docker mode does not
recreate the active PostgreSQL container or download another full database
image.

For manual scheduling, open root's crontab with `sudo crontab -e` and add these
lines:

```cron
CRON_TZ=UTC
0 6 * * * /srv/bgp-api/install.sh --mode docker --update >> /var/log/mehrnet-bgp-api-update.log 2>&1
```

The `--auto-update` option writes the equivalent schedule to
`/etc/cron.d/mehrnet-bgp-api-update` and enables the host's cron service.

### Uninstall

This permanently removes the selected deployment's API and database data,
automatic-update job, managed Caddy configuration, and `/srv/bgp-api`. The mode
is normally detected from the installation:

```sh
sudo /srv/bgp-api/install.sh --uninstall
```

For an older installation that does not have the flag yet, run the current
installer directly:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash -s -- --uninstall
```

Use `--mode docker --uninstall` or `--mode bare --uninstall` to require a mode
match. The uninstall is non-interactive. It deliberately leaves Docker,
PostgreSQL, and Caddy packages installed because other applications may depend
on them.

Useful operational commands:

```sh
cd /srv/bgp-api
sudo docker compose --env-file .env -f docker-compose.yml ps
sudo docker compose --env-file .env -f docker-compose.yml logs -f api
sudo docker compose --env-file .env -f docker-compose.yml restart api
journalctl -u bgp-api -f
```

Do not use `docker compose down` or remove the PostgreSQL container. Its
writable layer contains the patches applied after the original pre-indexed
image was published.

## Run From Source

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

To build one static binary locally:

```sh
go build -o bin/bgp-api ./cmd/bgp-api
```

## Bare-Metal PostgreSQL Sync

The producer builds one PostgreSQL dataset directly from parsed RIR and geofeed
inputs. It writes a logical snapshot with a separately executable index phase,
then builds those indexes in the same physical database used by the Docker
image. The dataset contains `lookup_prefixes`, normalized
allocation/route/aut-num tables, and generated `range_summaries`.

```sh
export DATABASE_URL='postgres://bgp_api:change-me@127.0.0.1:5432/bgp_api'
./scripts/sync-postgres.sh
```

The default synchronizer mode is `patch`. It locates every published release
after the active release, verifies each `postgres-patch-manifest.json` and its
SHA-256-checked patch asset, then applies those patches strictly in order to
the active schema. A patch is accepted only when its `base_release` exactly
matches the locally active release. Existing PostgreSQL indexes are updated
incrementally; no second schema or full indexed dataset is created.

After the final patch, the synchronizer installs the matching
`bgp-api-linux-$arch` binary and restarts `bgp-api`. If a patch fails, its SQL
transaction rolls back and dataset metadata is not advanced. An unavailable or
incompatible chain fails closed rather than silently using disk for a full
import.

Use `BGP_API_SYNC_MODE=snapshot` only to bootstrap a new database or recover
from a missing patch chain. Snapshot mode imports the latest release into a
versioned `bgp_YYYYMMDD_HHMM` schema, validates it, atomically repoints the
stable public views, and removes the old schema after activation. It needs
temporary disk for both datasets.

It requires `curl`, `jq`, `gzip`, `install`, `psql`, `sha256sum`, `tar`, and
`uname`. The public GitHub API is sufficient for its daily run; set
`BGP_API_GITHUB_TOKEN` only to raise the GitHub API rate limit. Set
`BGP_API_SYNC_BINARY=0` to disable binary installation, `BGP_API_BINARY_PATH` to
change the install path, `BGP_API_SERVICE_NAME` to change the restarted service,
or `BGP_API_SYNC_MODE=snapshot` for explicit bootstrap/recovery.

Run it after the GitHub producer build with a lock to prevent overlapping
imports. Keep `DATABASE_URL` in a root-readable environment file. The
recommended production schedule is the systemd timer in `deploy/`, which runs
daily at 06:00 UTC, after the 04:00 UTC producer run, and writes sync output to
journald. It compares the latest published GitHub release tag with the active
dataset, verifies that each patch release timestamp is not later than the
server's NTP-synchronized UTC clock, and applies the patch chain in order.

```sh
sudo cp deploy/bgp-api-postgres-sync.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now bgp-api-postgres-sync.timer
```

## Docker Deployment

Docker is the fast bootstrap route. Each release publishes digest-pinned amd64
API, updater, and pre-indexed PostgreSQL images in
`docker-deployment-manifest.json`. PostgreSQL starts directly from the
pre-indexed data directory in its pinned image, so bootstrap does not need a
second full-database volume copy. This route is suitable for small disks, but
the PostgreSQL container must not be removed: its writable layer holds logical
patches applied after the image's initial release.
It is intentionally an amd64 image because PostgreSQL data directories are
architecture-specific. The Dockerfiles also pin their Debian/Go/PostgreSQL base
image digests, so rebuilds are reproducible. The host still supplies the Linux
kernel and does not need a matching distribution release.

Copy the Compose files and create a private environment file from the example.
Set the three image values from the release deployment manifest and set a unique
origin token. The GHCR packages must be public for anonymous pulls; otherwise
authenticate the host with `docker login ghcr.io` before deploying.

```sh
cp .env.docker.example .env
docker compose up -d
```

The Compose file exposes only the API on `127.0.0.1:3102`; PostgreSQL has no
host port and is reachable only on its internal Compose network. Its bootstrap
cluster uses trust authentication inside that private network, so do not attach
untrusted containers to it or publish the PostgreSQL service.

Run the Docker synchronizer after a producer release. It verifies the release
manifest and checksums, applies the same sequential logical patch chain to the
existing PostgreSQL container, then recreates only the API container using the
new digest-pinned image. It never recreates PostgreSQL during a normal update.

```sh
./scripts/sync-docker.sh
```

For a daily host cron job at 06:00 UTC:

```cron
CRON_TZ=UTC
0 6 * * * cd /srv/bgp-api && ./scripts/sync-docker.sh >> /var/log/bgp-api-docker-sync.log 2>&1
```

Use `docker compose stop` or `docker compose restart` for this stack. Do not
run `docker compose down`, `docker compose rm postgres`, or force-recreate the
PostgreSQL service: those discard patches and require another bootstrap from
the pre-indexed image. Use the bare-metal deployment when a separately managed
PostgreSQL data directory is required.

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

- `mehrnet_bgp_postgres.sql.gz`: self-contained PostgreSQL logical snapshot.
  A normal `psql` restore loads the data and creates every lookup index. Patch
  generation sets `skip_indexes=1` to restore only schema and data.
- `mehrnet_bgp_postgres_indexes.sql`: the small, standalone index phase used by
  the logical snapshot and available for manual schema/data-only restores.
- `mehrnet_bgp_postgres.patch.<base-release>.sql.gz`: logical PostgreSQL delta
  from the named PostgreSQL release. It is a `psql` script containing `COPY`
  staging data and set-based deletions/inserts, so it updates existing indexes.
- `postgres-patch-manifest.json`: patch format and exact base/target release
  contract. A patch may only be applied when the local active release matches
  `base_release` exactly.
- `bgp-api-linux-amd64.tar.gz`: static Linux amd64 API server binary.
- `bgp-api-linux-arm64.tar.gz`: static Linux arm64 API server binary.
- `SHA256SUMS.txt`: hashes for every release asset.

Assets over GitHub's release limit are split into numbered `.part-*` files.
Download every part and concatenate them in order before extracting or
decompressing the asset. The synchronizer applies patches sequentially when
available and uses the self-contained logical snapshot only for bootstrap or
recovery. The PostgreSQL Docker image carries the physical pre-indexed database
and does not rebuild indexes during startup.

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
