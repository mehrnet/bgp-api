package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

type patchTable struct {
	name          string
	columns, keys []string
}

var tables = []patchTable{
	{"lookup_prefixes", []string{"source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"}, []string{"source", "prefix_key"}},
	{"allocation_objects", []string{"start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"}, []string{"start_ip_sort", "end_ip_sort", "ip_version"}},
	{"route_objects", []string{"prefix", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "origin_asn", "asn_number", "registry", "record_source", "mnt_by", "org", "abuse_contact", "description"}, []string{"prefix", "origin_asn", "registry"}},
	{"autnums", []string{"asn", "asn_number", "registry", "country", "as_name", "org", "status", "created", "last_modified", "record_source", "mnt_by", "abuse_contact", "description"}, []string{"asn", "registry"}},
	{"range_summaries", []string{"cidr", "ip_version", "prefix_length", "allocation_records", "route_records", "countries", "asns"}, []string{"cidr"}},
}

func main() {
	dsn := flag.String("database-url", "", "PostgreSQL connection URL")
	base := flag.String("base-schema", "", "previous dataset schema")
	target := flag.String("target-schema", "", "new dataset schema")
	output := flag.String("output", "", "gzip SQL patch path")
	baseRelease := flag.String("base-release", "", "previous release")
	targetRelease := flag.String("target-release", "", "new release")
	flag.Parse()
	if *dsn == "" || *base == "" || *target == "" || *output == "" || *baseRelease == "" || *targetRelease == "" {
		log.Fatal("missing patch arguments")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET work_mem = '512MB'; SET maintenance_work_mem = '1GB'"); err != nil {
		log.Fatal(err)
	}
	file, err := os.Create(*output)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		log.Fatal(err)
	}
	writer := bufio.NewWriterSize(gz, 1024*1024)
	fmt.Fprintf(writer, "-- MehrNet BGP logical PostgreSQL patch\n-- base_release: %s\n-- target_release: %s\nBEGIN;\n", *baseRelease, *targetRelease)
	for _, table := range tables {
		if err := writeTable(ctx, conn, writer, *base, *target, table); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Fprintln(writer, "ANALYZE :\"dataset_schema\".lookup_prefixes; ANALYZE :\"dataset_schema\".allocation_objects; ANALYZE :\"dataset_schema\".route_objects; ANALYZE :\"dataset_schema\".autnums; ANALYZE :\"dataset_schema\".range_summaries; COMMIT;")
	if err := writer.Flush(); err != nil {
		log.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		log.Fatal(err)
	}
}

func writeTable(ctx context.Context, conn *pgx.Conn, writer *bufio.Writer, base, target string, table patchTable) error {
	columns := strings.Join(table.columns, ", ")
	deleted, err := writeDelta(ctx, conn, writer, base, target, table, columns, "deleted")
	if err != nil {
		return err
	}
	if deleted > 0 {
		fmt.Fprintf(writer, "CREATE INDEX ON patch_%s_deleted (%s); ANALYZE patch_%s_deleted; DELETE FROM :\"dataset_schema\".%s AS target USING patch_%s_deleted AS patch WHERE %s; DROP TABLE patch_%s_deleted;\n", table.name, strings.Join(table.keys, ", "), table.name, table.name, table.name, matchClause(table, "target", "patch"), table.name)
	}
	inserted, err := writeDelta(ctx, conn, writer, target, base, table, columns, "inserted")
	if err != nil {
		return err
	}
	if inserted > 0 {
		fmt.Fprintf(writer, "INSERT INTO :\"dataset_schema\".%s (%s) SELECT %s FROM patch_%s_inserted; DROP TABLE patch_%s_inserted;\n", table.name, columns, columns, table.name, table.name)
	}
	return nil
}

func writeDelta(ctx context.Context, conn *pgx.Conn, writer *bufio.Writer, left, right string, table patchTable, columns, direction string) (int, error) {
	query := fmt.Sprintf("SELECT %s FROM %s.%s EXCEPT SELECT %s FROM %s.%s", columns, quote(left), table.name, columns, quote(right), table.name)
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	fmt.Fprintf(writer, "CREATE TEMP TABLE patch_%s_%s (LIKE :\"dataset_schema\".%s INCLUDING DEFAULTS) ON COMMIT DROP; COPY patch_%s_%s (%s) FROM STDIN WITH (FORMAT text);\n", table.name, direction, table.name, table.name, direction, columns)
	count := 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return count, err
		}
		fields := make([]string, len(values))
		for i, value := range values {
			fields[i] = copyValue(value)
		}
		fmt.Fprintln(writer, strings.Join(fields, "\t"))
		count++
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	fmt.Fprintln(writer, "\\.")
	if count == 0 {
		fmt.Fprintf(writer, "DROP TABLE patch_%s_%s;\n", table.name, direction)
	}
	return count, nil
}

func matchClause(table patchTable, left, right string) string {
	key := map[string]bool{}
	for _, value := range table.keys {
		key[value] = true
	}
	conditions := make([]string, 0, len(table.columns))
	for _, value := range table.columns {
		if key[value] {
			conditions = append(conditions, fmt.Sprintf("%s.%s = %s.%s", left, value, right, value))
		} else {
			conditions = append(conditions, fmt.Sprintf("%s.%s IS NOT DISTINCT FROM %s.%s", left, value, right, value))
		}
	}
	return strings.Join(conditions, " AND ")
}
func quote(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
func copyValue(value any) string {
	if value == nil {
		return `\N`
	}
	var text string
	switch value := value.(type) {
	case []byte:
		text = string(value)
	default:
		text = fmt.Sprint(value)
	}
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, "\t", `\t`)
	text = strings.ReplaceAll(text, "\n", `\n`)
	return strings.ReplaceAll(text, "\r", `\r`)
}
