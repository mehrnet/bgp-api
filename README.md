# MehrNet BGP API

Read-only IP intelligence backed by a pre-indexed [bbolt](https://github.com/etcd-io/bbolt) dataset.
It combines RIR allocations, BGP routes, ASN records, and geofeed locations in one API.

- Live API: [`bgp-api.mehrnet.com`](https://bgp-api.mehrnet.com)
- Web interface: [`bgp.mehrnet.com`](https://bgp.mehrnet.com)
- Response contract: [`docs/data-contract.md`](docs/data-contract.md)
- OpenAPI: [`openapi.json`](https://bgp.mehrnet.com/openapi.json)

The server only reads the immutable release file. It does not build indexes or call
RIR, RDAP, routing, or geolocation services while handling a request.

## Install

### Requirements

- Linux with systemd
- `amd64` or `arm64`
- Root access
- An active release database plus temporary space for a second copy during updates

Go and a compiler are not required.

### Local service

Bare-metal installation binds the API to `127.0.0.1:3102`:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | sudo bash
```

### HTTPS with Caddy

Create a DNS record for the server, then pass the hostname. Caddy is installed and
managed automatically:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | \
  sudo bash -s -- --domain bgp-api.example.com
```

Add `--auto-update` to schedule a verified release check at **06:00 UTC**:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | \
  sudo bash -s -- --domain bgp-api.example.com --auto-update
```

The installer downloads the matching static API binary and bbolt dataset from the
latest GitHub release. It verifies both with `SHA256SUMS.txt` before starting the service.

A public installation stores its hostname in a root-only reinstallation record. After
`--uninstall`, running the normal install command again restores that hostname and its
Caddy vhost automatically; a fresh origin token is generated when needed. The record
never contains the token or dataset. Use `--local-only` to intentionally install only
the loopback service instead.

## Operations

### Check status

```sh
systemctl status bgp-api.service
curl http://127.0.0.1:3102/v1/health
```

Domain deployments serve the active slot through Caddy. The active listener is recorded
in `/var/lib/bgp-api/active-slot` (`primary` on port `3102`, `secondary` on port `3103`).

### Update

```sh
sudo /srv/bgp-api/install.sh --update
```

Updates use the inactive slot when Caddy is configured:

1. Download and verify the release.
2. Validate the database and binary.
3. Release optional caches in the active slot if memory is constrained.
4. Fully warm the inactive slot before it opens its listener, then check its health.
5. Atomically move Caddy to the warmed slot, retaining the old slot as a retry target during its drain grace period.
6. Stop the old slot and remove its database.

This keeps planned dataset updates available without rebuilding indexes on the server.
If Caddy is unavailable, disk space is insufficient, or `BGP_API_BLUE_GREEN=0` is set,
the updater falls back to stop-and-replace mode.

Before staging, the updater records `MemAvailable` and the active process RSS. If
available memory is below its reserve, it first verifies the active slot's authenticated
health capability and then sends a compatible process `SIGUSR2`:
it drops only its optional selector and serialized-response caches, while continuing
to answer from bbolt. This creates room for the staged process without a traffic
cutover. The replacement then completes its selected cache warmup before it accepts
traffic, while the old process continues to answer. Only after that health check does
Caddy move to the new slot; the prior slot remains a retry target through the drain
period. Kernel page cache is left to Linux to reclaim; the updater never uses global
`drop_caches`.

Release downloads and archive extraction run at idle I/O priority and `nice 19`.
The staged slot also receives the minimum systemd CPU and I/O weight while it
warms. This keeps the active read-only API ahead of all multi-gigabyte staging
work on small hosts.

### Logs and scheduled updates

```sh
journalctl -u bgp-api -f
journalctl -u bgp-api-sync --since today
```

### Benchmark production paths

The benchmark keeps connections alive and measures the private loopback origin, direct-origin
HTTPS, and public HTTPS through Cloudflare from an independent host. Direct-origin HTTPS keeps
the hostname for TLS SNI but connects to `BGP_API_ORIGIN_IP`, bypassing Cloudflare only for the
measurement. It is a development operation, not a runtime dependency; Go is needed only on the
machine that runs the script.

```sh
BGP_API_ORIGIN_HOST=root@78.31.250.179 \
BGP_API_EXTERNAL_HOST=root@138.199.236.120 \
./scripts/benchmark-production.sh
```

It warms the compact IP cache before each measurement. Override `BGP_API_BENCH_REQUESTS`,
`BGP_API_BENCH_CONCURRENCY`, or `BGP_API_BENCH_WARMUP` to change the workload.

For a manually managed cron schedule:

```cron
CRON_TZ=UTC
0 6 * * * /usr/bin/systemctl start bgp-api-sync.service
```

### Uninstall

The command removes the API services, databases, update schedule, binary, and managed
Caddy configuration. Shared packages such as Caddy remain installed. It retains only
the prior public hostname, so a normal subsequent install restores the Caddy deployment
without requiring `--domain` again.

```sh
sudo /srv/bgp-api/install.sh --uninstall
```

To remove that reinstallation record as well, use:

```sh
sudo /srv/bgp-api/install.sh --uninstall --purge
```

If the local installer is unavailable:

```sh
curl -fsSL https://raw.githubusercontent.com/mehrnet/bgp-api/main/install.sh | \
  sudo bash -s -- --uninstall
```

## API

All endpoints are `GET` requests and return JSON.

| Endpoint | Use |
| --- | --- |
| `/v1/ip?query=1.1.1.1` | Most-specific route, allocation, and location |
| `/v1/ip?query=1.1.1.1&details=full` | IP lookup with matching source records |
| `/v1/prefix?prefix=1.1.1.0/24` | Allocation and route records for a CIDR |
| `/v1/range?start=1.1.1.0&end=1.1.1.255` | Records overlapping an inclusive range |
| `/v1/asn?query=AS13335` | ASN identity and registered routes |
| `/v1/health` | Runtime and dataset metadata |

Paginated endpoints accept:

| Parameter | Applies to | Notes |
| --- | --- | --- |
| `limit` | prefix, range, ASN | `1`-`100`; default is server-defined |
| `cursor` | prefix, ASN | Continue the previous result set |
| `cursor` | range | Opaque cursor returned by the preceding range response; pass it unchanged |
| `page` | ASN | Numbered pagination; do not combine with `cursor` |
| `details=full` | IP | Include matching allocation, route, and geofeed records |
| `kind=allocations\|routes` | range | Select range record types |

Canonical IPv4 `/0` through `/16` range requests use one precomputed summary per
prefix. Narrow ranges, IPv6 ranges, and non-CIDR ranges return source records with
pagination.

Example:

```sh
curl -sS 'https://bgp-api.mehrnet.com/v1/ip?query=1.1.1.1' | jq
curl -sS 'https://bgp-api.mehrnet.com/v1/asn?query=AS13335&limit=20' | jq
```

See [`docs/data-contract.md`](docs/data-contract.md) for stable response objects and
the public [OpenAPI document](https://bgp.mehrnet.com/openapi.json) for generated
client documentation.

## Configuration

The installer writes `/etc/bgp-api/bgp-api.env`:

| Variable | Default | Purpose |
| --- | --- | --- |
| `BGP_API_DATABASE_PATH` | `/var/lib/bgp-api/primary.bbolt` | Active database path |
| `BGP_API_PRIMARY_DATABASE_PATH` | `/var/lib/bgp-api/primary.bbolt` | Primary update slot |
| `BGP_API_SECONDARY_DATABASE_PATH` | `/var/lib/bgp-api/secondary.bbolt` | Secondary update slot |
| `LISTEN_ADDR` | `127.0.0.1:3102` | HTTP listener for this service |
| `BGP_API_BLUE_GREEN` | `auto` | `auto`, `1`, or `0` |
| `BGP_API_STARTUP_TIMEOUT_SECONDS` | `300` | Maximum wait for a staged API slot to become healthy |
| `BGP_API_CACHE_STRATEGY` | `auto` | `auto`, `minimal`, `balanced`, or `full` |
| `BGP_API_DEFER_CACHE_WARMUP` | `0` | Start serving before cache warmup; the updater uses this internally |
| `BGP_API_BLOCK_UNTIL_CACHE_WARMUP` | `0` | Finish the selected warmup before opening the listener; the updater uses this for the staged slot |
| `BGP_API_RUNTIME_CACHE_CONTROL` | `1` | Enable `SIGUSR2` release of optional in-process caches for a memory-constrained staged update |
| `BGP_API_STAGE_MEMORY_RESERVE_MIB` | `768` | Minimum `MemAvailable` before the updater asks the active slot to release optional caches |
| `BGP_API_DRAIN_GRACE_SECONDS` | `45` | Keep the old backend running after the Caddy switch so stale proxy connections can finish |
| `CORS_ALLOWED_ORIGINS_JSON` | empty | JSON array of browser origins |
| `ORIGIN_AUTH_TOKEN` | empty | Proxy-to-origin authentication token |
| `BGP_API_COMPACT_CACHE_MIB` | strategy default | Override the in-process cache budget for default IP responses |
| `BGP_API_RESOURCE_CACHE_MIB` | strategy default | Override the in-process cache budget for prefix, range, and ASN responses |
| `GOMAXPROCS` | Go default | CPU limit |
| `GOMEMLIMIT` | Go default | Soft Go heap limit |

`auto` selects the safest profile from the deployed bbolt file size and `MemTotal`:

| Profile | Runtime behavior |
| --- | --- |
| `minimal` | `64 MiB` compact cache, `16 MiB` resource cache, no dataset preload |
| `balanced` | `256 MiB` compact cache, `64 MiB` resource cache, and the roughly `508 MiB` compact selector copy |
| `full` | `128 MiB` compact cache, `32 MiB` resource cache, then a sequential warm of the immutable bbolt file through the Linux page cache |

`full` is not a second Go copy of the database. It needs the dataset, response-cache
budget, and a `768 MiB` operating-system reserve to fit at once. With the current
roughly `3.6 GiB` dataset, a 4 GiB host does **not** qualify; `auto` selects `balanced`
there. A 5 GiB-or-larger host can usually use `full`, while bbolt still lets Linux reclaim
pages under pressure.

On ordinary startup the active server begins serving before its selected cache warms. During a
blue-green update, however, the staged slot warms before its listener opens, while the current
slot remains live. Caddy makes the warmed slot primary and retains the old slot as a retry target
for the drain period. This avoids exposing the one-time selector copy to public requests.

## Releases and datasets

The daily GitHub Actions workflow runs at 04:00 UTC and supports manual dispatch. RIR
parsers run in a matrix; release formats and static binaries are built independently.

| Asset | Purpose |
| --- | --- |
| `mehrnet_bgp_bbolt.tar.zst` | Ready-to-query production dataset |
| `mehrnet_bgp_sqlite.tar.zst` | Optional SQLite export for third-party users |
| `bgp-api-linux-amd64.tar.gz` | Static Linux `amd64` binary |
| `bgp-api-linux-arm64.tar.gz` | Static Linux `arm64` binary |
| `SHA256SUMS.txt` | Asset integrity checksums |

The API runtime uses bbolt. SQLite is a separate export and is not required for serving
requests.

## Build from source

Go 1.22 or newer is required for local builds.

Run the API against a local dataset:

```sh
export BGP_API_DATABASE_PATH="$PWD/mehrnet_bgp.bbolt"
go run ./cmd/bgp-api
```

Build a static binary:

```sh
CGO_ENABLED=0 go build -trimpath -o bin/bgp-api ./cmd/bgp-api
```

Build a bbolt dataset from producer output:

```sh
go run ./cmd/build-bbolt-dataset \
  -input final_data \
  -output mehrnet_bgp.bbolt \
  -release-tag local \
  -built-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -source-commit "$(git rev-parse HEAD)"
```

The optional SQLite export is built from the same `final_data/` directory:

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

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```
