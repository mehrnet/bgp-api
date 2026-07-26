import { Database } from "bun:sqlite";
import { rangeToPrefixes } from "../src/lib/ip";

interface SourceRow {
  record_id: number;
  start_ip_sort: string;
  end_ip_sort: string;
  ip_version: 4 | 6;
  registry: string | null;
  country: string | null;
  netname: string | null;
  cidr: string | null;
  asn: string | null;
  region: string | null;
  city: string | null;
}

interface SourceDefinition {
  source: "allocation" | "route" | "geofeed";
  query: string;
}

const BATCH_SIZE = 1_000;
const LOG_INTERVAL = 100_000;
const path = process.argv[2] ?? process.env.LOCAL_DB_PATH;
if (!path) throw new Error("usage: bun run db:build-prefix-index -- /path/to/mehrnet_bgp.db");

const sources: SourceDefinition[] = [
  {
    source: "allocation",
    query: "SELECT rowid AS record_id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, NULL AS cidr, NULL AS asn, NULL AS region, NULL AS city FROM allocations WHERE rowid > ? ORDER BY rowid LIMIT ?",
  },
  {
    source: "route",
    query: "SELECT rowid AS record_id, start_ip_sort, end_ip_sort, ip_version, NULL AS registry, NULL AS country, NULL AS netname, cidr, asn, NULL AS region, NULL AS city FROM routes WHERE rowid > ? ORDER BY rowid LIMIT ?",
  },
  {
    source: "geofeed",
    query: "SELECT rowid AS record_id, start_ip_sort, end_ip_sort, ip_version, NULL AS registry, country, NULL AS netname, NULL AS cidr, NULL AS asn, region, city FROM geolocations WHERE rowid > ? ORDER BY rowid LIMIT ?",
  },
];

const sqlite = new Database(path);
sqlite.exec("DROP TABLE IF EXISTS lookup_prefixes");
sqlite.exec(`
  CREATE TABLE lookup_prefixes (
    source TEXT NOT NULL,
    prefix_key TEXT NOT NULL,
    prefix_length INTEGER NOT NULL,
    start_ip_sort TEXT NOT NULL,
    end_ip_sort TEXT NOT NULL,
    ip_version INTEGER NOT NULL,
    registry TEXT,
    country TEXT,
    netname TEXT,
    cidr TEXT,
    asn TEXT,
    region TEXT,
    city TEXT
  )
`);

const insert = sqlite.query(`
  INSERT INTO lookup_prefixes (
    source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version,
    registry, country, netname, cidr, asn, region, city
  ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`);

for (const definition of sources) {
  const select = sqlite.query<SourceRow, [number, number]>(definition.query);
  let lastRowId = 0;
  let sourceRows = 0;
  let prefixRows = 0;

  for (;;) {
    const rows = select.all(lastRowId, BATCH_SIZE);
    if (rows.length === 0) break;

    sqlite.exec("BEGIN IMMEDIATE");
    try {
      for (const row of rows) {
        for (const prefix of rangeToPrefixes(row.start_ip_sort, row.end_ip_sort, row.ip_version)) {
          insert.run(
            definition.source,
            prefix.key,
            prefix.length,
            row.start_ip_sort,
            row.end_ip_sort,
            row.ip_version,
            row.registry,
            row.country,
            row.netname,
            row.cidr,
            row.asn,
            row.region,
            row.city,
          );
          prefixRows++;
        }
      }
      sqlite.exec("COMMIT");
    } catch (error) {
      sqlite.exec("ROLLBACK");
      throw error;
    }

    lastRowId = rows.at(-1)!.record_id;
    sourceRows += rows.length;
    if (sourceRows % LOG_INTERVAL === 0 || rows.length < BATCH_SIZE) {
      console.log(`${definition.source}: ${sourceRows} records, ${prefixRows} prefix rows`);
    }
  }
}

sqlite.exec("CREATE INDEX idx_lookup_prefix ON lookup_prefixes(source, prefix_key)");
sqlite.exec("ANALYZE lookup_prefixes");
console.log("lookup prefix index complete");
