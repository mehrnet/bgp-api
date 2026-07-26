import { sql, type SQLWrapper } from "drizzle-orm";
import { parseIp, sortKeyToIp, type ParsedIp } from "./lib/ip";
import { ipLookupResponseSchema, type IpLookupResponse } from "./lib/schema";

export interface AllocationRow {
  start_ip_sort: string;
  end_ip_sort: string;
  ip_version: number;
  registry: string;
  country: string;
  netname: string;
}

export interface RouteRow {
  start_ip_sort: string;
  end_ip_sort: string;
  ip_version: number;
  cidr: string;
  asn: string;
}

export interface GeolocationRow {
  start_ip_sort: string;
  end_ip_sort: string;
  ip_version: number;
  country: string;
  region: string;
  city: string;
}

export interface QueryExecutor {
  all<T>(query: SQLWrapper): Promise<T[]> | T[];
}

export interface IpLookupRepository {
  lookup(input: string): Promise<IpLookupResponse | null>;
}

const MAX_CANDIDATES = 64;

function narrowest<T extends { start_ip_sort: string; end_ip_sort: string }>(rows: T[]): T | null {
  if (rows.length === 0) return null;
  return rows.reduce((best, row) => {
    const bestWidth = BigInt(best.end_ip_sort) - BigInt(best.start_ip_sort);
    const rowWidth = BigInt(row.end_ip_sort) - BigInt(row.start_ip_sort);
    return rowWidth < bestWidth ? row : best;
  });
}

function countryCode(value: string | null): string | null {
  const cleaned = value?.trim().toUpperCase() ?? "";
  return /^[A-Z]{2}$/.test(cleaned) ? cleaned : null;
}

function nullableValue(value: string | null | undefined): string | null {
  const cleaned = value?.trim() ?? "";
  return cleaned || null;
}

function parseAsNumber(asn: string | null): number | null {
  const matched = asn?.trim().toUpperCase().match(/^AS(\d+)$/);
  if (!matched) return null;
  const number = Number(matched[1]);
  return Number.isSafeInteger(number) && number > 0 ? number : null;
}

async function candidateRows(executor: QueryExecutor, ip: ParsedIp) {
  const filters = sql`ip_version = ${ip.version} AND start_ip_sort <= ${ip.sortKey} AND end_ip_sort >= ${ip.sortKey}`;
  const [allocations, routes, geolocations] = await Promise.all([
    executor.all<AllocationRow>(sql`SELECT start_ip_sort, end_ip_sort, ip_version, registry, country, netname FROM allocations WHERE ${filters} ORDER BY start_ip_sort DESC LIMIT ${MAX_CANDIDATES}`),
    executor.all<RouteRow>(sql`SELECT start_ip_sort, end_ip_sort, ip_version, cidr, asn FROM routes WHERE ${filters} ORDER BY start_ip_sort DESC LIMIT ${MAX_CANDIDATES}`),
    executor.all<GeolocationRow>(sql`SELECT start_ip_sort, end_ip_sort, ip_version, country, region, city FROM geolocations WHERE ${filters} ORDER BY start_ip_sort DESC LIMIT ${MAX_CANDIDATES}`),
  ]);
  return { allocations, routes, geolocations };
}

export function createIpLookupRepository(executor: QueryExecutor): IpLookupRepository {
  return {
    async lookup(input) {
      const ip = parseIp(input);
      if (!ip) return null;

      const candidates = await candidateRows(executor, ip);
      const allocation = narrowest(candidates.allocations);
      const route = narrowest(candidates.routes);
      const geolocation = narrowest(candidates.geolocations);
      if (!allocation && !route && !geolocation) return null;

      const allocationCountry = nullableValue(allocation?.country);
      const response: IpLookupResponse = {
        ip: ip.canonical,
        version: ip.version,
        registry: nullableValue(allocation?.registry)?.toLowerCase() ?? null,
        // The current producer schema does not retain these upstream values.
        allocation_date: null,
        allocation_status: null,
        network: {
          cidr: nullableValue(route?.cidr),
          start_ip: route ? sortKeyToIp(route.start_ip_sort, ip.version) : null,
          end_ip: route ? sortKeyToIp(route.end_ip_sort, ip.version) : null,
          asn: nullableValue(route?.asn)?.toUpperCase() ?? null,
          as_number: parseAsNumber(route?.asn ?? null),
          name: nullableValue(allocation?.netname),
          status: null,
        },
        allocation: {
          start_ip: allocation ? sortKeyToIp(allocation.start_ip_sort, ip.version) : null,
          end_ip: allocation ? sortKeyToIp(allocation.end_ip_sort, ip.version) : null,
          registry: nullableValue(allocation?.registry)?.toLowerCase() ?? null,
          country_code: countryCode(allocationCountry),
          country_raw: allocationCountry,
          name: nullableValue(allocation?.netname),
          allocation_date: null,
          status: null,
        },
        location: {
          country_code: countryCode(nullableValue(geolocation?.country)) ?? countryCode(allocationCountry),
          region: nullableValue(geolocation?.region),
          city: nullableValue(geolocation?.city),
        },
        sources: {
          allocation: allocation !== null,
          route: route !== null,
          geofeed: geolocation !== null,
        },
      };
      return ipLookupResponseSchema.parse(response);
    },
  };
}
