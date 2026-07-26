import { describe, expect, test } from "bun:test";
import { createApiApp } from "../app";
import { ipLookupResponseSchema } from "../lib/schema";
import type { IpLookupRepository } from "../repository";

const record = ipLookupResponseSchema.parse({
  ip: "1.1.1.1",
  version: 4,
  registry: "apnic",
  allocation_date: null,
  allocation_status: null,
  network: {
    cidr: "1.1.1.0/24",
    start_ip: "1.1.1.0",
    end_ip: "1.1.1.255",
    asn: "AS13335",
    as_number: 13335,
    name: "apnic-labs",
    status: null,
  },
  allocation: {
    start_ip: "1.1.1.0",
    end_ip: "1.1.1.255",
    registry: "apnic",
    country_code: "AU",
    country_raw: "AU",
    name: "apnic-labs",
    allocation_date: null,
    status: null,
  },
  location: { country_code: "AU", region: null, city: null },
  sources: { allocation: true, route: true, geofeed: false },
});

const repository: IpLookupRepository = { lookup: async () => record };
const app = createApiApp(repository, { corsAllowedOrigins: ["https://app.example.test"] });

describe("IP lookup route", () => {
  test("returns the public JSON contract", async () => {
    const response = await app.request("https://api.example.test/v1/ip/1.1.1.1");
    expect(response.status).toBe(200);
    expect(ipLookupResponseSchema.parse(await response.json())).toEqual(record);
  });

  test("rejects an invalid address before lookup", async () => {
    const response = await app.request("https://api.example.test/v1/ip/not-an-ip");
    expect(response.status).toBe(400);
    expect((await response.json()) as unknown).toEqual({ error: { code: "INVALID_IP", message: "path parameter must be a valid IPv4 or IPv6 address" } });
  });

  test("answers CORS preflight only for allowed origins", async () => {
    const response = await app.request("https://api.example.test/v1/ip/1.1.1.1", { method: "OPTIONS", headers: { Origin: "https://app.example.test" } });
    expect(response.status).toBe(204);
    expect(response.headers.get("Access-Control-Allow-Origin")).toBe("https://app.example.test");
  });
});
