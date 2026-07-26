package main

import (
	"bufio"
	"compress/gzip"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

var schemaPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func main() {
	databasePath := flag.String("database", "", "indexed SQLite database")
	outputPath := flag.String("output", "", "PostgreSQL dump .sql.gz")
	schema := flag.String("schema", "", "destination PostgreSQL schema")
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

	columns := []string{"source", "prefix_key", "prefix_length", "start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "cidr", "asn", "region", "city"}
	fmt.Fprintf(writer, "SET client_encoding = 'UTF8';\nCREATE SCHEMA %s;\n", *schema)
	fmt.Fprintf(writer, "CREATE TABLE %s.lookup_prefixes (source TEXT NOT NULL, prefix_key TEXT NOT NULL, prefix_length INTEGER NOT NULL, start_ip_sort TEXT NOT NULL, end_ip_sort TEXT NOT NULL, ip_version INTEGER NOT NULL, registry TEXT, country TEXT, netname TEXT, cidr TEXT, asn TEXT, region TEXT, city TEXT);\n", *schema)
	fmt.Fprintf(writer, "COPY %s.lookup_prefixes (%s) FROM STDIN WITH (FORMAT text);\n", *schema, strings.Join(columns, ", "))

	rows, err := database.Query("SELECT " + strings.Join(columns, ", ") + " FROM lookup_prefixes")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	count := 0
	values := make([]sql.NullString, len(columns))
	scan := make([]any, len(columns))
	for index := range values {
		scan[index] = &values[index]
	}
	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			log.Fatal(err)
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
			fmt.Printf("lookup_prefixes: %d rows\n", count)
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Fprint(writer, "\\.\n")
	fmt.Fprintf(writer, "CREATE INDEX idx_lookup_prefix ON %s.lookup_prefixes (source, prefix_key);\nANALYZE %s.lookup_prefixes;\n", *schema, *schema)
	fmt.Printf("lookup_prefixes: %d rows\n", count)
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
