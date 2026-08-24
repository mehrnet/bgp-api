package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

var summaryLengths = []uint{0, 8, 16}

type summaryKey struct {
	length uint
	bucket uint32
}

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
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: go run ./cmd/build-range-summaries /path/to/mehrnet_bgp.db")
	}
	database, err := sql.Open("sqlite3", flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("PRAGMA synchronous = OFF; PRAGMA journal_mode = MEMORY;"); err != nil {
		log.Fatal(err)
	}

	summaries := make(map[summaryKey]*summary)
	if err := collectAllocations(database, summaries); err != nil {
		log.Fatal(err)
	}
	if err := collectRoutes(database, summaries); err != nil {
		log.Fatal(err)
	}
	if err := persist(database, summaries); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("range summaries: %d rows\n", len(summaries))
}

func collectAllocations(database *sql.DB, summaries map[summaryKey]*summary) error {
	return collect(database, "SELECT start_ip_sort, end_ip_sort, country FROM allocations WHERE ip_version = 4", func(item *summary, value string) {
		item.allocations++
		if country := countryCode(value); country != "" {
			item.countries[country]++
		}
	}, summaries)
}

func collectRoutes(database *sql.DB, summaries map[summaryKey]*summary) error {
	return collect(database, "SELECT start_ip_sort, end_ip_sort, asn FROM routes WHERE ip_version = 4", func(item *summary, value string) {
		item.routes++
		if asn := canonicalASN(value); asn != "" {
			item.asns[asn]++
		}
	}, summaries)
}

func collect(database *sql.DB, query string, add func(*summary, string), summaries map[summaryKey]*summary) error {
	rows, err := database.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	var records int64
	for rows.Next() {
		var startRaw, endRaw string
		var value sql.NullString
		if err := rows.Scan(&startRaw, &endRaw, &value); err != nil {
			return err
		}
		start, err := ipv4Value(startRaw)
		if err != nil {
			return err
		}
		end, err := ipv4Value(endRaw)
		if err != nil {
			return err
		}
		if start > end {
			return fmt.Errorf("invalid IPv4 range %s-%s", startRaw, endRaw)
		}
		for _, length := range summaryLengths {
			shift := uint(32 - length)
			first, last := start>>shift, end>>shift
			for bucket := first; ; bucket++ {
				key := summaryKey{length: length, bucket: bucket}
				item := summaries[key]
				if item == nil {
					item = &summary{countries: make(map[string]int64), asns: make(map[string]int64)}
					summaries[key] = item
				}
				add(item, value.String)
				if bucket == last {
					break
				}
			}
		}
		records++
		if records%1000000 == 0 {
			fmt.Printf("range summaries: processed %d records\n", records)
		}
	}
	return rows.Err()
}

func persist(database *sql.DB, summaries map[summaryKey]*summary) error {
	if _, err := database.Exec(`
		DROP TABLE IF EXISTS range_summaries;
		CREATE TABLE range_summaries (
			cidr TEXT PRIMARY KEY,
			ip_version INTEGER NOT NULL,
			prefix_length INTEGER NOT NULL,
			allocation_records INTEGER NOT NULL,
			route_records INTEGER NOT NULL,
			countries TEXT NOT NULL,
			asns TEXT NOT NULL
		);
	`); err != nil {
		return err
	}
	keys := make([]summaryKey, 0, len(summaries))
	for key := range summaries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].length != keys[right].length {
			return keys[left].length < keys[right].length
		}
		return keys[left].bucket < keys[right].bucket
	})

	transaction, err := database.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	insert, err := transaction.Prepare(`INSERT INTO range_summaries (cidr, ip_version, prefix_length, allocation_records, route_records, countries, asns) VALUES (?, 4, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for _, key := range keys {
		item := summaries[key]
		countries, err := json.Marshal(topFacets(item.countries))
		if err != nil {
			return err
		}
		asns, err := json.Marshal(topFacets(item.asns))
		if err != nil {
			return err
		}
		if _, err := insert.Exec(cidr(key), key.length, item.allocations, item.routes, string(countries), string(asns)); err != nil {
			return err
		}
	}
	if _, err := transaction.Exec("ANALYZE range_summaries"); err != nil {
		return err
	}
	return transaction.Commit()
}

func ipv4Value(sortKey string) (uint32, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(sortKey), 10, 64)
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

func countryCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 2 {
		return ""
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return ""
		}
	}
	return value
}

func canonicalASN(value string) string {
	value = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(value)), "AS")
	if value == "" {
		return ""
	}
	if _, err := strconv.ParseUint(value, 10, 32); err != nil {
		return ""
	}
	return "AS" + value
}

func topFacets(values map[string]int64) []facet {
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

func cidr(key summaryKey) string {
	value := key.bucket << uint(32-key.length)
	return fmt.Sprintf("%d.%d.%d.%d/%d", value>>24, (value>>16)&255, (value>>8)&255, value&255, key.length)
}
