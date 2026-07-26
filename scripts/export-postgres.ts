import { Database } from "bun:sqlite";
import { once } from "node:events";
import { createWriteStream } from "node:fs";
import { finished } from "node:stream/promises";
import { createGzip } from "node:zlib";

interface TableDefinition {
  name: "allocations" | "routes" | "geolocations" | "lookup_prefixes";
  columns: string[];
  create: string;
}

const tables: TableDefinition[] = [
  {
    name: "allocations",
    columns: ["start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname"],
    create: "CREATE TABLE allocations (start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version INTEGER NOT NULL, registry TEXT NOT NULL, country TEXT NOT NULL, netname TEXT NOT NULL);",
  },
  {
    name: "routes",
    columns: ["start_ip_sort", "end_ip_sort", "ip_version", "cidr", "asn"],
    create: "CREATE TABLE routes (start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version INTEGER NOT NULL, cidr TEXT NOT NULL, asn TEXT NOT NULL);",
  },
  {
    name: "geolocations",
    columns: ["start_ip_sort", "end_ip_sort", "ip_version", "country", "region", "city"],
    create: "CREATE TABLE geolocations (start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version INTEGER NOT NULL, country TEXT NOT NULL, region TEXT NOT NULL, city TEXT NOT NULL);",
  },
  {
    name: "lookup_prefixes",
    columns: ["source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city"],
    create: "CREATE TABLE lookup_prefixes (source TEXT NOT NULL, prefix_key TEXT NOT NULL, prefix_length INTEGER NOT NULL, start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version INTEGER NOT NULL, registry TEXT, country TEXT, netname TEXT, cidr TEXT, asn TEXT, region TEXT, city TEXT);",
  },
];

const indexes = [
  "CREATE INDEX idx_alloc_by_start ON allocations (ip_version, start_ip_sort);",
  "CREATE INDEX idx_alloc_by_end ON allocations (ip_version, end_ip_sort);",
  "CREATE INDEX idx_routes_by_start ON routes (ip_version, start_ip_sort);",
  "CREATE INDEX idx_routes_by_end ON routes (ip_version, end_ip_sort);",
  "CREATE INDEX idx_geo_by_start ON geolocations (ip_version, start_ip_sort);",
  "CREATE INDEX idx_geo_by_end ON geolocations (ip_version, end_ip_sort);",
  "CREATE INDEX idx_lookup_prefix ON lookup_prefixes (source, prefix_key);",
];

const [databasePath, outputPath] = process.argv.slice(2);
if (!databasePath || !outputPath) throw new Error("usage: bun scripts/export-postgres.ts /path/to/mehrnet_bgp.db output.sql.gz");

function copyValue(value: unknown) {
  if (value === null || value === undefined) return "\\N";
  return String(value).replaceAll("\\", "\\\\").replaceAll("\t", "\\t").replaceAll("\n", "\\n").replaceAll("\r", "\\r");
}

const database = new Database(databasePath, { readonly: true });
const output = createWriteStream(outputPath);
const gzip = createGzip({ level: 9 });
gzip.pipe(output);

async function write(value: string) {
  if (!gzip.write(value)) await once(gzip, "drain");
}

async function exportTable(table: TableDefinition) {
  await write(`${table.create}\nCOPY ${table.name} (${table.columns.join(", ")}) FROM STDIN WITH (FORMAT text);\n`);
  const statement = database.query(`SELECT ${table.columns.join(", ")} FROM ${table.name}`);
  let count = 0;
  for (const row of statement.iterate() as Iterable<Record<string, unknown>>) {
    await write(`${table.columns.map((column) => copyValue(row[column])).join("\t")}\n`);
    count++;
    if (count % 1_000_000 === 0) console.log(`${table.name}: ${count} rows`);
  }
  await write("\\.\n");
  console.log(`${table.name}: ${count} rows`);
}

try {
  await write("SET client_encoding = 'UTF8';\n");
  for (const table of tables) await exportTable(table);
  await write(`${indexes.join("\n")}\nANALYZE;\n`);
  gzip.end();
  await finished(output);
} finally {
  database.close();
}
