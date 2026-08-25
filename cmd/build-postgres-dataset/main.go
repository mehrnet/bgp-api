package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mehrnet/bgp-api/internal/ipkey"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type summary struct {
	allocations int64
	routes      int64
	countries   map[string]int64
	asns        map[string]int64
}

type facet struct {
	Value       string `json:"value"`
	RecordCount int64  `json:"record_count"`
}

func main() {
	databaseURL := flag.String("database-url", "", "PostgreSQL connection URL")
	inputDir := flag.String("input", "", "producer input directory")
	schema := flag.String("schema", "", "dataset schema")
	release := flag.String("release-tag", "", "release tag")
	builtAt := flag.String("built-at", "", "dataset build timestamp")
	commit := flag.String("source-commit", "", "producer source commit")
	flag.Parse()
	if *databaseURL == "" || *inputDir == "" || !identifier.MatchString(*schema) {
		log.Fatal("usage: build-postgres-dataset -database-url URL -input final_data -schema bgp_...")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET synchronous_commit = off; SET maintenance_work_mem = '1GB'"); err != nil {
		log.Fatal(err)
	}
	if err := build(ctx, conn, *inputDir, *schema, *release, *builtAt, *commit); err != nil {
		log.Fatal(err)
	}
}

func build(ctx context.Context, conn *pgx.Conn, dir, schema, release, builtAt, commit string) error {
	if _, err := conn.Exec(ctx, `CREATE ROLE bgp_api LOGIN`); err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoteIdent(schema)); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, schemaSQL(schema)); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s.dataset_metadata VALUES ($1,$2,$3)", quoteIdent(schema)), release, builtAt, commit); err != nil {
		return err
	}

	allocPath := filepath.Join(dir, "allocations.csv")
	routePath := filepath.Join(dir, "routes.csv")
	autnumPath := filepath.Join(dir, "autnums.csv")
	geoPath := filepath.Join(dir, "geolocations.csv")
	for _, path := range []string{allocPath, routePath, autnumPath, geoPath} {
		if _, err := os.Stat(path); err != nil {
			return err
		}
	}
	if err := copyCSV(ctx, conn, schema, "allocation_objects", allocationColumns, allocPath, func(row []string) ([]any, bool) {
		if len(row) < 15 {
			return nil, false
		}
		return []any{row[0], row[1], number(row[2]), row[3], nullable(row[4]), nullable(row[5]), nullable(row[6]), nullable(row[7]), nullable(row[8]), nullable(row[9]), nullable(row[10]), nullable(row[11]), nullable(row[12]), nullable(row[13]), nullable(row[14])}, true
	}); err != nil {
		return err
	}
	if err := copyCSV(ctx, conn, schema, "route_objects", routeColumns, routePath, func(row []string) ([]any, bool) {
		if len(row) < 11 {
			return nil, false
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(row[3]))
		if err != nil || strings.TrimSpace(row[4]) == "" {
			return nil, false
		}
		return []any{prefix.Masked().String(), prefix.Bits(), row[0], row[1], number(row[2]), row[4], nullableASN(row[4]), nullable(row[5]), nullable(row[6]), nullable(row[7]), nullable(row[8]), nullable(row[9]), nullable(row[10])}, true
	}); err != nil {
		return err
	}
	if err := copyCSV(ctx, conn, schema, "autnums", autnumColumns, autnumPath, func(row []string) ([]any, bool) {
		if len(row) < 12 || strings.TrimSpace(row[0]) == "" {
			return nil, false
		}
		return []any{row[0], nullableASN(row[0]), nullable(row[1]), nullable(row[2]), nullable(row[3]), nullable(row[4]), nullable(row[5]), nullable(row[6]), nullable(row[7]), nullable(row[8]), nullable(row[9]), nullable(row[10]), nullable(row[11])}, true
	}); err != nil {
		return err
	}

	if err := copyLookupRows(ctx, conn, schema, allocPath, routePath, geoPath); err != nil {
		return err
	}
	if err := copySummaryRows(ctx, conn, schema, allocPath, routePath); err != nil {
		return err
	}
	return publicViews(ctx, conn, schema)
}

var allocationColumns = []string{"start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"}
var routeColumns = []string{"prefix", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "origin_asn", "asn_number", "registry", "record_source", "mnt_by", "org", "abuse_contact", "description"}
var autnumColumns = []string{"asn", "asn_number", "registry", "country", "as_name", "org", "status", "created", "last_modified", "record_source", "mnt_by", "abuse_contact", "description"}
var lookupColumns = []string{"source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"}

