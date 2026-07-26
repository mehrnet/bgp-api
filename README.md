# bgp-api

Public IP-to-BGP/allocation/geofeed lookup API for `bgp-api.mehrnet.com`.
The same Hono routes run as a Cloudflare Worker with D1 or as a local Bun
process over the producer's SQLite database. Drizzle owns the database schema
and query boundary in both runtimes.

## Local development

```sh
bun install
LOCAL_DB_PATH=../mehrnet_bgp.db bun run dev
curl http://127.0.0.1:3102/v1/ip/1.1.1.1
bun run check
```

The current database stores IPv4 keys as IPv4-mapped IPv6 decimal values. The
lookup layer deliberately preserves that producer format; do not change it
without rebuilding the source database.

## Cloudflare preparation

No deployment is performed by this project. Before the first Worker deploy:

1. Create the D1 database: `bunx wrangler d1 create bgp-api`.
2. Replace the placeholder `database_id` in `wrangler.jsonc` with the returned ID.
3. Add `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` repository secrets.
4. Run **Import BGP Database** with the repository and release tag that publish
   `mehrnet_bgp.tar.gz`.
5. Verify `bunx wrangler d1 execute BGP_DB --remote --command 'SELECT count(*) FROM routes'`.
6. Deploy explicitly with `bun run deploy:worker` when the import and API result
   have been verified.

The importer uses `gh release download` inside GitHub Actions and does not run
on this machine. It imports in place, so plan a blue/green D1 promotion before
using it for frequent production refreshes.

The response contract and the current data limitations are documented in
[docs/data-contract.md](docs/data-contract.md).
