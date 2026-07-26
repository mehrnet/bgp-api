import { Database } from "bun:sqlite";
import { drizzle } from "drizzle-orm/bun-sqlite";
import { createApiApp } from "./app";
import { readConfig } from "./config";
import { createIpLookupRepository } from "./repository";
import * as schema from "./db/schema";

const path = process.env.LOCAL_DB_PATH ?? "./mehrnet_bgp.db";
const sqlite = new Database(path, { readonly: true });
const db = drizzle(sqlite, { schema });
const repository = createIpLookupRepository({ all: (query) => db.all(query) });
const config = readConfig({ CORS_ALLOWED_ORIGINS_JSON: process.env.CORS_ALLOWED_ORIGINS_JSON });
const app = createApiApp(repository, config);
const port = Number.parseInt(process.env.PORT ?? "3102", 10);

Bun.serve({
  port,
  fetch(request) {
    return app.fetch(request);
  },
});

console.log(`bgp-api listening on http://127.0.0.1:${port}`);
