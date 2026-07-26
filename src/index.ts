import { drizzle } from "drizzle-orm/d1";
import { createApiApp } from "./app";
import { readConfig, type RuntimeEnv } from "./config";
import { createIpLookupRepository } from "./repository";
import * as schema from "./db/schema";

export interface WorkerBindings extends RuntimeEnv {
  BGP_DB: D1Database;
}

export default {
  async fetch(request: Request, env: WorkerBindings, ctx: ExecutionContext) {
    const db = drizzle(env.BGP_DB, { schema });
    const repository = createIpLookupRepository({ all: (query) => db.all(query) });
    return createApiApp(repository, readConfig(env)).fetch(request, env, ctx);
  },
};