func schemaSQL(schema string) string {
	s := quoteIdent(schema)
	return fmt.Sprintf(`CREATE TABLE %s.dataset_metadata (release_tag TEXT, built_at TEXT, source_commit TEXT);
CREATE TABLE %s.allocation_objects (id BIGSERIAL PRIMARY KEY, start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version SMALLINT NOT NULL, registry TEXT, country TEXT, netname TEXT, status TEXT, allocation_date TEXT, created TEXT, last_modified TEXT, record_source TEXT, mnt_by TEXT, org TEXT, abuse_contact TEXT, description TEXT);
CREATE TABLE %s.route_objects (id BIGSERIAL PRIMARY KEY, prefix CIDR NOT NULL, prefix_length SMALLINT NOT NULL, start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version SMALLINT NOT NULL, origin_asn TEXT NOT NULL, asn_number BIGINT, registry TEXT, record_source TEXT, mnt_by TEXT, org TEXT, abuse_contact TEXT, description TEXT);
CREATE TABLE %s.autnums (id BIGSERIAL PRIMARY KEY, asn TEXT NOT NULL, asn_number BIGINT, registry TEXT, country TEXT, as_name TEXT, org TEXT, status TEXT, created TEXT, last_modified TEXT, record_source TEXT, mnt_by TEXT, abuse_contact TEXT, description TEXT);
CREATE TABLE %s.lookup_prefixes (source TEXT NOT NULL, prefix_key TEXT NOT NULL, prefix_length INTEGER NOT NULL, start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version SMALLINT NOT NULL, registry TEXT, country TEXT, netname TEXT, cidr TEXT, asn TEXT, region TEXT, city TEXT, status TEXT, allocation_date TEXT, created TEXT, last_modified TEXT, record_source TEXT, mnt_by TEXT, org TEXT, abuse_contact TEXT, description TEXT);
CREATE TABLE %s.range_summaries (cidr CIDR PRIMARY KEY, ip_version SMALLINT NOT NULL, prefix_length SMALLINT NOT NULL, allocation_records BIGINT NOT NULL, route_records BIGINT NOT NULL, countries JSONB NOT NULL, asns JSONB NOT NULL);`, s, s, s, s, s, s)
}

