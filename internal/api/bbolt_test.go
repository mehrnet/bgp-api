package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mehrnet/bgp-api/internal/boltstore"
	"github.com/mehrnet/bgp-api/internal/ipkey"
	bbolt "go.etcd.io/bbolt"
)

func TestBboltRepositorySupportsCompleteAPIContract(t *testing.T) {
	repository := testBboltRepository(t)
	ctx := context.Background()

	ip, _ := ipkey.ParseRuntime("1.1.1.1")
	compact, err := repository.Lookup(ctx, ip, LookupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if compact == nil || compact.Details != nil {
		t.Fatalf("compact lookup = %#v", compact)
	}

	lookup, err := repository.Lookup(ctx, ip, LookupOptions{Details: LookupDetailsFull})
	if err != nil {
		t.Fatal(err)
	}
	if lookup == nil || lookup.Registry == nil || *lookup.Registry != "apnic" || len(lookup.Network.ASNs) != 2 || lookup.Location.City == nil || *lookup.Location.City != "South Brisbane" {
		t.Fatalf("lookup = %#v", lookup)
	}
	if lookup.Details == nil || len(lookup.Details.Allocations) != 2 || len(lookup.Details.Routes) != 2 || len(lookup.Details.Geofeeds) != 2 {
		t.Fatalf("lookup details = %#v", lookup.Details)
	}
	lookup.Details = nil
	if !reflect.DeepEqual(compact, lookup) {
		t.Fatalf("compact response diverged from details=full response:\ncompact=%#v\nfull=%#v", compact, lookup)
	}
	legacyCompact, err := lookupCompactPrefixScan(repository, ip)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compact, legacyCompact) {
		t.Fatalf("route LPM response diverged from compact prefix scan:\nlpm=%#v\nscan=%#v", compact, legacyCompact)
	}

	prefix, _ := ipkey.ParsePrefix("1.1.1.0/24")
	prefixResponse, err := repository.LookupPrefix(ctx, prefix, Page{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if prefixResponse.Allocation == nil || prefixResponse.Allocation.Name == nil || *prefixResponse.Allocation.Name != "APNIC-LABS" || len(prefixResponse.Routes.Items) != 1 || prefixResponse.Routes.NextCursor == nil {
		t.Fatalf("prefix response = %#v", prefixResponse)
	}

	rangeValue, _ := ipkey.ParseRange("1.1.1.1", "1.1.1.2")
	rangeResponse, err := repository.LookupRange(ctx, rangeValue, RangeAllocations, RangePage{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rangeResponse.Allocations) != 2 || rangeResponse.Mode != "records" {
		t.Fatalf("range response = %#v", rangeResponse)
	}

	broadRange, _ := ipkey.ParseRange("1.0.0.0", "1.255.255.255")
	summary, err := repository.LookupRangeSummary(ctx, broadRange, RangeRoutes)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Summary == nil || summary.Summary.RouteRecords != 2 || summary.Summary.AllocationRecords != 2 {
		t.Fatalf("summary = %#v", summary.Summary)
	}
	if summary.Summary.BucketPrefixLength != 8 || summary.Summary.Buckets != 1 {
		t.Fatalf("materialized summary = %#v", summary.Summary)
	}
	intermediateRange, _ := ipkey.ParseRange("1.0.0.0", "1.31.255.255")
	intermediateSummary, err := repository.LookupRangeSummary(ctx, intermediateRange, RangeAllocations)
	if err != nil {
		t.Fatal(err)
	}
	if intermediateSummary.Summary == nil || intermediateSummary.Summary.BucketPrefixLength != 11 || intermediateSummary.Summary.Buckets != 1 || intermediateSummary.Summary.AllocationRecords != 33 || intermediateSummary.Summary.RouteRecords != 2 {
		t.Fatalf("intermediate materialized summary = %#v", intermediateSummary.Summary)
	}

	asnResponse, err := repository.LookupASN(ctx, 13335, Page{Limit: 1, Number: 1, Numbered: true})
	if err != nil {
		t.Fatal(err)
	}
	if asnResponse == nil || asnResponse.Autnum == nil || asnResponse.Autnum.Name == nil || *asnResponse.Autnum.Name != "CLOUDFLARENET" || asnResponse.Routes.TotalItems != 1 || asnResponse.Routes.TotalPages != 1 {
		t.Fatalf("ASN response = %#v", asnResponse)
	}

	metadata, err := repository.DatasetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if metadata == nil || metadata.ReleaseTag == nil || *metadata.ReleaseTag != "db-test" || metadata.ActivatedAt == nil {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestBboltRangePaginationUsesProducerRangeIndex(t *testing.T) {
	repository := testBboltRepository(t)
	ctx := context.Background()
	rangeValue, _ := ipkey.ParseRange("1.1.1.0", "1.1.1.255")

	first, err := repository.LookupRange(ctx, rangeValue, RangeAllocations, RangePage{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Allocations) != 1 || first.Allocations[0].Name == nil || *first.Allocations[0].Name != "APNIC-1" || first.NextCursor == nil {
		t.Fatalf("first allocation page = %#v", first)
	}
	second, err := repository.LookupRange(ctx, rangeValue, RangeAllocations, RangePage{Limit: 1, Cursor: *first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Allocations) != 1 || second.Allocations[0].Name == nil || *second.Allocations[0].Name != "APNIC-LABS" || second.NextCursor != nil {
		t.Fatalf("second allocation page = %#v", second)
	}

	firstRoute, err := repository.LookupRange(ctx, rangeValue, RangeRoutes, RangePage{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstRoute.Routes) != 1 || firstRoute.Routes[0].OriginASN != "AS13335" || firstRoute.NextCursor == nil {
		t.Fatalf("first route page = %#v", firstRoute)
	}
	secondRoute, err := repository.LookupRange(ctx, rangeValue, RangeRoutes, RangePage{Limit: 1, Cursor: *firstRoute.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondRoute.Routes) != 1 || secondRoute.Routes[0].OriginASN != "AS64500" || secondRoute.NextCursor != nil {
		t.Fatalf("second route page = %#v", secondRoute)
	}

	_, err = repository.LookupRange(ctx, rangeValue, RangeRoutes, RangePage{Limit: 1, Cursor: "not-a-range-cursor"})
	if !errors.Is(err, errInvalidRangeCursor) {
		t.Fatalf("invalid cursor error = %v", err)
	}
	_, err = repository.LookupRange(ctx, rangeValue, RangeRoutes, RangePage{Limit: 1, Cursor: *first.NextCursor})
	if !errors.Is(err, errInvalidRangeCursor) {
		t.Fatalf("cross-kind cursor error = %v", err)
	}
}

func TestBboltBuildMaterializesAllIPv4SummaryPrefixes(t *testing.T) {
	repository := testBboltRepository(t)
	var summaries int
	if err := repository.db.View(func(transaction *bbolt.Tx) error {
		summaries = transaction.Bucket(boltstore.BucketRangeSummaries).Stats().KeyN
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if summaries != (1<<17)-1 {
		t.Fatalf("summary records = %d", summaries)
	}
}

func BenchmarkBboltLookupIP(b *testing.B) {
	repository := testBboltRepository(b)
	ip, _ := ipkey.ParseRuntime("1.1.1.1")
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := repository.Lookup(context.Background(), ip, LookupOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBboltLookupIPFull(b *testing.B) {
	repository := testBboltRepository(b)
	ip, _ := ipkey.ParseRuntime("1.1.1.1")
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := repository.Lookup(context.Background(), ip, LookupOptions{Details: LookupDetailsFull}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBboltLookupIPPrefixScan(b *testing.B) {
	repository := testBboltRepository(b)
	ip, _ := ipkey.ParseRuntime("1.1.1.1")
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := lookupCompactPrefixScan(repository, ip); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBboltLookupIPRouteLPMPrefixScan(b *testing.B) {
	repository := testBboltRepository(b)
	ip, _ := ipkey.ParseRuntime("1.1.1.1")
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := lookupCompactRouteLPMPrefixScan(repository, ip); err != nil {
			b.Fatal(err)
		}
	}
}

// lookupCompactPrefixScan is the compact lookup before the producer-built
// route LPM. Keeping it test-only makes the end-to-end benchmark comparable
// without retaining the old path in production.
func lookupCompactPrefixScan(repository *BboltRepository, ip ipkey.RuntimeIP) (*LookupResponse, error) {
	address := ip.Address
	var allocations []boltstore.Allocation
	var routes []boltstore.Route
	var geofeeds []boltstore.Geofeed
	err := repository.db.View(func(transaction *bbolt.Tx) error {
		allocationIDs, err := matchingIPIDs(context.Background(), transaction.Bucket(boltstore.BucketAllocationIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range allocationIDs {
			record, err := allocationLookupByID(transaction, id)
			if err != nil {
				return err
			}
			allocations = append(allocations, record)
		}
		routeIDs, err := matchingMostSpecificIPIDsForTest(context.Background(), transaction.Bucket(boltstore.BucketRouteIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range routeIDs {
			record, err := routeLookupByID(transaction, id)
			if err != nil {
				return err
			}
			routes = append(routes, record)
		}
		geofeedIDs, err := matchingIPIDs(context.Background(), transaction.Bucket(boltstore.BucketGeofeedIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range geofeedIDs {
			record, err := geofeedByID(transaction, id)
			if err != nil {
				return err
			}
			geofeeds = append(geofeeds, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return buildBboltResponse(ip, allocations, routes, geofeeds, LookupOptions{}), nil
}

// lookupCompactRouteLPMPrefixScan matches the schema-3 compact path: routes
// use their serialized LPM, while allocations and geofeeds still use bbolt
// prefix seeks. It isolates the effect of the schema-4 selectors in tests.
func lookupCompactRouteLPMPrefixScan(repository *BboltRepository, ip ipkey.RuntimeIP) (*LookupResponse, error) {
	address := ip.Address
	var allocations []boltstore.Allocation
	var routes []boltstore.Route
	var geofeeds []boltstore.Geofeed
	err := repository.db.View(func(transaction *bbolt.Tx) error {
		allocationIDs, err := matchingIPIDs(context.Background(), transaction.Bucket(boltstore.BucketAllocationIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range allocationIDs {
			record, err := allocationLookupByID(transaction, id)
			if err != nil {
				return err
			}
			allocations = append(allocations, record)
		}
		routeLPM := transaction.Bucket(boltstore.BucketRouteLPM).Get(boltstore.RouteLPMKey(uint8(ip.Version)))
		routeIDs, err := boltstore.LookupRouteLPM(routeLPM, address)
		if err != nil {
			return err
		}
		if len(routeIDs) > maxCandidates {
			routeIDs = routeIDs[:maxCandidates]
		}
		for _, id := range routeIDs {
			record, err := routeLookupByID(transaction, id)
			if err != nil {
				return err
			}
			routes = append(routes, record)
		}
		geofeedIDs, err := matchingIPIDs(context.Background(), transaction.Bucket(boltstore.BucketGeofeedIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range geofeedIDs {
			record, err := geofeedByID(transaction, id)
			if err != nil {
				return err
			}
			geofeeds = append(geofeeds, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return buildBboltResponse(ip, allocations, routes, geofeeds, LookupOptions{}), nil
}

func BenchmarkBboltRouteLPM(b *testing.B) {
	repository := testBboltRepository(b)
	address, _ := ipkey.ParseRuntime("1.1.1.1")
	query := address.Address
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		err := repository.db.View(func(transaction *bbolt.Tx) error {
			value := transaction.Bucket(boltstore.BucketRouteLPM).Get(boltstore.RouteLPMKey(uint8(address.Version)))
			_, err := boltstore.LookupRouteLPM(value, query)
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBboltRoutePrefixScan is the former compact-route selection path.
// It is retained in tests to quantify the benefit of the immutable LPM index.
func BenchmarkBboltRoutePrefixScan(b *testing.B) {
	repository := testBboltRepository(b)
	address, _ := ipkey.ParseRuntime("1.1.1.1")
	query := address.Address
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		err := repository.db.View(func(transaction *bbolt.Tx) error {
			_, err := matchingMostSpecificIPIDsForTest(context.Background(), transaction.Bucket(boltstore.BucketRouteIndex), query, maxCandidates)
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func matchingMostSpecificIPIDsForTest(ctx context.Context, index *bbolt.Bucket, address netip.Addr, limit int) ([]uint32, error) {
	bits := 128
	if address.Is4() {
		bits = 32
	}
	cursor := index.Cursor()
	var seek [boltstore.IndexKeySize]byte
	for length := bits; length >= 0; length-- {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		base := boltstore.PutPrefixKey(seek[:], netip.PrefixFrom(address, length))
		ids := make([]uint32, 0)
		for key, _ := cursor.Seek(seek[:]); len(key) == boltstore.IndexKeySize && bytes.Equal(key[:boltstore.PrefixKeySize], base); key, _ = cursor.Next() {
			id, _ := boltstore.IndexID(key)
			ids = append(ids, id)
			if limit > 0 && len(ids) == limit {
				break
			}
		}
		if len(ids) > 0 {
			return ids, nil
		}
	}
	return nil, nil
}

type testingTB interface {
	Helper()
	TempDir() string
	Cleanup(func())
	Fatal(...any)
}

func testBboltRepository(tb testingTB) *BboltRepository {
	tb.Helper()
	directory := tb.TempDir()
	writeTestCSV(tb, filepath.Join(directory, "allocations.csv"), [][]string{
		{sortKey("1.0.0.0"), sortKey("1.255.255.255"), "4", "APNIC", "AU", "APNIC-1", "ALLOCATED PORTABLE", "2011-08-11", "", "", "APNIC", "", "", "", ""},
		{sortKey("1.1.1.0"), sortKey("1.1.1.255"), "4", "APNIC", "AU", "APNIC-LABS", "ALLOCATED PORTABLE", "2011-08-11", "", "", "APNIC", "", "", "abuse@example.test", ""},
	})
	writeTestCSV(tb, filepath.Join(directory, "routes.csv"), [][]string{
		{sortKey("1.1.1.0"), sortKey("1.1.1.255"), "4", "1.1.1.0/24", "AS13335", "APNIC", "APNIC", "", "", "abuse@example.test", ""},
		{sortKey("1.1.1.0"), sortKey("1.1.1.255"), "4", "1.1.1.0/24", "AS64500", "APNIC", "APNIC", "", "", "", ""},
	})
	writeTestCSV(tb, filepath.Join(directory, "geolocations.csv"), [][]string{
		{sortKey("1.0.0.0"), sortKey("1.255.255.255"), "4", "AU", "New South Wales", "Sydney"},
		{sortKey("1.1.1.0"), sortKey("1.1.1.255"), "4", "AU", "Queensland", "South Brisbane"},
	})
	writeTestCSV(tb, filepath.Join(directory, "autnums.csv"), [][]string{{"AS13335", "APNIC", "US", "CLOUDFLARENET", "ORG-CLOUD14-ARIN", "ASSIGNED", "", "", "APNIC", "", "abuse@example.test", ""}})
	path := filepath.Join(directory, "mehrnet_bgp.bbolt")
	stats, err := boltstore.Build(context.Background(), boltstore.BuildOptions{InputDir: directory, OutputPath: path, ReleaseTag: "db-test", BuiltAt: "2026-08-26T00:00:00Z", SourceCommit: "abcdef", Compact: true})
	if err != nil {
		tb.Fatal(err)
	}
	if stats.Allocations != 2 || stats.Routes != 2 || stats.Geofeeds != 2 || stats.Autnums != 1 || stats.RouteLPMNodes == 0 || stats.RouteLPMIDs != stats.Routes || stats.RouteLPMBytes == 0 || stats.AllocationLPMNodes == 0 || stats.AllocationLPMBytes == 0 || stats.GeofeedLPMNodes == 0 || stats.GeofeedLPMBytes == 0 {
		tb.Fatal("unexpected build stats: ", stats)
	}
	repository, err := NewBboltRepository(path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = repository.Close() })
	return repository
}

func writeTestCSV(tb testingTB, path string, rows [][]string) {
	tb.Helper()
	file, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	writer := csv.NewWriter(file)
	for _, row := range rows {
		if err := writer.Write(row); err != nil {
			tb.Fatal(err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		tb.Fatal(err)
	}
	if err := file.Close(); err != nil {
		tb.Fatal(err)
	}
}

func sortKey(value string) string { parsed, _ := ipkey.Parse(value); return parsed.SortKey }
