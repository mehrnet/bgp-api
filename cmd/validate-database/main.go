package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"

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
}
