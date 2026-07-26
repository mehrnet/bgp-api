import { z } from "zod";

const nullableString = z.string().nullable();

export const ipLookupResponseSchema = z.object({
  ip: z.string(),
  version: z.union([z.literal(4), z.literal(6)]),
  registry: nullableString,
  allocation_date: nullableString,
  allocation_status: nullableString,
  network: z.object({
    cidr: nullableString,
    start_ip: nullableString,
    end_ip: nullableString,
    asn: nullableString,
    as_number: z.number().int().positive().nullable(),
    name: nullableString,
    status: nullableString,
  }),
  allocation: z.object({
    start_ip: nullableString,
    end_ip: nullableString,
    registry: nullableString,
    country_code: nullableString,
    country_raw: nullableString,
    name: nullableString,
    allocation_date: nullableString,
    status: nullableString,
  }),
  location: z.object({
    country_code: nullableString,
    region: nullableString,
    city: nullableString,
  }),
  sources: z.object({
    allocation: z.boolean(),
    route: z.boolean(),
    geofeed: z.boolean(),
  }),
});

export type IpLookupResponse = z.infer<typeof ipLookupResponseSchema>;

export const errorResponseSchema = z.object({
  error: z.object({
    code: z.string(),
    message: z.string(),
  }),
});
