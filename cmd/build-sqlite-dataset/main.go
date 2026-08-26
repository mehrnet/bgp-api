package main

import (
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const batchSize = 10000

type importSpec struct {
	name        string
	path        string
	fields      int
	create      string
	insert      string
	required    []int
	reportEvery int64
}

func main() {
	input := flag.String("input", "final_data", "directory containing allocations.csv, routes.csv, autnums.csv, and geolocations.csv")
	output := flag.String("output", "", "output SQLite database path")
	releaseTag := flag.String("release-tag", "", "dataset release tag")
	builtAt := flag.String("built-at", "", "dataset build timestamp")
	sourceCommit := flag.String("source-commit", "", "producer source commit")
	flag.Parse()
	if *output == "" || *releaseTag == "" || *builtAt == "" {
		log.Fatal("usage: build-sqlite-dataset -input final_data -output mehrnet_bgp.db -release-tag db-... -built-at ... [-source-commit ...]")
	}
	if err := os.Remove(*output); err != nil && !os.IsNotExist(err) {
		log.Fatal(err)
	}

	database, err := sql.Open("sqlite3", *output)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		PRAGMA journal_mode = OFF;
		PRAGMA synchronous = OFF;
		PRAGMA temp_store = MEMORY;
		PRAGMA locking_mode = EXCLUSIVE;
		PRAGMA cache_size = -524288;
	`); err != nil {
		log.Fatal(err)
	}
	if err := createMetadata(database, *releaseTag, *builtAt, *sourceCommit); err != nil {
		log.Fatal(err)
	}

	specs := []importSpec{
		{
			name: "allocations", path: filepath.Join(*input, "allocations.csv"), fields: 15, required: []int{0, 1, 2},
			create: `CREATE TABLE allocations (
				start_ip_sort TEXT NOT NULL,
				end_ip_sort TEXT NOT NULL,
				ip_version INTEGER NOT NULL,
				registry TEXT,
				country TEXT,
				netname TEXT,
				status TEXT,
				allocation_date TEXT,
				created TEXT,
				last_modified TEXT,
				source TEXT,
				mnt_by TEXT,
				org TEXT,
				abuse_contact TEXT,
				descr TEXT
			);`,
			insert:      `INSERT INTO allocations (start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date, created, last_modified, source, mnt_by, org, abuse_contact, descr) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			reportEvery: 1000000,
		},
		{
			name: "routes", path: filepath.Join(*input, "routes.csv"), fields: 11, required: []int{0, 1, 2, 3, 4},
			create: `CREATE TABLE routes (
				start_ip_sort TEXT NOT NULL,
				end_ip_sort TEXT NOT NULL,
				ip_version INTEGER NOT NULL,
				cidr TEXT NOT NULL,
				asn TEXT NOT NULL,
				registry TEXT,
				source TEXT,
				mnt_by TEXT,
				org TEXT,
				abuse_contact TEXT,
				descr TEXT
			);`,
			insert:      `INSERT INTO routes (start_ip_sort, end_ip_sort, ip_version, cidr, asn, registry, source, mnt_by, org, abuse_contact, descr) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			reportEvery: 500000,
		},
		{
			name: "autnums", path: filepath.Join(*input, "autnums.csv"), fields: 12, required: []int{0},
			create: `CREATE TABLE autnums (
				asn TEXT NOT NULL,
				registry TEXT,
				country TEXT,
				as_name TEXT,
				org TEXT,
				status TEXT,
				created TEXT,
				last_modified TEXT,
				source TEXT,
				mnt_by TEXT,
				abuse_contact TEXT,
				descr TEXT
			);`,
			insert:      `INSERT INTO autnums (asn, registry, country, as_name, org, status, created, last_modified, source, mnt_by, abuse_contact, descr) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			reportEvery: 100000,
		},
		{
			name: "geolocations", path: filepath.Join(*input, "geolocations.csv"), fields: 6, required: []int{0, 1, 2},
			create: `CREATE TABLE geolocations (
				start_ip_sort TEXT NOT NULL,
				end_ip_sort TEXT NOT NULL,
				ip_version INTEGER NOT NULL,
				country TEXT,
				region TEXT,
				city TEXT
			);`,
			insert:      `INSERT INTO geolocations (start_ip_sort, end_ip_sort, ip_version, country, region, city) VALUES (?, ?, ?, ?, ?, ?)`,
			reportEvery: 500000,
		},
	}

	for _, spec := range specs {
		if _, err := database.Exec(spec.create); err != nil {
			log.Fatal(err)
		}
		count, err := importCSV(database, spec)
		if err != nil {
			log.Fatalf("%s: %v", spec.name, err)
		}
		fmt.Printf("%s: %d rows\n", spec.name, count)
	}
	if _, err := database.Exec("ANALYZE; PRAGMA optimize;"); err != nil {
		log.Fatal(err)
	}
}

func createMetadata(database *sql.DB, releaseTag, builtAt, sourceCommit string) error {
	_, err := database.Exec(`
		CREATE TABLE dataset_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE bgp_api_dataset (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			release_tag TEXT NOT NULL,
			built_at TEXT NOT NULL,
			activated_at TEXT,
			source_commit TEXT
		);
		INSERT INTO dataset_metadata (key, value) VALUES
			('release_tag', ?),
			('built_at', ?),
			('source_commit', ?);
		INSERT INTO bgp_api_dataset (singleton, release_tag, built_at, activated_at, source_commit)
			VALUES (1, ?, ?, NULL, ?);
	`, releaseTag, builtAt, sourceCommit, releaseTag, builtAt, sourceCommit)
	return err
}

func importCSV(database *sql.DB, spec importSpec) (int64, error) {
	file, err := os.Open(spec.path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = true

	var count int64
	var transaction *sql.Tx
	var insert *sql.Stmt
	closeBatch := func() error {
		if insert != nil {
			if err := insert.Close(); err != nil {
				_ = transaction.Rollback()
				return err
			}
			insert = nil
		}
		if transaction != nil {
			if err := transaction.Commit(); err != nil {
				return err
			}
			transaction = nil
		}
		return nil
	}
	defer func() {
		if insert != nil {
			_ = insert.Close()
		}
		if transaction != nil {
			_ = transaction.Rollback()
		}
	}()

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		if !validRow(row, spec.required) {
			continue
		}
		if transaction == nil {
			transaction, err = database.Begin()
			if err != nil {
				return count, err
			}
			insert, err = transaction.Prepare(spec.insert)
			if err != nil {
				return count, err
			}
		}
		if _, err := insert.Exec(rowValues(row, spec.fields)...); err != nil {
			return count, err
		}
		count++
		if count%int64(batchSize) == 0 {
			if err := closeBatch(); err != nil {
				return count, err
			}
		}
		if spec.reportEvery > 0 && count%spec.reportEvery == 0 {
			fmt.Printf("%s: imported %s rows\n", spec.name, strconv.FormatInt(count, 10))
		}
	}
	if err := closeBatch(); err != nil {
		return count, err
	}
	if count == 0 {
		return count, fmt.Errorf("%s is empty", spec.path)
	}
	return count, nil
}

func validRow(row []string, required []int) bool {
	for _, index := range required {
		if index >= len(row) || strings.TrimSpace(row[index]) == "" {
			return false
		}
	}
	return true
}

func rowValues(row []string, fields int) []any {
	values := make([]any, fields)
	for index := range fields {
		if index >= len(row) {
			values[index] = nil
			continue
		}
		value := strings.TrimSpace(row[index])
		if value == "" {
			values[index] = nil
		} else {
			values[index] = value
		}
	}
	return values
}
