package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestWriteTablePatchProducesSetBasedDelta(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("ATTACH DATABASE ':memory:' AS base"); err != nil {
		t.Fatal(err)
	}
	columns := "start_ip_sort TEXT,end_ip_sort TEXT,ip_version INTEGER,registry TEXT,country TEXT,netname TEXT,status TEXT,allocation_date TEXT,created TEXT,last_modified TEXT,source TEXT,mnt_by TEXT,org TEXT,abuse_contact TEXT,descr TEXT"
	for _, schema := range []string{"main", "base"} {
		if _, err := database.Exec("CREATE TABLE " + schema + ".allocations (" + columns + ")"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec("INSERT INTO base.allocations (start_ip_sort, end_ip_sort, ip_version, netname) VALUES ('1', '1', 4, 'old')"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO main.allocations (start_ip_sort, end_ip_sort, ip_version, netname) VALUES ('2', '2', 4, 'new')"); err != nil {
		t.Fatal(err)
	}

	table := patchTable{
		name:          "allocation_objects",
		sourceTable:   "allocations",
		sourceColumns: []string{"start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "status", "allocation_date", "created", "last_modified", "source", "mnt_by", "org", "abuse_contact", "descr"},
		targetColumns: []string{"start_ip_sort", "end_ip_sort", "ip_version", "registry", "country", "netname", "status", "allocation_date", "created", "last_modified", "record_source", "mnt_by", "org", "abuse_contact", "description"},
		keyColumns:    []string{"start_ip_sort", "end_ip_sort", "ip_version"},
	}
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	counts, err := writeTablePatch(database, writer, table)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if counts.deleted != 1 || counts.inserted != 1 {
		t.Fatalf("counts = %#v, want one deletion and one insertion", counts)
	}
	patch := output.String()
	for _, expected := range []string{
		"DELETE FROM :\"dataset_schema\".allocation_objects",
		"INSERT INTO :\"dataset_schema\".allocation_objects",
		"1\t1\t4",
		"2\t2\t4",
	} {
		if !strings.Contains(patch, expected) {
			t.Errorf("patch is missing %q:\n%s", expected, patch)
		}
	}
}
