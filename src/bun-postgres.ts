import { readConfig } from "./config";
import { createApiApp } from "./app";
import { drizzle } from "drizzle-orm/postgres-js";
import type { SQLWrapper } from "drizzle-orm";
import postgres from "postgres";
import { createIpLookupRepository } from "./repository";
import * as schema from "./db/postgres-schema";

const connectionString = process.env.DATABASE_URL;
if (!connectionString) throw new Error("DATABASE_URL is required for the PostgreSQL server");

const port = Number.parseInt(process.env.PORT ?? "3102", 10);
const maxConnections = Number.parseInt(process.env.POSTGRES_MAX_CONNECTIONS ?? "16", 10);
const client = postgres(connectionString, { max: maxConnections });
const db = drizzle(client, { schema });
const repository = createIpLookupRepository({
  all: async <T>(query: SQLWrapper) => (await db.execute(query)) as unknown as T[],
});
const config = readConfig({
  CORS_ALLOWED_ORIGINS_JSON: process.env.CORS_ALLOWED_ORIGINS_JSON,
  DATABASE_ENGINE: "postgresql",
  ORIGIN_AUTH_TOKEN: process.env.ORIGIN_AUTH_TOKEN,
});
const app = createApiApp(repository, config);

Bun.serve({
  port,
  fetch(request) {
    return app.fetch(request);
  },
});

console.log(`bgp-api PostgreSQL server listening on http://127.0.0.1:${port}`);
