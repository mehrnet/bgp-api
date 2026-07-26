import type { AppConfig } from "./app";

export interface RuntimeEnv {
  CORS_ALLOWED_ORIGINS_JSON?: string;
}

export function readConfig(env: RuntimeEnv): AppConfig {
  const value = env.CORS_ALLOWED_ORIGINS_JSON ?? "[]";
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed) || !parsed.every((item) => typeof item === "string")) throw new Error("not an array of strings");
    return { corsAllowedOrigins: parsed };
  } catch {
    throw new Error("CORS_ALLOWED_ORIGINS_JSON must be a JSON array of origins");
  }
}
