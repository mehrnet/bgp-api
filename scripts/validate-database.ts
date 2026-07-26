import { Database } from "bun:sqlite";

const path = process.argv[2] ?? process.env.LOCAL_DB_PATH;
if (!path) throw new Error("usage: bun run db:validate -- /path/to/mehrnet_bgp.db");

const sqlite = new Database(path, { readonly: true });
const expected = ["allocations", "routes", "geolocations"];
const tables = sqlite.query<{ name: string }, []>("SELECT name FROM sqlite_master WHERE type = 'table'").all().map((row) => row.name);
const missing = expected.filter((table) => !tables.includes(table));
if (missing.length > 0) throw new Error(`database is missing required tables: ${missing.join(", ")}`);

for (const table of expected) {
  const count = sqlite.query<{ count: number }, []>(`SELECT count(*) AS count FROM ${table}`).get()?.count ?? 0;
  if (count === 0) throw new Error(`database table ${table} is empty`);
  console.log(`${table}: ${count}`);
}
