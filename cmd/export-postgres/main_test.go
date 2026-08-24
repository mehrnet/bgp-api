package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestWriteIndexesUsesRangeOverlapIndexes(t *testing.T) {
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	writeIndexes(writer, "bgp_test")
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}

	dump := output.String()
	for _, statement := range []string{
		"CREATE INDEX idx_allocation_objects_overlap ON bgp_test.allocation_objects USING GIST",
		"CREATE INDEX idx_route_objects_overlap ON bgp_test.route_objects USING GIST",
		"numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]')",
	} {
		if !strings.Contains(dump, statement) {
			t.Fatalf("index dump missing %q", statement)
		}
	}
	if strings.Contains(dump, "idx_allocation_objects_range") || strings.Contains(dump, "idx_route_objects_range") {
		t.Fatal("index dump still contains the obsolete B-tree range indexes")
	}
}

func TestWriteSchemaIncludesGeneratedRangeSummaries(t *testing.T) {
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	writeSchema(writer, "bgp_test")
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "CREATE TABLE bgp_test.range_summaries") {
		t.Fatal("schema does not include generated range summaries")
	}
}
