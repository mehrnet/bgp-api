# bgp-api

Public IP-to-BGP/allocation/geofeed lookup API for `bgp-api.mehrnet.com`.
The same Hono routes run as a Cloudflare Worker with D1 or as a local Bun
process over the producer's SQLite database. Drizzle owns the database schema
and query boundary in both runtimes.

## Local development

```sh
bun install
LOCAL_DB_PATH=../mehrnet_bgp.db bun run db:build-prefix-index -- ../mehrnet_bgp.db
LOCAL_DB_PATH=../mehrnet_bgp.db bun run dev
curl http://127.0.0.1:3102/v1/ip/1.1.1.1
bun run check
```

The current database stores IPv4 keys as IPv4-mapped IPv6 decimal values. The
lookup layer deliberately preserves that producer format; do not change it
without rebuilding the source database.

`/v1/health` returns the active D1 target (`primary` or `secondary`). Public IP
lookups use Cloudflare's native fixed-window `RL_IP_LOOKUP` binding at 120
requests per minute per client IP; Bun development intentionally uses no limit.

## Cloudflare preparation

No deployment is performed by this project. Before the first Worker deploy:

1. Create both D1 databases: `bunx wrangler d1 create bgp-api-primary` and
   `bunx wrangler d1 create bgp-api-secondary`.
2. Replace both placeholder `database_id` values in `wrangler.jsonc` with the
   returned IDs.
3. Add `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` repository secrets.
4. Run **Build and Publish BGP Database**. It downloads all five RIR source
   datasets itself, then builds, indexes, publishes, and rolls out the data.
5. The workflow stages new data in secondary, deploys against it, rebuilds
   primary, switches back to primary, then refreshes secondary as standby.

The database build runs entirely inside GitHub Actions and performs the
blue/green D1 promotion described above.

Every successful import also creates a GitHub release with both database
distributions: `mehrnet_bgp.tar.gz` is the normal source database and
`mehrnet_bgp_indexed.tar.gz` contains the range and prefix indexes used by the
API. `SHA256SUMS.txt` verifies the downloads. GitHub requires each release
asset to be below 2 GiB, so an oversized indexed archive is automatically split
into numbered parts.

The response contract and the current data limitations are documented in
[docs/data-contract.md](docs/data-contract.md).
