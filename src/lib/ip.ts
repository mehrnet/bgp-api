export interface ParsedIp {
  canonical: string;
  version: 4 | 6;
  sortKey: string;
}

const SORT_KEY_WIDTH = 39;
const IPV4_MAPPED_PREFIX = 0xffffn << 32n;

function padSortKey(value: bigint): string {
  return value.toString(10).padStart(SORT_KEY_WIDTH, "0");
}

function parseIpv4(input: string): number[] | null {
  const parts = input.split(".");
  if (parts.length !== 4) return null;

  const octets: number[] = [];
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) return null;
    const octet = Number(part);
    if (octet > 255) return null;
    octets.push(octet);
  }
  return octets;
}

function ipv4Value(octets: number[]): bigint {
  return octets.reduce((value, octet) => (value << 8n) | BigInt(octet), 0n);
}

function parseIpv6(input: string): number[] | null {
  if (!input || input.includes("%") || input.split("::").length > 2) return null;

  const [left, right] = input.split("::");
  const leftParts = left ? left.split(":") : [];
  const rightParts = right ? right.split(":") : [];
  const parts = [...leftParts, ...rightParts];
  const groups: number[] = [];

  for (let index = 0; index < parts.length; index++) {
    const part = parts[index];
    if (part.includes(".")) {
      if (index !== parts.length - 1) return null;
      const ipv4 = parseIpv4(part);
      if (!ipv4) return null;
      groups.push((ipv4[0] << 8) | ipv4[1], (ipv4[2] << 8) | ipv4[3]);
      continue;
    }
    if (!/^[0-9a-fA-F]{1,4}$/.test(part)) return null;
    groups.push(Number.parseInt(part, 16));
  }

  if (input.includes("::")) {
    const omitted = 8 - groups.length;
    if (omitted < 1) return null;
    return [...groups.slice(0, leftParts.length), ...Array(omitted).fill(0), ...groups.slice(leftParts.length)];
  }
  return groups.length === 8 ? groups : null;
}

function ipv6Value(groups: number[]): bigint {
  return groups.reduce((value, group) => (value << 16n) | BigInt(group), 0n);
}

function compressIpv6(groups: number[]): string {
  let bestStart = -1;
  let bestLength = 0;
  for (let index = 0; index < groups.length;) {
    if (groups[index] !== 0) {
      index++;
      continue;
    }
    let end = index;
    while (end < groups.length && groups[end] === 0) end++;
    if (end - index > bestLength && end - index >= 2) {
      bestStart = index;
      bestLength = end - index;
    }
    index = end;
  }

  const rendered = groups.map((group) => group.toString(16));
  if (bestStart === -1) return rendered.join(":");
  const before = rendered.slice(0, bestStart).join(":");
  const after = rendered.slice(bestStart + bestLength).join(":");
  if (!before && !after) return "::";
  if (!before) return `::${after}`;
  if (!after) return `${before}::`;
  return `${before}::${after}`;
}

export function parseIp(input: string): ParsedIp | null {
  const value = input.trim();
  const ipv4 = parseIpv4(value);
  if (ipv4) {
    const number = ipv4Value(ipv4);
    return {
      canonical: ipv4.join("."),
      version: 4,
      // The existing Go producer calls net.IP.To16 for every address. Preserve
      // its IPv4-mapped key format so this API can query already-built databases.
      sortKey: padSortKey(IPV4_MAPPED_PREFIX | number),
    };
  }

  const ipv6 = parseIpv6(value);
  if (!ipv6) return null;
  return {
    canonical: compressIpv6(ipv6),
    version: 6,
    sortKey: padSortKey(ipv6Value(ipv6)),
  };
}

export function sortKeyToIp(sortKey: string, version: 4 | 6): string {
  const value = BigInt(sortKey);
  if (version === 4) {
    const ipv4 = value & 0xffffffffn;
    return [24n, 16n, 8n, 0n].map((shift) => Number((ipv4 >> shift) & 0xffn)).join(".");
  }

  const groups: number[] = [];
  for (let shift = 112n; shift >= 0n; shift -= 16n) groups.push(Number((value >> shift) & 0xffffn));
  return compressIpv6(groups);
}
