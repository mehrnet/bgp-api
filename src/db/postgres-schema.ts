import { index, integer, pgTable, text } from "drizzle-orm/pg-core";

// PostgreSQL production imports only the materialized lookup table. It carries
// every field needed by the API and avoids duplicating source-only tables.
export const lookupPrefixes = pgTable(
  "lookup_prefixes",
  {
    source: text("source").notNull(),
    prefixKey: text("prefix_key").notNull(),
    prefixLength: integer("prefix_length").notNull(),
    startIpSort: text("start_ip_sort").notNull(),
    endIpSort: text("end_ip_sort").notNull(),
    ipVersion: integer("ip_version").notNull(),
    registry: text("registry"),
    country: text("country"),
    netname: text("netname"),
    cidr: text("cidr"),
    asn: text("asn"),
    region: text("region"),
    city: text("city"),
  },
  (table) => [index("idx_lookup_prefix").on(table.source, table.prefixKey)],
);
