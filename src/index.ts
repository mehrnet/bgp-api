import { errorResponseSchema } from "./lib/schema";

export interface WorkerBindings {
  BUN_API_ORIGIN: string;
  ORIGIN_AUTH_TOKEN?: string;
  RL_IP_LOOKUP: { limit(options: { key: string }): Promise<{ success: boolean }> };
}

function rateLimitResponse() {
  return Response.json(errorResponseSchema.parse({ error: { code: "RATE_LIMITED", message: "too many lookup requests" } }), {
    status: 429,
    headers: { "Retry-After": "60" },
  });
}

function upstreamUrl(request: Request, originValue: string) {
  const requestUrl = new URL(request.url);
  const origin = new URL(originValue);
  requestUrl.protocol = origin.protocol;
  requestUrl.hostname = origin.hostname;
  requestUrl.port = origin.port;
  return requestUrl;
}

export default {
  async fetch(request: Request, env: WorkerBindings) {
    const path = new URL(request.url).pathname;
    if (path.startsWith("/v1/ip/")) {
      const clientIp = request.headers.get("cf-connecting-ip") ?? "unknown";
      const rateLimit = await env.RL_IP_LOOKUP.limit({ key: clientIp });
      if (!rateLimit.success) return rateLimitResponse();
    }

    const headers = new Headers(request.headers);
    headers.delete("host");
    if (env.ORIGIN_AUTH_TOKEN) headers.set("x-bgp-api-origin-token", env.ORIGIN_AUTH_TOKEN);
    return fetch(new Request(upstreamUrl(request, env.BUN_API_ORIGIN), { method: request.method, headers, body: request.body }));
  },
};
