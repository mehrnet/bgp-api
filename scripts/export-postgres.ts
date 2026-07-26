import { Database } from "bun:sqlite";
import { once } from "node:events";
import { createWriteStream } from "node:fs";
import { finished } from "node:stream/promises";
import { createGzip } from "node:zlib";

interface TableDefinition {
  name: "lookup_prefixes";
  columns: string[];
  create: string;
}

const table: TableDefinition = {
  name: "lookup_prefixes",
  columns: ["source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city"],
  create: "CREATE TABLE lookup_prefixes (source TEXT NOT NULL, prefix_key TEXT NOT NULL, prefix_length INTEGER NOT NULL, start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version INTEGER NOT NULL, registry TEXT, country TEXT, netname TEXT, cidr TEXT, asn TEXT, region TEXT, city TEXT);",
};

const [databasePath, outputPath, schema] = process.argv.slice(2);
if (!databasePath || !outputPath || !schema) throw new Error("usage: bun scripts/export-postgres.ts /path/to/mehrnet_bgp.db output.sql.gz schema_name");
if (!/^[a-z][a-z0-9_]*$/.test(schema)) throw new Error("schema name must be a lowercase PostgreSQL identifier");

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
  await write(`CREATE SCHEMA ${schema};\n${table.create.replace(table.name, `${schema}.${table.name}`)}\nCOPY ${schema}.${table.name} (${table.columns.join(", ")}) FROM STDIN WITH (FORMAT text);\n`);
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
  await exportTable(table);
  await write(`CREATE INDEX idx_lookup_prefix ON ${schema}.lookup_prefixes (source, prefix_key);\nANALYZE ${schema}.lookup_prefixes;\n`);
  gzip.end();
  await finished(output);
} finally {
  database.close();
}
