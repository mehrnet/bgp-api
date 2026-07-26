import { index, integer, sqliteTable, text } from "drizzle-orm/sqlite-core";

// These tables are produced by the RIR database pipeline. The API is read-only:
// migrations and data ingestion intentionally stay in the producer workflow.
export const allocations = sqliteTable(
  "allocations",
  {
    startIpSort: text("start_ip_sort").notNull(),
    endIpSort: text("end_ip_sort").notNull(),
    ipVersion: integer("ip_version").notNull(),
    registry: text("registry").notNull(),
    country: text("country").notNull(),
    netname: text("netname").notNull(),
  },
  (table) => [index("idx_alloc_lookup").on(table.ipVersion, table.startIpSort, table.endIpSort)],
);

export const routes = sqliteTable(
  "routes",
  {
    startIpSort: text("start_ip_sort").notNull(),
    endIpSort: text("end_ip_sort").notNull(),
    ipVersion: integer("ip_version").notNull(),
    cidr: text("cidr").notNull(),
    asn: text("asn").notNull(),
  },
  (table) => [index("idx_routes_lookup").on(table.ipVersion, table.startIpSort, table.endIpSort)],
);

export const geolocations = sqliteTable(
  "geolocations",
  {
    startIpSort: text("start_ip_sort").notNull(),
    endIpSort: text("end_ip_sort").notNull(),
    ipVersion: integer("ip_version").notNull(),
    country: text("country").notNull(),
    region: text("region").notNull(),
    city: text("city").notNull(),
  },
  (table) => [index("idx_geo_lookup").on(table.ipVersion, table.startIpSort, table.endIpSort)],
);
