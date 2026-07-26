import { sql, type SQLWrapper } from "drizzle-orm";
import { parseIp, prefixKeysForIp, sortKeyToIp, type ParsedIp } from "./lib/ip";
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

interface LookupPrefixRow {
  start_ip_sort: string;
  end_ip_sort: string;
  ip_version: number;
  registry: string | null;
  country: string | null;
  netname: string | null;
  cidr: string | null;
  asn: string | null;
  region: string | null;
  city: string | null;
}

function narrowest<T extends { start_ip_sort: string; end_ip_sort: string }>(rows: T[]): T | null {
  if (rows.length === 0) return null;
  return rows.reduce((best, row) => {
    const bestWidth = BigInt(best.end_ip_sort) - BigInt(best.start_ip_sort);
    const rowWidth = BigInt(row.end_ip_sort) - BigInt(row.start_ip_sort);
    return rowWidth < bestWidth ? row : best;
  });
}

function allNarrowest<T extends { start_ip_sort: string; end_ip_sort: string }>(rows: T[]): T[] {
  const best = narrowest(rows);
  if (!best) return [];
  const width = BigInt(best.end_ip_sort) - BigInt(best.start_ip_sort);
  return rows.filter((row) => BigInt(row.end_ip_sort) - BigInt(row.start_ip_sort) === width);
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
  const keys = sql.join(prefixKeysForIp(ip).map((key) => sql`${key}`), sql`, `);
  const query = (source: "allocation" | "route" | "geofeed") => executor.all<LookupPrefixRow>(sql`
    SELECT start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city
    FROM lookup_prefixes INDEXED BY idx_lookup_prefix
    WHERE source = ${source} AND prefix_key IN (${keys})
    ORDER BY prefix_length DESC
    LIMIT ${MAX_CANDIDATES}
  `);
  const [allocationRows, routeRows, geolocationRows] = await Promise.all([query("allocation"), query("route"), query("geofeed")]);
  return {
    allocations: allocationRows.map(({ start_ip_sort, end_ip_sort, ip_version, registry, country, netname }) => ({
      start_ip_sort, end_ip_sort, ip_version, registry: registry ?? "", country: country ?? "", netname: netname ?? "",
    })),
    routes: routeRows.map(({ start_ip_sort, end_ip_sort, ip_version, cidr, asn }) => ({
      start_ip_sort, end_ip_sort, ip_version, cidr: cidr ?? "", asn: asn ?? "",
    })),
    geolocations: geolocationRows.map(({ start_ip_sort, end_ip_sort, ip_version, country, region, city }) => ({
      start_ip_sort, end_ip_sort, ip_version, country: country ?? "", region: region ?? "", city: city ?? "",
    })),
  };
}

export function createIpLookupRepository(executor: QueryExecutor): IpLookupRepository {
  return {
    async lookup(input) {
      const ip = parseIp(input);
      if (!ip) return null;

      const candidates = await candidateRows(executor, ip);
      const allocation = narrowest(candidates.allocations);
      const routes = allNarrowest(candidates.routes);
      const route = routes[0] ?? null;
      const geolocation = narrowest(candidates.geolocations);
      if (!allocation && !route && !geolocation) return null;

      const allocationCountry = nullableValue(allocation?.country);
      const originAsns = [...new Set(routes.map((candidate) => nullableValue(candidate.asn)?.toUpperCase()).filter((asn): asn is string => asn !== null))]
        .sort((left, right) => (parseAsNumber(left) ?? 0) - (parseAsNumber(right) ?? 0));
      const uniqueOriginAsn = originAsns.length === 1 ? originAsns[0] : null;
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
          asn: uniqueOriginAsn,
          asns: originAsns,
          as_number: parseAsNumber(uniqueOriginAsn),
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
