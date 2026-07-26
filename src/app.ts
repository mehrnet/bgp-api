import { Hono } from "hono";
import { parseIp } from "./lib/ip";
import { errorResponseSchema } from "./lib/schema";
import type { IpLookupRepository } from "./repository";

export interface AppConfig {
  corsAllowedOrigins: string[];
}

function error(code: string, message: string) {
  return errorResponseSchema.parse({ error: { code, message } });
}

function allowedOrigin(config: AppConfig, origin: string | undefined): string | null {
  return origin && config.corsAllowedOrigins.includes(origin) ? origin : null;
}

export function createApiApp(repository: IpLookupRepository, config: AppConfig) {
  const app = new Hono();

  app.use("*", async (c, next) => {
    const origin = allowedOrigin(config, c.req.header("origin"));
    if (c.req.method === "OPTIONS") {
      const headers = new Headers({
        "Access-Control-Allow-Methods": "GET, OPTIONS",
        "Access-Control-Allow-Headers": "Content-Type",
        "Access-Control-Max-Age": "86400",
        Vary: "Origin",
      });
      if (origin) headers.set("Access-Control-Allow-Origin", origin);
      return new Response(null, { status: 204, headers });
    }
    await next();
    c.header("Access-Control-Allow-Methods", "GET, OPTIONS");
    c.header("Access-Control-Allow-Headers", "Content-Type");
    c.header("Vary", "Origin");
    if (origin) c.header("Access-Control-Allow-Origin", origin);
  });

  app.get("/", (c) => c.json({ ok: true, service: "bgp-api", version: 1 }));
  app.get("/v1/health", (c) => c.json({ ok: true, service: "bgp-api", version: 1 }));
  app.get("/v1/ip/:ip", async (c) => {
    const input = c.req.param("ip");
    if (!parseIp(input)) return c.json(error("INVALID_IP", "path parameter must be a valid IPv4 or IPv6 address"), 400);

    const result = await repository.lookup(input);
    if (!result) return c.json(error("IP_NOT_FOUND", "no RIR allocation, route, or geofeed record matched this IP"), 404);
    return c.json(result);
  });
  app.notFound((c) => c.json(error("NOT_FOUND", "route not found"), 404));
  app.onError((cause, c) => {
    console.error(cause);
    return c.json(error("INTERNAL_ERROR", "unexpected lookup failure"), 500);
  });

  return app;
}
