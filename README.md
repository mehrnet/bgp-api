# bgp-api

Public IP-to-BGP/allocation/geofeed lookup API for `bgp-api.mehrnet.com`.
The production API is Bun, Drizzle, and PostgreSQL. SQLite remains supported
for local development and for the producer's portable database artifacts.

Cloudflare Workers is an edge proxy for the Bun API: it provides the custom
domain and native rate limiting, but does not store or import this dataset.

## Bun with PostgreSQL

The producer publishes a compressed PostgreSQL `COPY` dump containing all
source tables, the materialized `lookup_prefixes` table, indexes, and
`ANALYZE`. Import it into an empty PostgreSQL database:

```sh
export DATABASE_URL='postgres://bgp_api:change-me@127.0.0.1:5432/bgp_api'
gh release download --repo mehrnet/bgp-api --pattern 'mehrnet_bgp_postgres.sql.gz'
gzip -dc mehrnet_bgp_postgres.sql.gz | psql "$DATABASE_URL"

DATABASE_URL="$DATABASE_URL" \
CORS_ALLOWED_ORIGINS_JSON='["https://your-frontend.example"]' \
bun run start:postgres
```

`DATABASE_URL` selects the PostgreSQL runtime. `POSTGRES_MAX_CONNECTIONS`
defaults to 16. The dump must be imported into an empty database. For a daily
blue/green refresh, load the new dump into an inactive PostgreSQL database,
validate it, change the Bun service's `DATABASE_URL` to that database, and
restart the service. Rebuild the newly inactive database afterward.

PostgreSQL is the production database because a daily full D1 import of this
indexed dataset would incur very high row-write charges. Do not use D1 for the
daily producer output.

## Bun with SQLite

SQLite is useful for local development, validation, and inspecting a release:

```sh
bun install
LOCAL_DB_PATH=../mehrnet_bgp.db bun run dev
curl http://127.0.0.1:3102/v1/ip/1.1.1.1
bun run check
```

The current database stores IPv4 keys as IPv4-mapped IPv6 decimal values. The
lookup layer deliberately preserves that producer format; do not change it
without rebuilding the source database.

## Cloudflare Worker Proxy

The Worker forwards requests to the Bun/PostgreSQL origin and applies the
native `RL_IP_LOOKUP` limit of 120 requests per minute per client IP on
`/v1/ip/*`. It holds no database binding or database credentials.

1. Set `BUN_API_ORIGIN` in `wrangler.jsonc`, or override it on deployment.
2. Generate a long random `ORIGIN_AUTH_TOKEN`, configure it in the Bun service,
   then add the same value to the Worker as a secret.
3. Deploy with an API token that can edit the Worker and its route.

```sh
export ORIGIN_AUTH_TOKEN='replace-with-a-long-random-value'
printf '%s' "$ORIGIN_AUTH_TOKEN" | bunx wrangler secret put ORIGIN_AUTH_TOKEN

export CLOUDFLARE_API_TOKEN='replace-with-a-scoped-token'
bun run deploy:worker -- --var BUN_API_ORIGIN:https://bgp-origin.example.com
```

Start Bun with the same origin token:

```sh
DATABASE_URL="$DATABASE_URL" ORIGIN_AUTH_TOKEN="$ORIGIN_AUTH_TOKEN" bun run start:postgres
```

When `ORIGIN_AUTH_TOKEN` is set, direct origin requests without
`x-bgp-api-origin-token` receive `401`. Keep the origin private or firewall it
to Cloudflare in addition to this application-level check.

## Producer Releases

GitHub Actions runs daily at 04:00 UTC and supports manual dispatch. It only
builds and publishes release assets; it never receives a Cloudflare API token.
Each successful release includes:

- `mehrnet_bgp.tar.gz`: normal SQLite database.
- `mehrnet_bgp_indexed.tar.gz`: SQLite database with lookup indexes.
- `mehrnet_bgp_postgres.sql.gz`: indexed PostgreSQL `COPY` dump.
- `SHA256SUMS.txt`: hashes for every release asset.

Assets over GitHub's release limit are split into numbered `.part-*` files.
Download every part and concatenate them in order before extracting or
decompressing the asset.

The response contract and the current data limitations are documented in
[docs/data-contract.md](docs/data-contract.md).
