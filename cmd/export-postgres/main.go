package main

import (
	"bufio"
	"compress/gzip"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

var schemaPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type tableExport struct {
	name    string
	columns []string
	query   string
	mapRow  func([]sql.NullString) ([]sql.NullString, error)
}

func main() {
	databasePath := flag.String("database", "", "indexed SQLite database")
	outputPath := flag.String("output", "", "PostgreSQL dump .sql.gz")
	schema := flag.String("schema", "", "destination PostgreSQL schema")
	releaseTag := flag.String("release-tag", "", "release tag for dataset metadata")
	builtAt := flag.String("built-at", "", "UTC build timestamp for dataset metadata")
	sourceCommit := flag.String("source-commit", "", "producer source commit for dataset metadata")
	flag.Parse()
	if *databasePath == "" || *outputPath == "" || !schemaPattern.MatchString(*schema) {
		log.Fatal("usage: go run ./cmd/export-postgres -database mehrnet_bgp.db -output release/mehrnet_bgp_postgres.sql.gz -schema bgp_YYYYMMDD_HHMM")
	}
	database, err := sql.Open("sqlite3", "file:"+*databasePath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()

	file, err := os.Create(*outputPath)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	gzipWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		log.Fatal(err)
	}
	writer := bufio.NewWriterSize(gzipWriter, 1024*1024)
	defer func() {
		if err := writer.Flush(); err != nil {
			log.Fatal(err)
		}
		if err := gzipWriter.Close(); err != nil {
			log.Fatal(err)
		}
	}()

	fmt.Fprintf(writer, "SET client_encoding = 'UTF8';\nCREATE SCHEMA %s;\n", *schema)
	writeSchema(writer, *schema)
	writeDatasetMetadata(writer, *schema, *releaseTag, *builtAt, *sourceCommit)

	exports := []tableExport{
		{
			name:    "lookup_prefixes",
			columns: []string{"source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"},
			query:   "SELECT source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city, status, allocation_date, created, last_modified, record_source, mnt_by, org, abuse_contact, description FROM lookup_prefixes",
		},
		{
			name:    "allocation_objects",
			columns: []string{"start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"},
			query:   "SELECT start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date, created, last_modified, source, mnt_by, org, abuse_contact, descr FROM allocations",
		},
		{
			name:    "route_objects",
			columns: []string{"prefix", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "origin_asn", "asn_number", "registry", "record_source", "mnt_by", "org", "abuse_contact", "description"},
			query:   "SELECT cidr, start_ip_sort, end_ip_sort, ip_version, asn, registry, source, mnt_by, org, abuse_contact, descr FROM routes",
			mapRow:  routeObjectRow,
		},
		{
			name:    "autnums",
			columns: []string{"asn", "asn_number", "registry", "country", "as_name", "org", "status", "created", "last_modified", "record_source", "mnt_by", "abuse_contact", "description"},
			query:   "SELECT asn, registry, country, as_name, org, status, created, last_modified, source, mnt_by, abuse_contact, descr FROM autnums",
			mapRow:  autnumRow,
		},
		{
			name:    "range_summaries",
			columns: []string{"cidr", "ip_version", "prefix_length", "allocation_records", "route_records", "countries", "asns"},
			query:   "SELECT cidr, ip_version, prefix_length, allocation_records, route_records, countries, asns FROM range_summaries",
		},
	}
	for _, export := range exports {
		count, err := copyTable(database, writer, *schema, export)
		if err != nil {
			log.Fatalf("export %s: %v", export.name, err)
		}
		fmt.Printf("%s: %d rows\n", export.name, count)
	}
	writeIndexes(writer, *schema)
}

func writeSchema(writer *bufio.Writer, schema string) {
	fmt.Fprintf(writer, `
CREATE TABLE %s.lookup_prefixes (
  source TEXT NOT NULL, prefix_key TEXT NOT NULL, prefix_length INTEGER NOT NULL,
  start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version SMALLINT NOT NULL,
  registry TEXT, country TEXT, netname TEXT, cidr TEXT, asn TEXT, region TEXT, city TEXT,
  status TEXT, allocation_date TEXT, created TEXT, last_modified TEXT,
  record_source TEXT, mnt_by TEXT, org TEXT, abuse_contact TEXT, description TEXT
);
CREATE TABLE %s.allocation_objects (
  id BIGSERIAL PRIMARY KEY,
  start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version SMALLINT NOT NULL,
  registry TEXT, country TEXT, netname TEXT, status TEXT, allocation_date TEXT,
  created TEXT, last_modified TEXT, record_source TEXT, mnt_by TEXT, org TEXT, abuse_contact TEXT, description TEXT
);
CREATE TABLE %s.route_objects (
  id BIGSERIAL PRIMARY KEY,
  prefix CIDR NOT NULL, prefix_length SMALLINT NOT NULL,
  start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version SMALLINT NOT NULL,
  origin_asn TEXT NOT NULL, asn_number BIGINT,
  registry TEXT, record_source TEXT, mnt_by TEXT, org TEXT, abuse_contact TEXT, description TEXT
);
CREATE TABLE %s.autnums (
  id BIGSERIAL PRIMARY KEY,
  asn TEXT NOT NULL, asn_number BIGINT, registry TEXT, country TEXT, as_name TEXT,
  org TEXT, status TEXT, created TEXT, last_modified TEXT, record_source TEXT, mnt_by TEXT, abuse_contact TEXT, description TEXT
);
CREATE TABLE %s.dataset_metadata (
  release_tag TEXT, built_at TEXT, source_commit TEXT
);
CREATE TABLE %s.range_summaries (
  cidr CIDR PRIMARY KEY, ip_version SMALLINT NOT NULL, prefix_length SMALLINT NOT NULL,
  allocation_records BIGINT NOT NULL, route_records BIGINT NOT NULL,
  countries JSONB NOT NULL, asns JSONB NOT NULL
);
`, schema, schema, schema, schema, schema, schema)
}

func writeDatasetMetadata(writer *bufio.Writer, schema, releaseTag, builtAt, sourceCommit string) {
	fmt.Fprintf(writer, "COPY %s.dataset_metadata (release_tag, built_at, source_commit) FROM STDIN WITH (FORMAT text);\n", schema)
	fmt.Fprintf(writer, "%s\t%s\t%s\n\\.\n", copyNullable(releaseTag), copyNullable(builtAt), copyNullable(sourceCommit))
}

func writeIndexes(writer *bufio.Writer, schema string) {
	fmt.Fprintf(writer, `
CREATE INDEX idx_lookup_prefix ON %s.lookup_prefixes (source, prefix_key);
CREATE INDEX idx_route_objects_prefix ON %s.route_objects USING SPGIST (prefix inet_ops);
CREATE INDEX idx_allocation_objects_overlap ON %s.allocation_objects USING GIST (numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]'));
CREATE INDEX idx_route_objects_overlap ON %s.route_objects USING GIST (numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]'));
CREATE INDEX idx_route_objects_asn_id ON %s.route_objects (asn_number, id) WHERE asn_number IS NOT NULL;
CREATE INDEX idx_autnums_asn_number ON %s.autnums (asn_number) WHERE asn_number IS NOT NULL;
ANALYZE %s.lookup_prefixes;
ANALYZE %s.allocation_objects;
ANALYZE %s.route_objects;
ANALYZE %s.autnums;
ANALYZE %s.range_summaries;
`, schema, schema, schema, schema, schema, schema, schema, schema, schema, schema, schema)
}

func copyTable(database *sql.DB, writer *bufio.Writer, schema string, export tableExport) (int, error) {
	fmt.Fprintf(writer, "COPY %s.%s (%s) FROM STDIN WITH (FORMAT text);\n", schema, export.name, strings.Join(export.columns, ", "))
	rows, err := database.Query(export.query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		values := make([]sql.NullString, len(export.columns))
		inputLength := len(values)
		if export.mapRow != nil {
			switch export.name {
			case "route_objects":
				inputLength = 11
			case "autnums":
				inputLength = 12
			}
		}
		input := make([]sql.NullString, inputLength)
		scan := make([]any, len(input))
		for index := range input {
			scan[index] = &input[index]
		}
		if err := rows.Scan(scan...); err != nil {
			return count, err
		}
		if export.mapRow == nil {
			values = input
		} else {
			values, err = export.mapRow(input)
			if err != nil {
				return count, err
			}
		}
		fields := make([]string, len(values))
		for index, value := range values {
			if value.Valid {
				fields[index] = copyValue(value.String)
			} else {
				fields[index] = `\N`
			}
		}
		fmt.Fprintln(writer, strings.Join(fields, "\t"))
		count++
		if count%1000000 == 0 {
			fmt.Printf("%s: %d rows\n", export.name, count)
		}
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	fmt.Fprint(writer, "\\.\n")
	return count, nil
}

func routeObjectRow(input []sql.NullString) ([]sql.NullString, error) {
	if len(input) != 11 || !input[0].Valid || !input[4].Valid {
		return nil, fmt.Errorf("invalid route row")
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(input[0].String))
	if err != nil {
		return nil, fmt.Errorf("parse route prefix %q: %w", input[0].String, err)
	}
	asnNumber := nullableASN(input[4])
	return []sql.NullString{
		{String: prefix.Masked().String(), Valid: true},
		{String: strconv.Itoa(prefix.Bits()), Valid: true},
		input[1], input[2], input[3], input[4], asnNumber, input[5], input[6], input[7], input[8], input[9], input[10],
	}, nil
}

func autnumRow(input []sql.NullString) ([]sql.NullString, error) {
	if len(input) != 12 || !input[0].Valid {
		return nil, fmt.Errorf("invalid autnum row")
	}
	return []sql.NullString{
		input[0], nullableASN(input[0]), input[1], input[2], input[3], input[4], input[5], input[6], input[7], input[8], input[9], input[10], input[11],
	}, nil
}

func nullableASN(value sql.NullString) sql.NullString {
	if !value.Valid {
		return sql.NullString{}
	}
	number, err := strconv.ParseUint(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(value.String)), "AS"), 10, 32)
	if err != nil || number == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: strconv.FormatUint(number, 10), Valid: true}
}

func copyValue(value string) string {
	// PostgreSQL TEXT accepts only valid UTF-8 and cannot store NUL bytes.
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.ReplaceAll(value, "\x00", "\uFFFD")
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\t", `\t`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, "\r", `\r`)
}

func copyNullable(value string) string {
	if strings.TrimSpace(value) == "" {
		return `\N`
	}
	return copyValue(value)
}
