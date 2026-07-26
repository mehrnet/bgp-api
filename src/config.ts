import type { AppConfig } from "./app";

export interface RuntimeEnv {
  CORS_ALLOWED_ORIGINS_JSON?: string;
  ACTIVE_DATABASE?: string;
  ORIGIN_AUTH_TOKEN?: string;
}

export function readConfig(env: RuntimeEnv): AppConfig {
  const value = env.CORS_ALLOWED_ORIGINS_JSON ?? "[]";
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed) || !parsed.every((item) => typeof item === "string")) throw new Error("not an array of strings");
    const activeDatabase = env.ACTIVE_DATABASE ?? "primary";
    if (activeDatabase !== "primary" && activeDatabase !== "secondary") throw new Error("invalid active database");
    return { corsAllowedOrigins: parsed, activeDatabase, originAuthToken: env.ORIGIN_AUTH_TOKEN };
  } catch {
    throw new Error("invalid runtime configuration");
  }
}
