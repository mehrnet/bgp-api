package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mehrnet/bgp-api/internal/ipkey"
)

const batchSize = 1000

type sourceDefinition struct {
	source string
	query  string
}

type sourceRow struct {
	rowID              int64
	start, end         string
	version            int
	registry, country  sql.NullString
	netname, cidr, asn sql.NullString
	region, city       sql.NullString
}

func main() {
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: go run ./cmd/build-prefix-index /path/to/mehrnet_bgp.db")
	}
	database, err := sql.Open("sqlite3", flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("PRAGMA synchronous = OFF; PRAGMA journal_mode = MEMORY;"); err != nil {
		log.Fatal(err)
	}

	if _, err := database.Exec(`
		DROP TABLE IF EXISTS lookup_prefixes;
		CREATE TABLE lookup_prefixes (
			source TEXT NOT NULL,
			prefix_key TEXT NOT NULL,
			prefix_length INTEGER NOT NULL,
			start_ip_sort TEXT NOT NULL,
			end_ip_sort TEXT NOT NULL,
			ip_version INTEGER NOT NULL,
			registry TEXT, country TEXT, netname TEXT, cidr TEXT, asn TEXT, region TEXT, city TEXT
		);
	`); err != nil {
		log.Fatal(err)
	}

	definitions := []sourceDefinition{
		{"allocation", "SELECT rowid, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, NULL, NULL, NULL, NULL FROM allocations WHERE rowid > ? ORDER BY rowid LIMIT ?"},
		{"route", "SELECT rowid, start_ip_sort, end_ip_sort, ip_version, NULL, NULL, NULL, cidr, asn, NULL, NULL FROM routes WHERE rowid > ? ORDER BY rowid LIMIT ?"},
		{"geofeed", "SELECT rowid, start_ip_sort, end_ip_sort, ip_version, NULL, country, NULL, NULL, NULL, region, city FROM geolocations WHERE rowid > ? ORDER BY rowid LIMIT ?"},
	}
	for _, definition := range definitions {
		if err := materialize(database, definition); err != nil {
			log.Fatalf("%s: %v", definition.source, err)
		}
	}
	if _, err := database.Exec(`
		DROP INDEX IF EXISTS idx_alloc;
		DROP INDEX IF EXISTS idx_routes;
		DROP INDEX IF EXISTS idx_geo;
		CREATE INDEX IF NOT EXISTS idx_alloc_by_start ON allocations(ip_version, start_ip_sort);
		CREATE INDEX IF NOT EXISTS idx_alloc_by_end ON allocations(ip_version, end_ip_sort);
		CREATE INDEX IF NOT EXISTS idx_routes_by_start ON routes(ip_version, start_ip_sort);
		CREATE INDEX IF NOT EXISTS idx_routes_by_end ON routes(ip_version, end_ip_sort);
		CREATE INDEX IF NOT EXISTS idx_geo_by_start ON geolocations(ip_version, start_ip_sort);
		CREATE INDEX IF NOT EXISTS idx_geo_by_end ON geolocations(ip_version, end_ip_sort);
		CREATE INDEX idx_lookup_prefix ON lookup_prefixes(source, prefix_key);
		ANALYZE;
	`); err != nil {
		log.Fatal(err)
	}
}

func materialize(database *sql.DB, definition sourceDefinition) error {
	lastRowID := int64(0)
	records, prefixes := 0, 0
	for {
		rows, err := database.Query(definition.query, lastRowID, batchSize)
		if err != nil {
			return err
		}
		batch := make([]sourceRow, 0, batchSize)
		for rows.Next() {
			row := sourceRow{}
			if err := rows.Scan(&row.rowID, &row.start, &row.end, &row.version, &row.registry, &row.country, &row.netname, &row.cidr, &row.asn, &row.region, &row.city); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}

		transaction, err := database.Begin()
		if err != nil {
			return err
		}
		insert, err := transaction.Prepare(`INSERT INTO lookup_prefixes (source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			transaction.Rollback()
			return err
		}
		for _, row := range batch {
			covers, err := ipkey.RangeToPrefixes(row.start, row.end, row.version)
			if err != nil {
				insert.Close()
				transaction.Rollback()
				return err
			}
			for _, cover := range covers {
				if _, err := insert.Exec(definition.source, cover.Key, cover.Length, row.start, row.end, row.version, nullValue(row.registry), nullValue(row.country), nullValue(row.netname), nullValue(row.cidr), nullValue(row.asn), nullValue(row.region), nullValue(row.city)); err != nil {
					insert.Close()
					transaction.Rollback()
					return err
				}
				prefixes++
			}
		}
		insert.Close()
		if err := transaction.Commit(); err != nil {
			return err
		}
		lastRowID = batch[len(batch)-1].rowID
		records += len(batch)
		if records%100000 == 0 || len(batch) < batchSize {
			fmt.Printf("%s: %d records, %d prefix rows\n", definition.source, records, prefixes)
		}
	}
}

func nullValue(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}
