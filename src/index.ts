import { drizzle } from "drizzle-orm/d1";
import { createApiApp } from "./app";
import { readConfig, type RuntimeEnv } from "./config";
import { createIpLookupRepository } from "./repository";
import * as schema from "./db/schema";

export interface WorkerBindings extends RuntimeEnv {
  BGP_DB_PRIMARY: D1Database;
  BGP_DB_SECONDARY: D1Database;
  RL_IP_LOOKUP: { limit(options: { key: string }): Promise<{ success: boolean }> };
}

export default {
  async fetch(request: Request, env: WorkerBindings, ctx: ExecutionContext) {
    const config = readConfig(env);
    const database = config.activeDatabase === "secondary" ? env.BGP_DB_SECONDARY : env.BGP_DB_PRIMARY;
    const db = drizzle(database, { schema });
    const repository = createIpLookupRepository({ all: (query) => db.all(query) });
    return createApiApp(repository, config, env.RL_IP_LOOKUP).fetch(request, env, ctx);
  },
};