func copyCSV(ctx context.Context, conn *pgx.Conn, schema, table string, columns []string, path string, mapRow func([]string) ([]any, bool)) error {
	values := make([][]any, 0, 10000)
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		_, err := conn.CopyFrom(ctx, pgx.Identifier{schema, table}, columns, pgx.CopyFromRows(values))
		values = values[:0]
		return err
	}
	if err := forEachCSV(path, func(row []string) error {
		if value, ok := mapRow(row); ok {
			values = append(values, value)
		}
		if len(values) >= 10000 {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	return flush()
}

func copyLookupRows(ctx context.Context, conn *pgx.Conn, schema string, allocPath, routePath, geoPath string) error {
	rows := make([][]any, 0, 10000)
	add := func(source string, row []string, cidr, asn, region, city string) error {
		if len(row) < 3 {
			return nil
		}
		version, err := strconv.Atoi(row[2])
		if err != nil {
			return nil
		}
		covers, err := ipkey.RangeToPrefixes(row[0], row[1], version)
		if err != nil {
			return nil
		}
		registry, country, netname, status, allocationDate := "", "", "", "", ""
		created, modified, recordSource, mntBy, org, abuse, description := "", "", "", "", "", "", ""
		switch source {
		case "allocation":
			registry, country, netname, status, allocationDate = at(row, 3), at(row, 4), at(row, 5), at(row, 6), at(row, 7)
			created, modified, recordSource, mntBy, org, abuse, description = at(row, 8), at(row, 9), at(row, 10), at(row, 11), at(row, 12), at(row, 13), at(row, 14)
		case "route":
			registry, recordSource, mntBy, org, abuse, description = at(row, 5), at(row, 6), at(row, 7), at(row, 8), at(row, 9), at(row, 10)
		case "geofeed":
			country, region, city = at(row, 3), at(row, 4), at(row, 5)
		}
		for _, cover := range covers {
			rows = append(rows, []any{source, cover.Key, cover.Length, row[0], row[1], version, nullable(registry), nullable(country), nullable(netname), nullable(cidr), nullable(asn), nullable(region), nullable(city), nullable(status), nullable(allocationDate), nullable(created), nullable(modified), nullable(recordSource), nullable(mntBy), nullable(org), nullable(abuse), nullable(description)})
			if len(rows) >= 10000 {
				if _, err := conn.CopyFrom(ctx, pgx.Identifier{schema, "lookup_prefixes"}, lookupColumns, pgx.CopyFromRows(rows)); err != nil {
					return err
				}
				rows = rows[:0]
			}
		}
		return nil
	}
	if err := forEachCSV(allocPath, func(row []string) error { return add("allocation", row, "", "", "", "") }); err != nil {
		return err
	}
	if err := forEachCSV(routePath, func(row []string) error { return add("route", row, at(row, 3), at(row, 4), "", "") }); err != nil {
		return err
	}
	if err := forEachCSV(geoPath, func(row []string) error { return add("geofeed", row, "", "", at(row, 4), at(row, 5)) }); err != nil {
		return err
	}
	if len(rows) > 0 {
		_, err := conn.CopyFrom(ctx, pgx.Identifier{schema, "lookup_prefixes"}, lookupColumns, pgx.CopyFromRows(rows))
		return err
	}
	return nil
}

func copySummaryRows(ctx context.Context, conn *pgx.Conn, schema, allocPath, routePath string) error {
	items := map[string]*summary{}
	collect := func(path string, route bool) error {
		return forEachCSV(path, func(row []string) error {
			if len(row) < 5 || row[2] != "4" {
				return nil
			}
			start, e1 := strconv.ParseUint(strings.TrimSpace(row[0]), 10, 32)
			end, e2 := strconv.ParseUint(strings.TrimSpace(row[1]), 10, 32)
			if e1 != nil || e2 != nil || start > end {
				return nil
			}
			value := at(row, 4)
			for _, length := range []uint{0, 8, 16} {
				first, last := uint32(start)>>(32-length), uint32(end)>>(32-length)
				for bucket := first; ; bucket++ {
					cidr := fmt.Sprintf("%d.%d.%d.%d/%d", bucket>>(24-length), (bucket>>(16-length))&255, (bucket>>(8-length))&255, bucket&255, length)
					item := items[cidr]
					if item == nil {
						item = &summary{countries: map[string]int64{}, asns: map[string]int64{}}
						items[cidr] = item
					}
					if route {
						item.routes++
						if strings.TrimSpace(value) != "" {
							item.asns[strings.ToUpper(value)]++
						}
					} else {
						item.allocations++
						if strings.TrimSpace(value) != "" {
							item.countries[strings.ToUpper(value)]++
						}
					}
					if bucket == last {
						break
					}
				}
			}
			return nil
		})
	}
	if err := collect(allocPath, false); err != nil {
		return err
	}
	if err := collect(routePath, true); err != nil {
		return err
	}
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([][]any, 0, len(keys))
	for _, cidr := range keys {
		item := items[cidr]
		countries, _ := json.Marshal(facets(item.countries))
		asns, _ := json.Marshal(facets(item.asns))
		prefix, _ := netip.ParsePrefix(cidr)
		values = append(values, []any{cidr, 4, prefix.Bits(), item.allocations, item.routes, countries, asns})
	}
	_, err := conn.CopyFrom(ctx, pgx.Identifier{schema, "range_summaries"}, []string{"cidr", "ip_version", "prefix_length", "allocation_records", "route_records", "countries", "asns"}, pgx.CopyFromRows(values))
	return err
}

func publicViews(ctx context.Context, conn *pgx.Conn, schema string) error {
	s := quoteIdent(schema)
	_, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE public.bgp_api_dataset (singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton), release_tag TEXT NOT NULL, dataset_schema TEXT NOT NULL, built_at TEXT, source_commit TEXT, activated_at TIMESTAMPTZ NOT NULL DEFAULT now());
INSERT INTO public.bgp_api_dataset (release_tag,dataset_schema,built_at,source_commit) SELECT release_tag,%[2]s,built_at,source_commit FROM %[1]s.dataset_metadata;
CREATE VIEW public.lookup_prefixes AS SELECT source,prefix_key,prefix_length,start_ip_sort,end_ip_sort,ip_version,registry,country,netname,cidr,asn,region,city,status,allocation_date,created,last_modified,record_source,mnt_by,org,abuse_contact,description FROM %[1]s.lookup_prefixes;
CREATE VIEW public.allocation_objects AS SELECT id,start_ip_sort,end_ip_sort,ip_version,registry,country,netname,status,allocation_date,created,last_modified,record_source,mnt_by,org,abuse_contact,description FROM %[1]s.allocation_objects;
CREATE VIEW public.route_objects AS SELECT id,prefix,prefix_length,start_ip_sort,end_ip_sort,ip_version,origin_asn,asn_number,registry,record_source,mnt_by,org,abuse_contact,description FROM %[1]s.route_objects;
CREATE VIEW public.autnums AS SELECT id,asn,asn_number,registry,country,as_name,org,status,created,last_modified,record_source,mnt_by,abuse_contact,description FROM %[1]s.autnums;
CREATE VIEW public.range_summaries AS SELECT cidr,ip_version,prefix_length,allocation_records,route_records,countries,asns FROM %[1]s.range_summaries;
GRANT USAGE ON SCHEMA %[1]s TO bgp_api;
GRANT SELECT ON ALL TABLES IN SCHEMA %[1]s TO bgp_api;
GRANT USAGE ON SCHEMA public TO bgp_api;
GRANT SELECT ON public.bgp_api_dataset, public.lookup_prefixes, public.allocation_objects, public.route_objects, public.autnums, public.range_summaries TO bgp_api;`, s, quoteLiteral(schema)))
	return err
}

func facets(values map[string]int64) []facet {
	items := make([]facet, 0, len(values))
	for value, count := range values {
		items = append(items, facet{Value: value, RecordCount: count})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].RecordCount != items[right].RecordCount {
			return items[left].RecordCount > items[right].RecordCount
		}
		return items[left].Value < items[right].Value
	})
	if len(items) > 10 {
		return items[:10]
	}
	return items
}

func forEachCSV(path string, fn func([]string) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(row); err != nil {
			return err
		}
	}
}
func quoteIdent(value string) string   { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
func quoteLiteral(value string) string { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }
func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
func at(row []string, index int) string {
	if index >= len(row) {
		return ""
	}
	return row[index]
}
func number(value string) any {
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return n
	}
	return nil
}
func nullableASN(value string) any {
	value = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(value)), "AS")
	if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
		return n
	}
	return nil
}
