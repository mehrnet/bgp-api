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
		"abuse-c: ABUSE-TEST",
		"descr: Example allocation",
		"geofeed: https://example.test/geofeed.csv",
	}, "TEST", allocations, routes, autnums, geoFile)
	allocations.Flush()
	if err := allocations.Error(); err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(allocationOutput.String()), ",")
	if len(fields) != 15 || fields[5] != "TEST-NET" || fields[6] != "ASSIGNED PA" || fields[8] != "2024-01-02T03:04:05Z" || fields[10] != "TEST" || fields[13] != "ABUSE-TEST" {
		t.Fatalf("allocation fields = %#v", fields)
	}

	processRPSLBlock([]string{
		"route: 192.0.2.0/24",
		"origin: AS64500",
		"source: TEST",
		"mnt-by: TEST-MNT",
		"abuse-mailbox: abuse@example.test",
		"descr: Example route",
	}, "TEST", allocations, routes, autnums, geoFile)
	routes.Flush()
	if err := routes.Error(); err != nil {
		t.Fatal(err)
	}
	fields = strings.Split(strings.TrimSpace(routeOutput.String()), ",")
	if len(fields) != 11 || fields[4] != "AS64500" || fields[7] != "TEST-MNT" || fields[9] != "abuse@example.test" {
		t.Fatalf("route fields = %#v", fields)
	}

	processRPSLBlock([]string{
		"aut-num: AS64500",
		"as-name: TEST-AS",
		"country: AU",
		"source: TEST",
		"abuse-c: ABUSE-AS",
		"descr: Example autonomous system",
	}, "TEST", allocations, routes, autnums, geoFile)
	autnums.Flush()
	if err := autnums.Error(); err != nil {
		t.Fatal(err)
	}
	fields = strings.Split(strings.TrimSpace(autnumOutput.String()), ",")
	if len(fields) != 12 || fields[0] != "AS64500" || fields[3] != "TEST-AS" || fields[10] != "ABUSE-AS" {
		t.Fatalf("autnum fields = %#v", fields)
	}
}

func TestProcessRPSLBlockSkipsGlobalIANAIPv4Placeholder(t *testing.T) {
	allocationOutput := &bytes.Buffer{}
	geoFile, err := os.CreateTemp(t.TempDir(), "geofeeds")
	if err != nil {
		t.Fatal(err)
	}
	defer geoFile.Close()

	allocations := csv.NewWriter(allocationOutput)
	processRPSLBlock([]string{
		"inetnum: 0.0.0.0 - 255.255.255.255",
		"netname: IANA-BLK",
		"country: EU # Country is really world wide",
		"status: ALLOCATED UNSPECIFIED",
		"source: RIPE",
	}, "RIPE", allocations, csv.NewWriter(&bytes.Buffer{}), csv.NewWriter(&bytes.Buffer{}), geoFile)
	allocations.Flush()
	if err := allocations.Error(); err != nil {
		t.Fatal(err)
	}
	if output := allocationOutput.String(); output != "" {
		t.Fatalf("global IANA placeholder was written: %q", output)
	}

	if !isGlobalIANAIPv4Placeholder("0.0.0.0", "255.255.255.255", "IANA-BLOCK") {
		t.Fatal("expected global IANA block to be recognized")
	}
	if isGlobalIANAIPv4Placeholder("80.0.0.0", "80.255.255.255", "RIPE-CIDR-BLOCK") {
		t.Fatal("ordinary RIR allocation was treated as an IANA placeholder")
	}
}
