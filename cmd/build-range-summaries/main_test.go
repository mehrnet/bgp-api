package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mehrnet/bgp-api/internal/ipkey"
)

func TestBuildRangeSummariesAggregatesFixedBuckets(t *testing.T) {
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "dataset.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE allocations (start_ip_sort TEXT, end_ip_sort TEXT, ip_version INTEGER, country TEXT);
		CREATE TABLE routes (start_ip_sort TEXT, end_ip_sort TEXT, ip_version INTEGER, asn TEXT);
	`); err != nil {
		t.Fatal(err)
	}
	insertRange(t, database, "allocations", "country", "80.0.0.0", "80.255.255.255", "AU")
	insertRange(t, database, "allocations", "country", "80.1.0.0", "80.1.255.255", "GB")
	insertRange(t, database, "routes", "asn", "80.0.0.0", "80.255.255.255", "AS1")
	insertRange(t, database, "routes", "asn", "80.1.0.0", "80.1.255.255", "AS2")

	summaries := make(map[summaryKey]*summary)
	if err := collectAllocations(database, summaries); err != nil {
		t.Fatal(err)
	}
	if err := collectRoutes(database, summaries); err != nil {
		t.Fatal(err)
	}
	if err := persist(database, summaries); err != nil {
		t.Fatal(err)
	}

	var allocations, routes int64
	var countries, asns string
	if err := database.QueryRow(`SELECT allocation_records, route_records, countries, asns FROM range_summaries WHERE cidr = '80.1.0.0/16'`).Scan(&allocations, &routes, &countries, &asns); err != nil {
		t.Fatal(err)
	}
	if allocations != 2 || routes != 2 {
		t.Fatalf("80.1.0.0/16 counts = allocations %d routes %d", allocations, routes)
	}
	if countries != `[{"value":"AU","record_count":1},{"value":"GB","record_count":1}]` {
		t.Fatalf("countries = %s", countries)
	}
	if asns != `[{"value":"AS1","record_count":1},{"value":"AS2","record_count":1}]` {
		t.Fatalf("ASNs = %s", asns)
	}
}

func insertRange(t *testing.T, database *sql.DB, table, column, start, end, value string) {
	t.Helper()
	startIP, ok := ipkey.Parse(start)
	if !ok {
		t.Fatalf("parse start %s", start)
	}
	endIP, ok := ipkey.Parse(end)
	if !ok {
		t.Fatalf("parse end %s", end)
	}
	query := "INSERT INTO " + table + " (start_ip_sort, end_ip_sort, ip_version, " + column + ") VALUES (?, ?, 4, ?)"
	if _, err := database.Exec(query, startIP.SortKey, endIP.SortKey, value); err != nil {
		t.Fatal(err)
	}
}
