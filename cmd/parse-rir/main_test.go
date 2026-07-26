package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"strings"
	"testing"
)

func TestProcessRPSLBlockRetainsObjectMetadata(t *testing.T) {
	allocationOutput := &bytes.Buffer{}
	routeOutput := &bytes.Buffer{}
	autnumOutput := &bytes.Buffer{}
	geoFile, err := os.CreateTemp(t.TempDir(), "geofeeds")
	if err != nil {
		t.Fatal(err)
	}
	defer geoFile.Close()

	allocations := csv.NewWriter(allocationOutput)
	routes := csv.NewWriter(routeOutput)
	autnums := csv.NewWriter(autnumOutput)
	processRPSLBlock([]string{
		"inetnum: 192.0.2.0 - 192.0.2.255",
		"netname: TEST-NET",
		"country: AU",
		"status: ASSIGNED PA",
		"created: 2024-01-02T03:04:05Z",
		"last-modified: 2024-02-03T04:05:06Z",
		"source: TEST # Filtered",
		"mnt-by: TEST-MNT",
		"org: ORG-TEST",
		"descr: Example allocation",
		"geofeed: https://example.test/geofeed.csv",
	}, "TEST", allocations, routes, autnums, geoFile)
	allocations.Flush()
	if err := allocations.Error(); err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(allocationOutput.String()), ",")
	if len(fields) != 14 || fields[5] != "TEST-NET" || fields[6] != "ASSIGNED PA" || fields[8] != "2024-01-02T03:04:05Z" || fields[10] != "TEST" {
		t.Fatalf("allocation fields = %#v", fields)
	}

	processRPSLBlock([]string{
		"route: 192.0.2.0/24",
		"origin: AS64500",
		"source: TEST",
		"mnt-by: TEST-MNT",
		"descr: Example route",
	}, "TEST", allocations, routes, autnums, geoFile)
	routes.Flush()
	if err := routes.Error(); err != nil {
		t.Fatal(err)
	}
	fields = strings.Split(strings.TrimSpace(routeOutput.String()), ",")
	if len(fields) != 10 || fields[4] != "AS64500" || fields[7] != "TEST-MNT" {
		t.Fatalf("route fields = %#v", fields)
	}

	processRPSLBlock([]string{
		"aut-num: AS64500",
		"as-name: TEST-AS",
		"country: AU",
		"source: TEST",
		"descr: Example autonomous system",
	}, "TEST", allocations, routes, autnums, geoFile)
	autnums.Flush()
	if err := autnums.Error(); err != nil {
		t.Fatal(err)
	}
	fields = strings.Split(strings.TrimSpace(autnumOutput.String()), ",")
	if len(fields) != 11 || fields[0] != "AS64500" || fields[3] != "TEST-AS" {
		t.Fatalf("autnum fields = %#v", fields)
	}
}
