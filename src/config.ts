import type { AppConfig } from "./app";

export interface RuntimeEnv {
  CORS_ALLOWED_ORIGINS_JSON?: string;
  DATABASE_ENGINE?: string;
  ORIGIN_AUTH_TOKEN?: string;
}

export function readConfig(env: RuntimeEnv): AppConfig {
  const value = env.CORS_ALLOWED_ORIGINS_JSON ?? "[]";
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed) || !parsed.every((item) => typeof item === "string")) throw new Error("not an array of strings");
    const databaseEngine = env.DATABASE_ENGINE ?? "sqlite";
    if (databaseEngine !== "sqlite" && databaseEngine !== "postgresql") throw new Error("invalid database engine");
    return { corsAllowedOrigins: parsed, databaseEngine, originAuthToken: env.ORIGIN_AUTH_TOKEN };
  } catch {
    throw new Error("invalid runtime configuration");
  }
}
