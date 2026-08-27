package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mehrnet/bgp-api/internal/boltstore"
	"github.com/mehrnet/bgp-api/internal/ipkey"
	bbolt "go.etcd.io/bbolt"
)

type BboltRepository struct {
	db           *bbolt.DB
	metadata     *DatasetMetadata
	overlapSlots chan struct{}
}

var errBboltQueryTooBroad = errors.New("bbolt overlap scan limit exceeded")
var errInvalidRangeCursor = errors.New("invalid range cursor")

const (
	maxCandidates          = 64
	maxOverlapIndexEntries = 2_000_000
)

func NewBboltRepository(path string) (*BboltRepository, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("bbolt database path is required")
	}
	db, err := bbolt.Open(path, 0o400, &bbolt.Options{
		ReadOnly: true, NoFreelistSync: true, NoStatistics: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open bbolt database: %w", err)
	}
	repository := &BboltRepository{db: db, overlapSlots: make(chan struct{}, 2)}
	if err := db.View(func(tx *bbolt.Tx) error {
		for _, name := range boltstore.RequiredBuckets {
			if tx.Bucket(name) == nil {
				return fmt.Errorf("bbolt database is missing bucket %q", name)
			}
		}
		metadata := tx.Bucket(boltstore.BucketMetadata)
		version := metadata.Get(boltstore.KeySchemaVersion)
		if len(version) != 4 || binary.BigEndian.Uint32(version) != boltstore.SchemaVersion {
			return fmt.Errorf("unsupported bbolt schema version")
		}
		activatedAt := time.Now().UTC().Format(time.RFC3339)
		repository.metadata = &DatasetMetadata{
			ReleaseTag:   stringValue(metadata.Get(boltstore.KeyReleaseTag)),
			BuiltAt:      stringValue(metadata.Get(boltstore.KeyBuiltAt)),
			ActivatedAt:  &activatedAt,
			SourceCommit: stringValue(metadata.Get(boltstore.KeySourceCommit)),
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	return repository, nil
}

func (repository *BboltRepository) Close() error { return repository.db.Close() }

func (repository *BboltRepository) DatasetMetadata(context.Context) (*DatasetMetadata, error) {
	if repository.metadata == nil {
		return nil, nil
	}
	copy := *repository.metadata
	return &copy, nil
}

func (repository *BboltRepository) Lookup(ctx context.Context, ip ipkey.RuntimeIP, options LookupOptions) (*LookupResponse, error) {
	if options.Details == LookupDetailsFull {
		return repository.lookupFull(ctx, ip)
	}
	return repository.lookupCompact(ctx, ip)
}

// lookupCompact is the default browser/API path. It selects the same records
// as details=full, but reads only the fields represented in LookupResponse.
func (repository *BboltRepository) lookupCompact(ctx context.Context, ip ipkey.RuntimeIP) (*LookupResponse, error) {
	address := ip.Address
	var allocation *boltstore.Allocation
	var routes []boltstore.Route
	var geofeed *boltstore.Geofeed
	err := repository.db.View(func(tx *bbolt.Tx) error {
		allocationLPM := tx.Bucket(boltstore.BucketAllocationLPM).Get(boltstore.SelectionLPMKey(uint8(ip.Version)))
		allocationID, err := boltstore.LookupSelectionLPM(allocationLPM, address)
		if err != nil {
			return err
		}
		if allocationID != 0 {
			record, err := allocationLookupByID(tx, allocationID)
			if err != nil {
				return err
			}
			allocation = &record
		}
		routeLPM := tx.Bucket(boltstore.BucketRouteLPM).Get(boltstore.RouteLPMKey(uint8(ip.Version)))
		routeIDs, err := boltstore.LookupRouteLPM(routeLPM, address)
		if err != nil {
			return err
		}
		if len(routeIDs) > maxCandidates {
			routeIDs = routeIDs[:maxCandidates]
		}
		if len(routeIDs) > 0 {
			routes = make([]boltstore.Route, len(routeIDs))
		}
		for index, id := range routeIDs {
			record, err := routeLookupByID(tx, id)
			if err != nil {
				return err
			}
			routes[index] = record
		}
		geofeedLPM := tx.Bucket(boltstore.BucketGeofeedLPM).Get(boltstore.SelectionLPMKey(uint8(ip.Version)))
		geofeedID, err := boltstore.LookupSelectionLPM(geofeedLPM, address)
		if err != nil {
			return err
		}
		if geofeedID != 0 {
			record, err := geofeedByID(tx, geofeedID)
			if err != nil {
				return err
			}
			geofeed = &record
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bbolt compact IP lookup: %w", err)
	}
	return buildCompactBboltResponse(ip, allocation, routes, geofeed), nil
}

// lookupFull keeps the complete RIR/IRR/geofeed source traversal available to
// clients that explicitly request details=full.
func (repository *BboltRepository) lookupFull(ctx context.Context, ip ipkey.RuntimeIP) (*LookupResponse, error) {
	address := ip.Address
	var allocations []boltstore.Allocation
	var routes []boltstore.Route
	var geofeeds []boltstore.Geofeed
	err := repository.db.View(func(tx *bbolt.Tx) error {
		allocationIDs, err := matchingIPIDs(ctx, tx.Bucket(boltstore.BucketAllocationIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range allocationIDs {
			record, err := allocationByID(tx, id)
			if err != nil {
				return err
			}
			allocations = append(allocations, record)
		}
		routeIDs, err := matchingIPIDs(ctx, tx.Bucket(boltstore.BucketRouteIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range routeIDs {
			record, err := routeByID(tx, id)
			if err != nil {
				return err
			}
			routes = append(routes, record)
		}
		geofeedIDs, err := matchingIPIDs(ctx, tx.Bucket(boltstore.BucketGeofeedIndex), address, maxCandidates)
		if err != nil {
			return err
		}
		for _, id := range geofeedIDs {
			record, err := geofeedByID(tx, id)
			if err != nil {
				return err
			}
			geofeeds = append(geofeeds, record)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bbolt full IP lookup: %w", err)
	}
	return buildBboltResponse(ip, allocations, routes, geofeeds, LookupOptions{Details: LookupDetailsFull}), nil
}

// buildBboltResponse is the production point-lookup path. It keeps bbolt's
// binary addresses intact through selection and JSON construction, avoiding
// the historical decimal sort-key and math/big conversion layer.
func buildBboltResponse(ip ipkey.RuntimeIP, allocations []boltstore.Allocation, routes []boltstore.Route, geofeeds []boltstore.Geofeed, options LookupOptions) *LookupResponse {
	allocation := narrowestAllocation(allocations)
	bestRoutes := narrowestRoutes(routes)
	geofeed := narrowestGeofeed(geofeeds)
	response := buildSelectedBboltResponse(ip, allocation, bestRoutes, geofeed)
	if response == nil {
		return nil
	}
	if options.Details == LookupDetailsFull {
		response.Details = bboltDetails(allocations, routes, geofeeds)
	}
	return response
}

// buildCompactBboltResponse avoids the source slices and re-selection work
// needed for details=full. The immutable selectors have already chosen the
// sole allocation and geofeed matching the compact response contract.
func buildCompactBboltResponse(ip ipkey.RuntimeIP, allocation *boltstore.Allocation, routes []boltstore.Route, geofeed *boltstore.Geofeed) *LookupResponse {
	return buildSelectedBboltResponse(ip, allocation, routes, geofeed)
}

func buildSelectedBboltResponse(ip ipkey.RuntimeIP, allocation *boltstore.Allocation, routes []boltstore.Route, geofeed *boltstore.Geofeed) *LookupResponse {
	if allocation == nil && len(routes) == 0 && geofeed == nil {
		return nil
	}

	response := &LookupResponse{IP: ip.Canonical, Version: ip.Version}
	response.Network.ASNs = bboltASNs(routes)
	if len(response.Network.ASNs) == 1 {
		response.Network.ASN = &response.Network.ASNs[0]
		response.Network.ASNumber = asNumber(response.Network.ASN)
	}
	if len(routes) > 0 {
		route := routes[0]
		response.Network.CIDR = optional(recordPrefix(route))
		start, end := prefixRangeForRecord(route)
		startIP, endIP := start.Addr(int(route.Version)).String(), end.Addr(int(route.Version)).String()
		response.Network.StartIP, response.Network.EndIP = &startIP, &endIP
		response.Network.AbuseContact = optional(route.AbuseContact)
	}
	if allocation != nil {
		response.Network.Name = optional(allocation.Name)
		allocationStart := allocation.Start.Addr(int(allocation.Version)).String()
		allocationEnd := allocation.End.Addr(int(allocation.Version)).String()
		response.Allocation.StartIP, response.Allocation.EndIP = &allocationStart, &allocationEnd
		response.Allocation.Registry = lower(optional(allocation.Registry))
		response.Allocation.CountryRaw = upper(optional(allocation.Country))
		response.Allocation.CountryCode = countryCode(response.Allocation.CountryRaw)
		response.Allocation.Name = optional(allocation.Name)
		response.Allocation.AllocationDate = optional(allocation.AllocationDate)
		response.Allocation.Status = optional(allocation.Status)
		response.Allocation.AbuseContact = optional(allocation.AbuseContact)
		response.Registry = lower(optional(allocation.Registry))
		response.AllocationDate = response.Allocation.AllocationDate
		response.AllocationStatus = response.Allocation.Status
	}
	if geofeed != nil {
		response.Location.CountryCode = countryCode(upper(optional(geofeed.Country)))
		response.Location.Region = optional(geofeed.Region)
		response.Location.City = optional(geofeed.City)
	}
	if response.Location.CountryCode == nil {
		response.Location.CountryCode = response.Allocation.CountryCode
	}
	response.Sources.Allocation = allocation != nil
	response.Sources.Route = len(routes) > 0
	response.Sources.Geofeed = geofeed != nil
	return response
}

func narrowestAllocation(values []boltstore.Allocation) *boltstore.Allocation {
	if len(values) == 0 {
		return nil
	}
	best := &values[0]
	for index := 1; index < len(values); index++ {
		candidate := &values[index]
		if rangeWidthCompare(candidate.Start, candidate.End, best.Start, best.End) < 0 {
			best = candidate
		}
	}
	return best
}

func narrowestGeofeed(values []boltstore.Geofeed) *boltstore.Geofeed {
	if len(values) == 0 {
		return nil
	}
	best := &values[0]
	for index := 1; index < len(values); index++ {
		candidate := &values[index]
		if rangeWidthCompare(candidate.Start, candidate.End, best.Start, best.End) < 0 {
			best = candidate
		}
	}
	return best
}

func narrowestRoutes(values []boltstore.Route) []boltstore.Route {
	if len(values) == 0 {
		return nil
	}
	bestLength := values[0].PrefixLength
	for _, value := range values[1:] {
		if value.PrefixLength > bestLength {
			bestLength = value.PrefixLength
		}
	}
	result := make([]boltstore.Route, 0, len(values))
	for _, value := range values {
		if value.PrefixLength == bestLength {
			result = append(result, value)
		}
	}
	return result
}

func rangeWidthCompare(leftStart, leftEnd, rightStart, rightEnd boltstore.Address) int {
	var leftWidth, rightWidth [16]byte
	subtractAddress(leftWidth[:], leftEnd[:], leftStart[:])
	subtractAddress(rightWidth[:], rightEnd[:], rightStart[:])
	return bytes.Compare(leftWidth[:], rightWidth[:])
}

func subtractAddress(destination, left, right []byte) {
	borrow := 0
	for index := len(destination) - 1; index >= 0; index-- {
		value := int(left[index]) - int(right[index]) - borrow
		if value < 0 {
			value += 256
			borrow = 1
		} else {
			borrow = 0
		}
		destination[index] = byte(value)
	}
}

func bboltASNs(routes []boltstore.Route) []string {
	if len(routes) == 1 {
		name := strings.ToUpper(strings.TrimSpace(routes[0].OriginASN))
		if name == "" {
			return nil
		}
		return []string{name}
	}
	type asnValue struct {
		name   string
		number uint32
	}
	values := make([]asnValue, 0, len(routes))
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		name := strings.ToUpper(strings.TrimSpace(route.OriginASN))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		values = append(values, asnValue{name: name, number: route.ASNumber})
	}
	sort.Slice(values, func(left, right int) bool {
		if values[left].number != values[right].number {
			return values[left].number < values[right].number
		}
		return values[left].name < values[right].name
	})
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.name)
	}
	return result
}

func bboltDetails(allocations []boltstore.Allocation, routes []boltstore.Route, geofeeds []boltstore.Geofeed) *LookupDetails {
	result := &LookupDetails{}
	if len(allocations) > 0 {
		result.Allocations = make([]LookupDetailRecord, 0, len(allocations))
		for _, value := range allocations {
			result.Allocations = append(result.Allocations, allocationDetail(value))
		}
	}
	if len(routes) > 0 {
		result.Routes = make([]LookupDetailRecord, 0, len(routes))
		for _, value := range routes {
			result.Routes = append(result.Routes, routeDetail(value))
		}
	}
	if len(geofeeds) > 0 {
		result.Geofeeds = make([]LookupDetailRecord, 0, len(geofeeds))
		for _, value := range geofeeds {
			result.Geofeeds = append(result.Geofeeds, geofeedDetail(value))
		}
	}
	return result
}

func allocationDetail(value boltstore.Allocation) LookupDetailRecord {
	return LookupDetailRecord{
		StartIP: value.Start.Addr(int(value.Version)).String(), EndIP: value.End.Addr(int(value.Version)).String(), Version: int(value.Version),
		Registry: lower(optional(value.Registry)), CountryCode: countryCode(upper(optional(value.Country))), CountryRaw: upper(optional(value.Country)), Name: optional(value.Name), Status: optional(value.Status), AllocationDate: optional(value.AllocationDate), Created: optional(value.Created), LastModified: optional(value.LastModified), Source: optional(value.Source), Maintainers: optional(value.Maintainers), Organization: optional(value.Organization), AbuseContact: optional(value.AbuseContact), Description: optional(value.Description),
	}
}

func routeDetail(value boltstore.Route) LookupDetailRecord {
	start, end := prefixRangeForRecord(value)
	result := LookupDetailRecord{
		StartIP: start.Addr(int(value.Version)).String(), EndIP: end.Addr(int(value.Version)).String(), Version: int(value.Version), CIDR: optional(recordPrefix(value)), ASN: upper(optional(value.OriginASN)), Registry: lower(optional(value.Registry)), Source: optional(value.Source), Maintainers: optional(value.Maintainers), Organization: optional(value.Organization), AbuseContact: optional(value.AbuseContact), Description: optional(value.Description),
	}
	result.ASNumber = asNumber(result.ASN)
	return result
}

func geofeedDetail(value boltstore.Geofeed) LookupDetailRecord {
	return LookupDetailRecord{
		StartIP: value.Start.Addr(int(value.Version)).String(), EndIP: value.End.Addr(int(value.Version)).String(), Version: int(value.Version), CountryCode: countryCode(upper(optional(value.Country))), CountryRaw: upper(optional(value.Country)), Region: optional(value.Region), City: optional(value.City),
	}
}

func (repository *BboltRepository) LookupPrefix(ctx context.Context, prefix ipkey.ParsedPrefix, page Page) (*PrefixResponse, error) {
	if err := repository.acquireOverlapSlot(ctx); err != nil {
		return nil, err
	}
	defer repository.releaseOverlapSlot()
	var allocation *AllocationObject
	var routes []RouteObject
	err := repository.db.View(func(tx *bbolt.Tx) error {
		var err error
		allocation, err = coveringAllocation(ctx, tx, prefix)
		if err != nil {
			return err
		}
		ids, err := overlapIDs(ctx, tx.Bucket(boltstore.BucketRouteIndex), prefix.Start, prefix.End, page.Cursor)
		if err != nil {
			return err
		}
		if len(ids) > page.Limit+1 {
			ids = ids[:page.Limit+1]
		}
		for _, id := range ids {
			record, err := routeByID(tx, id)
			if err != nil {
				return err
			}
			object := routeObject(id, record)
			routePrefix := recordPrefix(record)
			switch {
			case routePrefix == prefix.Canonical:
				object.Relation = "exact"
			case routePrefixContains(routePrefix, prefix):
				object.Relation = "covering"
			default:
				object.Relation = "more_specific"
			}
			routes = append(routes, object)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bbolt prefix lookup: %w", err)
	}
	routes, next := routePage(routes, page.Limit)
	return &PrefixResponse{Prefix: prefixDescriptor(prefix), Allocation: allocation, Routes: RoutePage{Items: routes, NextCursor: next}}, nil
}

func (repository *BboltRepository) LookupRange(ctx context.Context, value ipkey.ParsedRange, kind RangeKind, page RangePage) (*RangeResponse, error) {
	if err := repository.acquireOverlapSlot(ctx); err != nil {
		return nil, err
	}
	defer repository.releaseOverlapSlot()
	response := &RangeResponse{Range: rangeDescriptor(value), Kind: kind, Mode: "records"}
	err := repository.db.View(func(tx *bbolt.Tx) error {
		entries, next, err := rangePageEntries(ctx, tx, value, kind, page)
		if err != nil {
			return err
		}
		response.NextCursor = next
		if kind == RangeAllocations {
			for _, entry := range entries {
				record, err := allocationByID(tx, entry.id)
				if err != nil {
					return err
				}
				response.Allocations = append(response.Allocations, allocationObject(entry.id, record))
			}
			return nil
		}
		for _, entry := range entries {
			record, err := routeByID(tx, entry.id)
			if err != nil {
				return err
			}
			response.Routes = append(response.Routes, routeObject(entry.id, record))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bbolt range lookup: %w", err)
	}
	return response, nil
}

const (
	rangeCursorCovering byte = 1
	rangeCursorDirect   byte = 2
	rangeCursorSize          = 1 + 1 + 16 + 16 + boltstore.RangeIndexKeySize
)

type rangeCursor struct {
	phase      byte
	kind       RangeKind
	queryStart boltstore.Address
	queryEnd   boltstore.Address
	key        [boltstore.RangeIndexKeySize]byte
}

type rangeIndexEntry struct {
	id  uint32
	key [boltstore.RangeIndexKeySize]byte
}

func rangePageEntries(ctx context.Context, tx *bbolt.Tx, value ipkey.ParsedRange, kind RangeKind, page RangePage) ([]rangeIndexEntry, nullableString, error) {
	cursor, err := decodeRangeCursor(page.Cursor)
	if err != nil {
		return nil, nil, err
	}
	start := boltstore.AddressFromAddr(netip.MustParseAddr(value.Start.Canonical))
	end := boltstore.AddressFromAddr(netip.MustParseAddr(value.End.Canonical))
	version := uint8(value.Version)
	if cursor.phase != 0 && (cursor.kind != kind || cursor.queryStart != start || cursor.queryEnd != end || cursor.key[0] != version) {
		return nil, nil, errInvalidRangeCursor
	}
	pointIndex, rangeIndex := rangeBuckets(tx, kind)
	if pointIndex == nil || rangeIndex == nil {
		return nil, nil, fmt.Errorf("range index bucket is missing")
	}
	entries := make([]rangeIndexEntry, 0, page.Limit)
	if cursor.phase != rangeCursorDirect {
		covering, err := coveringRangeEntries(ctx, tx, pointIndex, kind, version, start)
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range covering {
			if cursor.phase == rangeCursorCovering && bytes.Compare(entry.key[:], cursor.key[:]) <= 0 {
				continue
			}
			if len(entries) == page.Limit {
				return entries, encodeRangeCursor(rangeCursorCovering, kind, start, end, entries[len(entries)-1].key), nil
			}
			entries = append(entries, entry)
		}
	}

	var after *[boltstore.RangeIndexKeySize]byte
	if cursor.phase == rangeCursorDirect {
		after = &cursor.key
	}
	direct, more, err := directRangeEntries(ctx, rangeIndex, version, start, end, after, page.Limit-len(entries)+1)
	if err != nil {
		return nil, nil, err
	}
	if len(entries) == page.Limit {
		if len(direct) > 0 {
			var first [boltstore.RangeIndexKeySize]byte
			boltstore.RangeIndexStartKey(first[:], version, start)
			return entries, encodeRangeCursor(rangeCursorDirect, kind, start, end, first), nil
		}
		return entries, nil, nil
	}
	capacity := page.Limit - len(entries)
	if len(direct) > capacity {
		direct = direct[:capacity]
		more = true
	}
	entries = append(entries, direct...)
	if more && len(entries) > 0 {
		return entries, encodeRangeCursor(rangeCursorDirect, kind, start, end, entries[len(entries)-1].key), nil
	}
	return entries, nil, nil
}

func rangeBuckets(tx *bbolt.Tx, kind RangeKind) (*bbolt.Bucket, *bbolt.Bucket) {
	if kind == RangeAllocations {
		return tx.Bucket(boltstore.BucketAllocationIndex), tx.Bucket(boltstore.BucketAllocationRange)
	}
	return tx.Bucket(boltstore.BucketRouteIndex), tx.Bucket(boltstore.BucketRouteRange)
}

func coveringRangeEntries(ctx context.Context, tx *bbolt.Tx, pointIndex *bbolt.Bucket, kind RangeKind, version uint8, start boltstore.Address) ([]rangeIndexEntry, error) {
	address := start.Addr(int(version))
	ids, err := matchingIPIDs(ctx, pointIndex, address, 0)
	if err != nil {
		return nil, err
	}
	seen := make(map[uint32]struct{}, len(ids))
	entries := make([]rangeIndexEntry, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		entry, matches, err := coveringRangeEntry(tx, kind, id, start)
		if err != nil {
			return nil, err
		}
		if matches {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(left, right int) bool { return bytes.Compare(entries[left].key[:], entries[right].key[:]) < 0 })
	return entries, nil
}

func coveringRangeEntry(tx *bbolt.Tx, kind RangeKind, id uint32, queryStart boltstore.Address) (rangeIndexEntry, bool, error) {
	var start, end boltstore.Address
	var version uint8
	if kind == RangeAllocations {
		record, err := allocationByID(tx, id)
		if err != nil {
			return rangeIndexEntry{}, false, err
		}
		start, end, version = record.Start, record.End, record.Version
	} else {
		record, err := routeByID(tx, id)
		if err != nil {
			return rangeIndexEntry{}, false, err
		}
		start, end = prefixRangeForRecord(record)
		version = record.Version
	}
	if bytes.Compare(start[:], queryStart[:]) >= 0 || bytes.Compare(end[:], queryStart[:]) < 0 {
		return rangeIndexEntry{}, false, nil
	}
	entry := rangeIndexEntry{id: id}
	boltstore.PutRangeIndexKey(entry.key[:], version, start, end, id)
	return entry, true, nil
}

func prefixRangeForRecord(record boltstore.Route) (boltstore.Address, boltstore.Address) {
	start := record.Network
	end := start
	bits, offset := 128, 0
	if record.Version == 4 {
		bits, offset = 32, 12
	}
	for bit := int(record.PrefixLength); bit < bits; bit++ {
		end[offset+bit/8] |= 1 << uint(7-bit%8)
	}
	return start, end
}

func directRangeEntries(ctx context.Context, bucket *bbolt.Bucket, version uint8, start, end boltstore.Address, after *[boltstore.RangeIndexKeySize]byte, maximum int) ([]rangeIndexEntry, bool, error) {
	if maximum < 1 {
		maximum = 1
	}
	var seek [boltstore.RangeIndexKeySize]byte
	if after != nil {
		seek = *after
	} else {
		boltstore.RangeIndexStartKey(seek[:], version, start)
	}
	cursor := bucket.Cursor()
	key, _ := cursor.Seek(seek[:])
	if after != nil && bytes.Equal(key, after[:]) {
		key, _ = cursor.Next()
	}
	entries := make([]rangeIndexEntry, 0, maximum)
	for ; len(key) == boltstore.RangeIndexKeySize && key[0] == version; key, _ = cursor.Next() {
		entryStart, _ := boltstore.RangeIndexStart(key)
		if bytes.Compare(entryStart[:], end[:]) > 0 {
			break
		}
		if err := contextError(ctx); err != nil {
			return nil, false, err
		}
		id, _ := boltstore.RangeIndexID(key)
		entry := rangeIndexEntry{id: id}
		copy(entry.key[:], key)
		entries = append(entries, entry)
		if len(entries) == maximum {
			return entries, true, nil
		}
	}
	return entries, false, nil
}

func encodeRangeCursor(phase byte, kind RangeKind, start, end boltstore.Address, key [boltstore.RangeIndexKeySize]byte) nullableString {
	payload := make([]byte, rangeCursorSize)
	payload[0] = phase
	payload[1] = rangeKindValue(kind)
	copy(payload[2:18], start[:])
	copy(payload[18:34], end[:])
	copy(payload[34:], key[:])
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return &encoded
}

func decodeRangeCursor(value string) (rangeCursor, error) {
	if value == "" {
		return rangeCursor{}, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(payload) != rangeCursorSize || (payload[0] != rangeCursorCovering && payload[0] != rangeCursorDirect) {
		return rangeCursor{}, errInvalidRangeCursor
	}
	result := rangeCursor{phase: payload[0], kind: rangeKindFromValue(payload[1])}
	if result.kind == "" {
		return rangeCursor{}, errInvalidRangeCursor
	}
	copy(result.queryStart[:], payload[2:18])
	copy(result.queryEnd[:], payload[18:34])
	copy(result.key[:], payload[34:])
	return result, nil
}

func rangeKindValue(kind RangeKind) byte {
	if kind == RangeAllocations {
		return 1
	}
	return 2
}

func rangeKindFromValue(value byte) RangeKind {
	switch value {
	case 1:
		return RangeAllocations
	case 2:
		return RangeRoutes
	default:
		return ""
	}
}

func (repository *BboltRepository) LookupRangeSummary(ctx context.Context, value ipkey.ParsedRange, kind RangeKind) (*RangeResponse, error) {
	keys, ok := ipkey.SummaryPrefixKeys(value)
	if !ok {
		return nil, fmt.Errorf("range is not eligible for a generated summary")
	}
	response := &RangeResponse{Range: rangeDescriptor(value), Kind: kind, Mode: "summary"}
	summary := &RangeSummary{Aggregation: "overlapping_source_records", BucketPrefixLength: summaryBucketLength(keys[0]), Buckets: len(keys), Countries: []RangeFacet{}, ASNs: []RangeFacet{}}
	countries, asns := make(map[string]int64), make(map[string]int64)
	err := repository.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(boltstore.BucketRangeSummaries)
		for _, key := range keys {
			if err := contextError(ctx); err != nil {
				return err
			}
			prefix, err := netip.ParsePrefix(key)
			if err != nil {
				return err
			}
			raw := bucket.Get(boltstore.PrefixKey(prefix))
			if raw == nil {
				continue
			}
			item, err := boltstore.DecodeSummary(raw)
			if err != nil {
				return err
			}
			summary.AllocationRecords += int64(item.AllocationRecords)
			summary.RouteRecords += int64(item.Routes)
			for _, facet := range item.Countries {
				countries[facet.Value] += int64(facet.Count)
			}
			for _, facet := range item.ASNs {
				asns[facet.Value] += int64(facet.Count)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bbolt range summary lookup: %w", err)
	}
	summary.Countries, summary.ASNs = rankFacets(countries), rankFacets(asns)
	response.Summary = summary
	return response, nil
}

func (repository *BboltRepository) LookupASN(ctx context.Context, asn uint32, page Page) (*ASNResponse, error) {
	var autnum *AutnumObject
	var routes []RouteObject
	var total int64
	err := repository.db.View(func(tx *bbolt.Tx) error {
		rawAutnum := tx.Bucket(boltstore.BucketAutnums).Get(boltstore.ASNKey(asn))
		if rawAutnum != nil {
			record, err := boltstore.DecodeAutnum(rawAutnum)
			if err != nil {
				return err
			}
			object := autnumObject(record)
			autnum = &object
		}
		rawIDs := tx.Bucket(boltstore.BucketASNRoutes).Get(boltstore.ASNKey(asn))
		if len(rawIDs)%boltstore.IDKeySize != 0 {
			return fmt.Errorf("invalid ASN route list")
		}
		total = int64(len(rawIDs) / boltstore.IDKeySize)
		start := 0
		if page.Numbered {
			start64 := int64(page.Number-1) * int64(page.Limit)
			if start64 < total {
				start = int(start64)
			} else {
				start = int(total)
			}
		} else if page.Cursor > 0 {
			start = sort.Search(int(total), func(index int) bool {
				return int64(binary.BigEndian.Uint32(rawIDs[index*boltstore.IDKeySize:])) > page.Cursor
			})
		}
		end := start + page.Limit
		if !page.Numbered {
			end++
		}
		if end > int(total) {
			end = int(total)
		}
		for index := start; index < end; index++ {
			if err := contextError(ctx); err != nil {
				return err
			}
			id := binary.BigEndian.Uint32(rawIDs[index*boltstore.IDKeySize:])
			record, err := routeByID(tx, id)
			if err != nil {
				return err
			}
			routes = append(routes, routeObject(id, record))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("bbolt ASN lookup: %w", err)
	}
	if autnum == nil && total == 0 {
		return nil, nil
	}
	response := &ASNResponse{ASN: "AS" + strconv.FormatUint(uint64(asn), 10), ASNumber: int(asn), Autnum: autnum}
	if page.Numbered {
		response.Routes = RoutePage{Items: routes, Page: page.Number, TotalPages: int((total + int64(page.Limit) - 1) / int64(page.Limit)), TotalItems: total}
	} else {
		routes, next := routePage(routes, page.Limit)
		response.Routes = RoutePage{Items: routes, NextCursor: next}
	}
	return response, nil
}

func matchingIPIDs(ctx context.Context, index *bbolt.Bucket, address netip.Addr, limit int) ([]uint32, error) {
	bits := 128
	if address.Is4() {
		bits = 32
	}
	ids := make([]uint32, 0)
	cursor := index.Cursor()
	var seek [boltstore.IndexKeySize]byte
	for length := bits; length >= 0 && (limit == 0 || len(ids) < limit); length-- {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		base := boltstore.PutPrefixKey(seek[:], netip.PrefixFrom(address, length))
		for key, _ := cursor.Seek(seek[:]); (limit == 0 || len(ids) < limit) && len(key) == boltstore.IndexKeySize && bytes.Equal(key[:boltstore.PrefixKeySize], base); key, _ = cursor.Next() {
			id, _ := boltstore.IndexID(key)
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func overlapIDs(ctx context.Context, index *bbolt.Bucket, startValue, endValue ipkey.Parsed, after int64) ([]uint32, error) {
	startAddress := boltstore.AddressFromAddr(netip.MustParseAddr(startValue.Canonical))
	endAddress := boltstore.AddressFromAddr(netip.MustParseAddr(endValue.Canonical))
	version := byte(startValue.Version)
	ids := make(map[uint32]struct{})
	scanned := 0
	add := func(id uint32) {
		if int64(id) > after {
			ids[id] = struct{}{}
		}
	}
	cursor := index.Cursor()
	bits := 128
	if startValue.Version == 4 {
		bits = 32
	}
	startIP := netip.MustParseAddr(startValue.Canonical)
	var exactSeek [boltstore.IndexKeySize]byte
	for length := bits; length >= 0; length-- {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		base := boltstore.PutPrefixKey(exactSeek[:], netip.PrefixFrom(startIP, length))
		for key, _ := cursor.Seek(exactSeek[:]); len(key) == boltstore.IndexKeySize && bytes.Equal(key[:boltstore.PrefixKeySize], base); key, _ = cursor.Next() {
			scanned++
			if scanned > maxOverlapIndexEntries {
				return nil, errBboltQueryTooBroad
			}
			id, _ := boltstore.IndexID(key)
			add(id)
		}
	}
	seek := make([]byte, boltstore.IndexKeySize)
	seek[0] = version
	copy(seek[1:17], startAddress[:])
	for key, _ := cursor.Seek(seek); len(key) == boltstore.IndexKeySize && key[0] == version; key, _ = cursor.Next() {
		if bytes.Compare(key[1:17], endAddress[:]) > 0 {
			break
		}
		scanned++
		if scanned > maxOverlapIndexEntries {
			return nil, errBboltQueryTooBroad
		}
		id, _ := boltstore.IndexID(key)
		add(id)
		if len(ids)%4096 == 0 {
			if err := contextError(ctx); err != nil {
				return nil, err
			}
		}
	}
	result := make([]uint32, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result, nil
}

func coveringAllocation(ctx context.Context, tx *bbolt.Tx, prefix ipkey.ParsedPrefix) (*AllocationObject, error) {
	ids, err := matchingIPIDs(ctx, tx.Bucket(boltstore.BucketAllocationIndex), netip.MustParseAddr(prefix.Start.Canonical), 0)
	if err != nil {
		return nil, err
	}
	start, end := boltstore.AddressFromAddr(netip.MustParseAddr(prefix.Start.Canonical)), boltstore.AddressFromAddr(netip.MustParseAddr(prefix.End.Canonical))
	var selected boltstore.Allocation
	var selectedID uint32
	for _, id := range ids {
		record, err := allocationByID(tx, id)
		if err != nil {
			return nil, err
		}
		if bytes.Compare(record.Start[:], start[:]) > 0 || bytes.Compare(record.End[:], end[:]) < 0 {
			continue
		}
		if selectedID == 0 || bytes.Compare(record.Start[:], selected.Start[:]) > 0 || (record.Start == selected.Start && (bytes.Compare(record.End[:], selected.End[:]) < 0 || (record.End == selected.End && id < selectedID))) {
			selected, selectedID = record, id
		}
	}
	if selectedID == 0 {
		return nil, nil
	}
	object := allocationObject(selectedID, selected)
	return &object, nil
}

func allocationByID(tx *bbolt.Tx, id uint32) (boltstore.Allocation, error) {
	var key [boltstore.IDKeySize]byte
	binary.BigEndian.PutUint32(key[:], id)
	raw := tx.Bucket(boltstore.BucketAllocations).Get(key[:])
	if raw == nil {
		return boltstore.Allocation{}, fmt.Errorf("allocation %d is missing", id)
	}
	return boltstore.DecodeAllocation(raw)
}

func allocationLookupByID(tx *bbolt.Tx, id uint32) (boltstore.Allocation, error) {
	var key [boltstore.IDKeySize]byte
	binary.BigEndian.PutUint32(key[:], id)
	raw := tx.Bucket(boltstore.BucketAllocations).Get(key[:])
	if raw == nil {
		return boltstore.Allocation{}, fmt.Errorf("allocation %d is missing", id)
	}
	return boltstore.DecodeAllocationLookup(raw)
}

func routeByID(tx *bbolt.Tx, id uint32) (boltstore.Route, error) {
	var key [boltstore.IDKeySize]byte
	binary.BigEndian.PutUint32(key[:], id)
	raw := tx.Bucket(boltstore.BucketRoutes).Get(key[:])
	if raw == nil {
		return boltstore.Route{}, fmt.Errorf("route %d is missing", id)
	}
	return boltstore.DecodeRoute(raw)
}

func routeLookupByID(tx *bbolt.Tx, id uint32) (boltstore.Route, error) {
	var key [boltstore.IDKeySize]byte
	binary.BigEndian.PutUint32(key[:], id)
	raw := tx.Bucket(boltstore.BucketRoutes).Get(key[:])
	if raw == nil {
		return boltstore.Route{}, fmt.Errorf("route %d is missing", id)
	}
	return boltstore.DecodeRouteLookup(raw)
}

func geofeedByID(tx *bbolt.Tx, id uint32) (boltstore.Geofeed, error) {
	var key [boltstore.IDKeySize]byte
	binary.BigEndian.PutUint32(key[:], id)
	raw := tx.Bucket(boltstore.BucketGeofeeds).Get(key[:])
	if raw == nil {
		return boltstore.Geofeed{}, fmt.Errorf("geofeed %d is missing", id)
	}
	return boltstore.DecodeGeofeed(raw)
}

func allocationObject(id uint32, value boltstore.Allocation) AllocationObject {
	return AllocationObject{ID: int64(id), StartIP: value.Start.Addr(int(value.Version)).String(), EndIP: value.End.Addr(int(value.Version)).String(), Version: int(value.Version), Registry: lower(optional(value.Registry)), CountryCode: countryCode(upper(optional(value.Country))), CountryRaw: upper(optional(value.Country)), Name: optional(value.Name), Status: optional(value.Status), AllocationDate: optional(value.AllocationDate), Created: optional(value.Created), LastModified: optional(value.LastModified), Source: optional(value.Source), Maintainers: optional(value.Maintainers), Organization: optional(value.Organization), AbuseContact: optional(value.AbuseContact), Description: optional(value.Description)}
}

func routeObject(id uint32, value boltstore.Route) RouteObject {
	var asnNumber *int
	if value.ASNumber > 0 {
		number := int(value.ASNumber)
		asnNumber = &number
	}
	return RouteObject{ID: int64(id), Prefix: recordPrefix(value), Version: int(value.Version), OriginASN: value.OriginASN, ASNumber: asnNumber, Registry: lower(optional(value.Registry)), Source: optional(value.Source), Maintainers: optional(value.Maintainers), Organization: optional(value.Organization), AbuseContact: optional(value.AbuseContact), Description: optional(value.Description)}
}

func autnumObject(value boltstore.Autnum) AutnumObject {
	country := upper(optional(value.Country))
	return AutnumObject{ASN: value.ASN, ASNumber: int(value.ASNumber), Registry: lower(optional(value.Registry)), CountryCode: countryCode(country), CountryRaw: country, Name: optional(value.Name), Organization: optional(value.Organization), Status: optional(value.Status), Created: optional(value.Created), LastModified: optional(value.LastModified), Source: optional(value.Source), Maintainers: optional(value.Maintainers), AbuseContact: optional(value.AbuseContact), Description: optional(value.Description)}
}

func recordPrefix(value boltstore.Route) string {
	return netip.PrefixFrom(value.Network.Addr(int(value.Version)), int(value.PrefixLength)).String()
}
func optional(value string) nullableString {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
func stringValue(value []byte) nullableString {
	if len(value) == 0 {
		return nil
	}
	result := string(value)
	return &result
}
func routePrefixContains(route string, query ipkey.ParsedPrefix) bool {
	prefix, err := netip.ParsePrefix(route)
	return err == nil && prefix.Contains(netip.MustParseAddr(query.Start.Canonical)) && prefix.Contains(netip.MustParseAddr(query.End.Canonical))
}
func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (repository *BboltRepository) acquireOverlapSlot(ctx context.Context) error {
	select {
	case repository.overlapSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (repository *BboltRepository) releaseOverlapSlot() { <-repository.overlapSlots }

func rankFacets(values map[string]int64) []RangeFacet {
	items := make([]RangeFacet, 0, len(values))
	for value, count := range values {
		items = append(items, RangeFacet{Value: value, RecordCount: count})
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

func summaryBucketLength(key string) int {
	index := strings.LastIndexByte(key, '/')
	if index == -1 {
		return 0
	}
	length, _ := strconv.Atoi(key[index+1:])
	return length
}

func routePage(items []RouteObject, limit int) ([]RouteObject, nullableString) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	cursor := strconv.FormatInt(items[len(items)-1].ID, 10)
	return items, &cursor
}

func allocationPage(items []AllocationObject, limit int) ([]AllocationObject, nullableString) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	cursor := strconv.FormatInt(items[len(items)-1].ID, 10)
	return items, &cursor
}

func prefixDescriptor(prefix ipkey.ParsedPrefix) PrefixDescriptor {
	return PrefixDescriptor{
		CIDR: prefix.Canonical, Version: prefix.Version, PrefixLength: prefix.PrefixLength,
		StartIP: prefix.Start.Canonical, EndIP: prefix.End.Canonical, AddressCount: prefix.AddressCount,
	}
}

func rangeDescriptor(value ipkey.ParsedRange) RangeDescriptor {
	return RangeDescriptor{
		StartIP: value.Start.Canonical, EndIP: value.End.Canonical,
		Version: value.Version, AddressCount: value.AddressCount,
	}
}
