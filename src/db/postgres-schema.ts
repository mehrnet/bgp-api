import { index, integer, pgTable, text } from "drizzle-orm/pg-core";

// PostgreSQL representation of the read-only RIR dataset. The producer emits
// this schema and data as a PostgreSQL COPY dump; the API never mutates it.
export const allocations = pgTable(
  "allocations",
  {
    startIpSort: text("start_ip_sort").notNull(),
    endIpSort: text("end_ip_sort").notNull(),
    ipVersion: integer("ip_version").notNull(),
    registry: text("registry").notNull(),
    country: text("country").notNull(),
    netname: text("netname").notNull(),
  },
  (table) => [
    index("idx_alloc_by_start").on(table.ipVersion, table.startIpSort),
    index("idx_alloc_by_end").on(table.ipVersion, table.endIpSort),
  ],
);

export const routes = pgTable(
  "routes",
  {
    startIpSort: text("start_ip_sort").notNull(),
    endIpSort: text("end_ip_sort").notNull(),
    ipVersion: integer("ip_version").notNull(),
    cidr: text("cidr").notNull(),
    asn: text("asn").notNull(),
  },
  (table) => [
    index("idx_routes_by_start").on(table.ipVersion, table.startIpSort),
    index("idx_routes_by_end").on(table.ipVersion, table.endIpSort),
  ],
);

export const geolocations = pgTable(
  "geolocations",
  {
    startIpSort: text("start_ip_sort").notNull(),
    endIpSort: text("end_ip_sort").notNull(),
    ipVersion: integer("ip_version").notNull(),
    country: text("country").notNull(),
    region: text("region").notNull(),
    city: text("city").notNull(),
  },
  (table) => [
    index("idx_geo_by_start").on(table.ipVersion, table.startIpSort),
    index("idx_geo_by_end").on(table.ipVersion, table.endIpSort),
  ],
);

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
