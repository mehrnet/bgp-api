// build-postgres-patch writes a replayable, logical PostgreSQL delta between
// two indexed SQLite datasets. It deliberately compares source records rather
// than SQLite pages, so the resulting patch is portable across PostgreSQL
// hosts and updates existing indexes incrementally.
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
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

type patchTable struct {
	name          string
	sourceTable   string
	sourceColumns []string
	targetColumns []string
	mapRow        func([]sql.NullString) ([]sql.NullString, error)
}

type patchCounts struct {
	deleted  int
	inserted int
}

func main() {
	basePath := flag.String("base", "", "previous indexed SQLite database")
	targetPath := flag.String("target", "", "new indexed SQLite database")
	outputPath := flag.String("output", "", "PostgreSQL patch .sql.gz")
	baseRelease := flag.String("base-release", "", "previous release tag")
	targetRelease := flag.String("target-release", "", "new release tag")
	flag.Parse()
	if *basePath == "" || *targetPath == "" || *outputPath == "" || *baseRelease == "" || *targetRelease == "" {
		log.Fatal("usage: go run ./cmd/build-postgres-patch -base old.db -target new.db -output patch.sql.gz -base-release db-... -target-release db-...")
	}

	database, err := sql.Open("sqlite3", "file:"+*targetPath+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("ATTACH DATABASE ? AS base", *basePath); err != nil {
		log.Fatalf("attach base dataset: %v", err)
	}
	defer database.Exec("DETACH DATABASE base")

	output, err := os.Create(*outputPath)
	if err != nil {
		log.Fatal(err)
	}
	failed := true
	defer func() {
		if err := output.Close(); err != nil {
			log.Printf("close patch: %v", err)
		}
		if failed {
			_ = os.Remove(*outputPath)
		}
	}()
	gzipWriter, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		log.Fatal(err)
	}
	writer := bufio.NewWriterSize(gzipWriter, 1024*1024)

	if _, err := fmt.Fprintf(writer, "-- MehrNet BGP logical PostgreSQL patch\n-- base_release: %s\n-- target_release: %s\n-- Apply with: psql --set dataset_schema=<active schema> --set ON_ERROR_STOP=1\nBEGIN;\n", *baseRelease, *targetRelease); err != nil {
		log.Fatal(err)
	}
	tables := []patchTable{
		{
			name:          "lookup_prefixes",
			sourceTable:   "lookup_prefixes",
			sourceColumns: []string{"source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"},
			targetColumns: []string{"source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"},
		},
		{
			name:          "allocation_objects",
			sourceTable:   "allocations",
			sourceColumns: []string{"start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "status", "allocation_date", "created", "last_modified", "source", "mnt_by", "org", "abuse_contact", "descr"},
			targetColumns: []string{"start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"},
		},
		{
			name:          "route_objects",
			sourceTable:   "routes",
			sourceColumns: []string{"cidr", "start_ip_sort", "end_ip_sort", "ip_version", "asn", "registry", "source", "mnt_by", "org", "abuse_contact", "descr"},
			targetColumns: []string{"prefix", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "origin_asn", "asn_number", "registry", "record_source", "mnt_by", "org", "abuse_contact", "description"},
			mapRow:        routeObjectRow,
		},
		{
			name:          "autnums",
			sourceTable:   "autnums",
			sourceColumns: []string{"asn", "registry", "country", "as_name", "org", "status", "created", "last_modified", "source", "mnt_by", "abuse_contact", "descr"},
			targetColumns: []string{"asn", "asn_number", "registry", "country", "as_name", "org", "status", "created", "last_modified", "record_source", "mnt_by", "abuse_contact", "description"},
			mapRow:        autnumRow,
		},
		{
			name:          "range_summaries",
			sourceTable:   "range_summaries",
			sourceColumns: []string{"cidr", "ip_version", "prefix_length", "allocation_records", "route_records", "countries", "asns"},
			targetColumns: []string{"cidr", "ip_version", "prefix_length", "allocation_records", "route_records", "countries", "asns"},
		},
	}

	for _, table := range tables {
		counts, err := writeTablePatch(database, writer, table)
		if err != nil {
			log.Fatalf("build %s patch: %v", table.name, err)
		}
		fmt.Printf("%s: %d deleted, %d inserted\n", table.name, counts.deleted, counts.inserted)
	}
	if _, err := fmt.Fprintln(writer, "ANALYZE :\"dataset_schema\".lookup_prefixes;\nANALYZE :\"dataset_schema\".allocation_objects;\nANALYZE :\"dataset_schema\".route_objects;\nANALYZE :\"dataset_schema\".autnums;\nANALYZE :\"dataset_schema\".range_summaries;\nCOMMIT;"); err != nil {
		log.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		log.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		log.Fatal(err)
	}
	failed = false
}

func writeTablePatch(database *sql.DB, writer *bufio.Writer, table patchTable) (patchCounts, error) {
	deleted, err := writeDelta(database, writer, table, "deleted", "base", "main")
	if err != nil {
		return patchCounts{}, err
	}
	if deleted > 0 {
		if _, err := fmt.Fprintf(writer, "DELETE FROM :\"dataset_schema\".%s AS target USING patch_%s_deleted AS patch WHERE %s;\nDROP TABLE patch_%s_deleted;\n", table.name, table.name, nullSafeMatch("target", "patch", table.targetColumns), table.name); err != nil {
			return patchCounts{}, err
		}
	}
	inserted, err := writeDelta(database, writer, table, "inserted", "main", "base")
	if err != nil {
		return patchCounts{}, err
	}
	if inserted > 0 {
		columns := strings.Join(table.targetColumns, ", ")
		if _, err := fmt.Fprintf(writer, "INSERT INTO :\"dataset_schema\".%s (%s) SELECT %s FROM patch_%s_inserted;\nDROP TABLE patch_%s_inserted;\n", table.name, columns, columns, table.name, table.name); err != nil {
			return patchCounts{}, err
		}
	}
	return patchCounts{deleted: deleted, inserted: inserted}, nil
}

func writeDelta(database *sql.DB, writer *bufio.Writer, table patchTable, direction, leftSchema, rightSchema string) (int, error) {
	columns := strings.Join(table.sourceColumns, ", ")
	query := fmt.Sprintf("SELECT %s FROM %s.%s EXCEPT SELECT %s FROM %s.%s", columns, leftSchema, table.sourceTable, columns, rightSchema, table.sourceTable)
	rows, err := database.Query(query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	if _, err := fmt.Fprintf(writer, "CREATE TEMP TABLE patch_%s_%s (LIKE :\"dataset_schema\".%s INCLUDING DEFAULTS) ON COMMIT DROP;\nCOPY patch_%s_%s (%s) FROM STDIN WITH (FORMAT text);\n", table.name, direction, table.name, table.name, direction, strings.Join(table.targetColumns, ", ")); err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		input := make([]sql.NullString, len(table.sourceColumns))
		destinations := make([]any, len(input))
		for index := range input {
			destinations[index] = &input[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return count, err
		}
		values := input
		if table.mapRow != nil {
			values, err = table.mapRow(input)
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
		if _, err := fmt.Fprintln(writer, strings.Join(fields, "\t")); err != nil {
			return count, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	if _, err := fmt.Fprintln(writer, "\\."); err != nil {
		return count, err
	}
	if count == 0 {
		if _, err := fmt.Fprintf(writer, "DROP TABLE patch_%s_%s;\n", table.name, direction); err != nil {
			return count, err
		}
	}
	return count, nil
}

func nullSafeMatch(left, right string, columns []string) string {
	conditions := make([]string, len(columns))
	for index, column := range columns {
		conditions[index] = fmt.Sprintf("%s.%s IS NOT DISTINCT FROM %s.%s", left, column, right, column)
	}
	return strings.Join(conditions, " AND ")
}

func routeObjectRow(input []sql.NullString) ([]sql.NullString, error) {
	if len(input) != 11 || !input[0].Valid || !input[4].Valid {
		return nil, fmt.Errorf("invalid route row")
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(input[0].String))
	if err != nil {
		return nil, fmt.Errorf("parse route prefix %q: %w", input[0].String, err)
	}
	return []sql.NullString{{String: prefix.Masked().String(), Valid: true}, {String: strconv.Itoa(prefix.Bits()), Valid: true}, input[1], input[2], input[3], input[4], nullableASN(input[4]), input[5], input[6], input[7], input[8], input[9], input[10]}, nil
}

func autnumRow(input []sql.NullString) ([]sql.NullString, error) {
	if len(input) != 12 || !input[0].Valid {
		return nil, fmt.Errorf("invalid autnum row")
	}
	return []sql.NullString{input[0], nullableASN(input[0]), input[1], input[2], input[3], input[4], input[5], input[6], input[7], input[8], input[9], input[10], input[11]}, nil
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
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.ReplaceAll(value, "\x00", "\uFFFD")
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\t", `\t`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, "\r", `\r`)
}
