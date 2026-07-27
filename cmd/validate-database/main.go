package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	indexed := flag.Bool("indexed", false, "require lookup_prefixes")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: go run ./cmd/validate-database [-indexed] /path/to/mehrnet_bgp.db")
	}
	database, err := sql.Open("sqlite3", "file:"+flag.Arg(0)+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()

	tables := []string{"allocations", "routes", "autnums", "geolocations"}
	if *indexed {
		tables = append(tables, "lookup_prefixes")
	}
	for _, table := range tables {
		var count int64
		if err := database.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			log.Fatalf("validate %s: %v", table, err)
		}
		if count == 0 {
			log.Fatalf("validate %s: table is empty", table)
		}
		fmt.Printf("%s: %d\n", table, count)
	}
	requiredColumns := map[string][]string{
		"allocations":     {"abuse_contact"},
		"routes":          {"abuse_contact"},
		"autnums":         {"abuse_contact"},
		"lookup_prefixes": {"abuse_contact"},
	}
	for table, columns := range requiredColumns {
		if table == "lookup_prefixes" && !*indexed {
			continue
		}
		for _, column := range columns {
			if !hasColumn(database, table, column) {
				log.Fatalf("validate %s: missing required column %s", table, column)
			}
		}
	}
}

func hasColumn(database *sql.DB, table, column string) bool {
	rows, err := database.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		log.Fatalf("inspect %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			log.Fatalf("inspect %s: %v", table, err)
		}
		if strings.EqualFold(name, column) {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("inspect %s: %v", table, err)
	}
	return false
}
