package boltstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	bbolt "go.etcd.io/bbolt"
)

const buildBatchSize = 10_000

type BuildOptions struct {
	InputDir     string
	OutputPath   string
	ReleaseTag   string
	BuiltAt      string
	SourceCommit string
	Compact      bool
}

type BuildStats struct {
	Allocations, Routes, Geofeeds, Autnums    uint64
	AllocationIndex, RouteIndex, GeofeedIndex uint64
}

type summaryAccumulator struct {
	allocations, routes uint64
	countries, asns     map[string]uint64
}

type builder struct {
	db        *bbolt.DB
	stats     BuildStats
	asnRoutes map[uint32][]uint64
	summaries map[string]*summaryAccumulator
}

func Build(ctx context.Context, options BuildOptions) (BuildStats, error) {
	if options.InputDir == "" || options.OutputPath == "" {
		return BuildStats{}, errors.New("input directory and output path are required")
	}
	for _, name := range []string{"allocations.csv", "routes.csv", "autnums.csv", "geolocations.csv"} {
		if info, err := os.Stat(filepath.Join(options.InputDir, name)); err != nil || info.IsDir() {
			if err == nil {
				err = errors.New("path is a directory")
			}
			return BuildStats{}, fmt.Errorf("open %s: %w", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(options.OutputPath), 0o755); err != nil {
		return BuildStats{}, err
	}
	rawPath := options.OutputPath + ".building"
	compactPath := options.OutputPath + ".compact"
	for _, path := range []string{rawPath, compactPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return BuildStats{}, fmt.Errorf("remove stale build file %s: %w", path, err)
		}
	}
	db, err := bbolt.Open(rawPath, 0o600, &bbolt.Options{
		NoSync: true, NoGrowSync: true, NoFreelistSync: true,
		FreelistType: bbolt.FreelistMapType, InitialMmapSize: 8 << 30,
	})
	if err != nil {
		return BuildStats{}, err
	}
	build := &builder{db: db, asnRoutes: make(map[uint32][]uint64), summaries: make(map[string]*summaryAccumulator)}
	failed := true
	defer func() {
		_ = db.Close()
		if failed {
			_ = os.Remove(rawPath)
			_ = os.Remove(compactPath)
		}
	}()
	if err := build.initialize(options); err != nil {
		return BuildStats{}, err
	}
	if err := build.allocations(ctx, filepath.Join(options.InputDir, "allocations.csv")); err != nil {
		return BuildStats{}, err
	}
	if err := build.routes(ctx, filepath.Join(options.InputDir, "routes.csv")); err != nil {
		return BuildStats{}, err
	}
	if err := build.geofeeds(ctx, filepath.Join(options.InputDir, "geolocations.csv")); err != nil {
		return BuildStats{}, err
	}
	if err := build.autnums(ctx, filepath.Join(options.InputDir, "autnums.csv")); err != nil {
		return BuildStats{}, err
	}
	if err := build.writeDerived(ctx); err != nil {
		return BuildStats{}, err
	}
	if err := db.Sync(); err != nil {
		return BuildStats{}, err
	}
	if err := db.Close(); err != nil {
		return BuildStats{}, err
	}
	build.asnRoutes = nil
	build.summaries = nil
	runtime.GC()

	if options.Compact {
		source, err := bbolt.Open(rawPath, 0o400, &bbolt.Options{ReadOnly: true, NoFreelistSync: true, NoStatistics: true})
		if err != nil {
			return BuildStats{}, err
		}
		destination, err := bbolt.Open(compactPath, 0o600, &bbolt.Options{
			NoSync: true, NoGrowSync: true, NoFreelistSync: true,
			FreelistType: bbolt.FreelistMapType, InitialMmapSize: 8 << 30,
		})
		if err != nil {
			source.Close()
			return BuildStats{}, err
		}
		err = bbolt.Compact(destination, source, 64<<20)
		if err == nil {
			err = destination.Sync()
		}
		closeDestination, closeSource := destination.Close(), source.Close()
		if err != nil {
			return BuildStats{}, err
		}
		if closeDestination != nil {
			return BuildStats{}, closeDestination
		}
		if closeSource != nil {
			return BuildStats{}, closeSource
		}
		if err := os.Rename(compactPath, options.OutputPath); err != nil {
			return BuildStats{}, err
		}
		_ = os.Remove(rawPath)
	} else if err := os.Rename(rawPath, options.OutputPath); err != nil {
		return BuildStats{}, err
	}
	failed = false
	return build.stats, nil
}

func (build *builder) initialize(options BuildOptions) error {
	return build.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range RequiredBuckets {
			bucket, err := tx.CreateBucket(name)
			if err != nil {
				return err
			}
			bucket.FillPercent = 0.9
		}
		metadata := tx.Bucket(BucketMetadata)
		version := make([]byte, 4)
		binary.BigEndian.PutUint32(version, SchemaVersion)
		for _, item := range []struct{ key, value []byte }{
			{KeySchemaVersion, version},
			{KeyReleaseTag, []byte(options.ReleaseTag)},
			{KeyBuiltAt, []byte(options.BuiltAt)},
			{KeySourceCommit, []byte(options.SourceCommit)},
		} {
			if err := metadata.Put(item.key, item.value); err != nil {
				return err
			}
		}
		return nil
	})
}

func (build *builder) allocations(ctx context.Context, path string) error {
	return forEachCSVBatch(ctx, path, func(rows [][]string) error {
		return build.db.Update(func(tx *bbolt.Tx) error {
			objects, index := tx.Bucket(BucketAllocations), tx.Bucket(BucketAllocationIndex)
			for _, row := range rows {
				if len(row) < 6 {
					continue
				}
				version, start, end, ok := parseRange(row[0], row[1], row[2])
				if !ok {
					continue
				}
				id := build.stats.Allocations + 1
				record := Allocation{Start: start, End: end, Version: version, Registry: at(row, 3), Country: at(row, 4), Name: at(row, 5), Status: at(row, 6), AllocationDate: at(row, 7), Created: at(row, 8), LastModified: at(row, 9), Source: at(row, 10), Maintainers: at(row, 11), Organization: at(row, 12), AbuseContact: at(row, 13), Description: at(row, 14)}
				if err := objects.Put(IDKey(id), EncodeAllocation(record)); err != nil {
					return err
				}
				prefixes, err := rangePrefixes(start, end, int(version))
				if err != nil {
					return err
				}
				for _, prefix := range prefixes {
					if err := index.Put(IndexKey(prefix, id), []byte{0}); err != nil {
						return err
					}
					build.stats.AllocationIndex++
				}
				build.collectSummary(start, end, int(version), false, record.Country)
				build.stats.Allocations = id
			}
			return nil
		})
	})
}

func (build *builder) routes(ctx context.Context, path string) error {
	return forEachCSVBatch(ctx, path, func(rows [][]string) error {
		return build.db.Update(func(tx *bbolt.Tx) error {
			objects, index := tx.Bucket(BucketRoutes), tx.Bucket(BucketRouteIndex)
			for _, row := range rows {
				if len(row) < 5 {
					continue
				}
				prefix, err := netip.ParsePrefix(strings.TrimSpace(row[3]))
				if err != nil || prefix.Addr().Zone() != "" {
					continue
				}
				prefix = prefix.Masked()
				version := uint8(6)
				if prefix.Addr().Is4() {
					version = 4
				}
				origin := strings.ToUpper(at(row, 4))
				if origin == "" {
					continue
				}
				asn := parseASN(origin)
				id := build.stats.Routes + 1
				record := Route{Network: AddressFromAddr(prefix.Addr()), Version: version, PrefixLength: uint8(prefix.Bits()), ASNumber: asn, OriginASN: origin, Registry: at(row, 5), Source: at(row, 6), Maintainers: at(row, 7), Organization: at(row, 8), AbuseContact: at(row, 9), Description: at(row, 10)}
				if err := objects.Put(IDKey(id), EncodeRoute(record)); err != nil {
					return err
				}
				if err := index.Put(IndexKey(prefix, id), []byte{0}); err != nil {
					return err
				}
				if asn != 0 {
					build.asnRoutes[asn] = append(build.asnRoutes[asn], id)
				}
				start, end := prefixRange(prefix)
				build.collectSummary(start, end, int(version), true, record.OriginASN)
				build.stats.RouteIndex++
				build.stats.Routes = id
			}
			return nil
		})
	})
}

func (build *builder) geofeeds(ctx context.Context, path string) error {
	return forEachCSVBatch(ctx, path, func(rows [][]string) error {
		return build.db.Update(func(tx *bbolt.Tx) error {
			objects, index := tx.Bucket(BucketGeofeeds), tx.Bucket(BucketGeofeedIndex)
			for _, row := range rows {
				if len(row) < 6 {
					continue
				}
				version, start, end, ok := parseRange(row[0], row[1], row[2])
				if !ok {
					continue
				}
				id := build.stats.Geofeeds + 1
				record := Geofeed{Start: start, End: end, Version: version, Country: at(row, 3), Region: at(row, 4), City: at(row, 5)}
				if err := objects.Put(IDKey(id), EncodeGeofeed(record)); err != nil {
					return err
				}
				prefixes, err := rangePrefixes(start, end, int(version))
				if err != nil {
					return err
				}
				for _, prefix := range prefixes {
					if err := index.Put(IndexKey(prefix, id), []byte{0}); err != nil {
						return err
					}
					build.stats.GeofeedIndex++
				}
				build.stats.Geofeeds = id
			}
			return nil
		})
	})
}

func (build *builder) autnums(ctx context.Context, path string) error {
	return forEachCSVBatch(ctx, path, func(rows [][]string) error {
		return build.db.Update(func(tx *bbolt.Tx) error {
			objects := tx.Bucket(BucketAutnums)
			for _, row := range rows {
				if len(row) < 12 {
					continue
				}
				asn := parseASN(at(row, 0))
				if asn == 0 || objects.Get(ASNKey(asn)) != nil {
					continue
				}
				record := Autnum{ASNumber: asn, ASN: strings.ToUpper(at(row, 0)), Registry: at(row, 1), Country: at(row, 2), Name: at(row, 3), Organization: at(row, 4), Status: at(row, 5), Created: at(row, 6), LastModified: at(row, 7), Source: at(row, 8), Maintainers: at(row, 9), AbuseContact: at(row, 10), Description: at(row, 11)}
				if err := objects.Put(ASNKey(asn), EncodeAutnum(record)); err != nil {
					return err
				}
				build.stats.Autnums++
			}
			return nil
		})
	})
}

func (build *builder) writeDerived(ctx context.Context) error {
	return build.db.Update(func(tx *bbolt.Tx) error {
		asnBucket := tx.Bucket(BucketASNRoutes)
		asns := make([]uint32, 0, len(build.asnRoutes))
		for asn := range build.asnRoutes {
			asns = append(asns, asn)
		}
		sort.Slice(asns, func(left, right int) bool { return asns[left] < asns[right] })
		for _, asn := range asns {
			if err := contextError(ctx); err != nil {
				return err
			}
			if err := asnBucket.Put(ASNKey(asn), EncodeIDs(build.asnRoutes[asn])); err != nil {
				return err
			}
		}
		summaryBucket := tx.Bucket(BucketRangeSummaries)
		keys := make([]string, 0, len(build.summaries))
		for key := range build.summaries {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := contextError(ctx); err != nil {
				return err
			}
			prefix := netip.MustParsePrefix(key)
			item := build.summaries[key]
			value := RangeSummary{PrefixLength: uint8(prefix.Bits()), AllocationRecords: item.allocations, Routes: item.routes, Countries: rankedFacets(item.countries), ASNs: rankedFacets(item.asns)}
			if err := summaryBucket.Put(PrefixKey(prefix), EncodeSummary(value)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (build *builder) collectSummary(start, end Address, version int, route bool, value string) {
	if version != 4 {
		return
	}
	startValue := binary.BigEndian.Uint32(start[12:])
	endValue := binary.BigEndian.Uint32(end[12:])
	for _, length := range []uint{0, 8, 16} {
		first, last := uint64(startValue)>>uint(32-length), uint64(endValue)>>uint(32-length)
		for bucket := first; bucket <= last; bucket++ {
			network := uint32(bucket << uint(32-length))
			address := netip.AddrFrom4([4]byte{byte(network >> 24), byte(network >> 16), byte(network >> 8), byte(network)})
			key := netip.PrefixFrom(address, int(length)).String()
			item := build.summaries[key]
			if item == nil {
				item = &summaryAccumulator{countries: make(map[string]uint64), asns: make(map[string]uint64)}
				build.summaries[key] = item
			}
			value = strings.ToUpper(strings.TrimSpace(value))
			if route {
				item.routes++
				if value != "" {
					item.asns[value]++
				}
			} else {
				item.allocations++
				if value != "" {
					item.countries[value]++
				}
			}
		}
	}
}

func rankedFacets(values map[string]uint64) []Facet {
	items := make([]Facet, 0, len(values))
	for value, count := range values {
		items = append(items, Facet{Value: value, Count: count})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].Count != items[right].Count {
			return items[left].Count > items[right].Count
		}
		return items[left].Value < items[right].Value
	})
	if len(items) > 10 {
		items = items[:10]
	}
	return items
}

func forEachCSVBatch(ctx context.Context, path string, callback func([][]string) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	batch := make([][]string, 0, buildBatchSize)
	for {
		if err := contextError(ctx); err != nil {
			return err
		}
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		batch = append(batch, row)
		if len(batch) == buildBatchSize {
			if err := callback(batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		return callback(batch)
	}
	return nil
}

func parseRange(startValue, endValue, versionValue string) (uint8, Address, Address, bool) {
	version, err := strconv.Atoi(strings.TrimSpace(versionValue))
	if err != nil || (version != 4 && version != 6) {
		return 0, Address{}, Address{}, false
	}
	start, ok := addressFromSortKey(startValue, version)
	if !ok {
		return 0, Address{}, Address{}, false
	}
	end, ok := addressFromSortKey(endValue, version)
	if !ok || bytes.Compare(start[:], end[:]) > 0 {
		return 0, Address{}, Address{}, false
	}
	return uint8(version), start, end, true
}

func addressFromSortKey(value string, version int) (Address, bool) {
	number, ok := new(big.Int).SetString(strings.TrimSpace(value), 10)
	if !ok || number.Sign() < 0 || number.BitLen() > 128 {
		return Address{}, false
	}
	var result Address
	number.FillBytes(result[:])
	if version == 4 {
		for index := 0; index < 12; index++ {
			result[index] = 0
		}
	}
	return result, true
}

func rangePrefixes(start, end Address, version int) ([]netip.Prefix, error) {
	bits := 128
	startBytes, endBytes := start[:], end[:]
	if version == 4 {
		bits = 32
		startBytes, endBytes = start[12:], end[12:]
	}
	current := new(big.Int).SetBytes(startBytes)
	last := new(big.Int).SetBytes(endBytes)
	result := make([]netip.Prefix, 0)
	for current.Cmp(last) <= 0 {
		blockSize := new(big.Int)
		if current.Sign() == 0 {
			blockSize.Lsh(big.NewInt(1), uint(bits))
		} else {
			blockSize.And(current, new(big.Int).Neg(current))
		}
		remaining := new(big.Int).Sub(last, current)
		remaining.Add(remaining, big.NewInt(1))
		for blockSize.Cmp(remaining) > 0 {
			blockSize.Rsh(blockSize, 1)
		}
		length := bits - (blockSize.BitLen() - 1)
		if version == 4 {
			bytesValue := current.FillBytes(make([]byte, 4))
			prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte(bytesValue)), length)
			result = append(result, prefix)
		} else {
			bytesValue := current.FillBytes(make([]byte, 16))
			prefix := netip.PrefixFrom(netip.AddrFrom16([16]byte(bytesValue)), length)
			result = append(result, prefix)
		}
		current.Add(current, blockSize)
	}
	return result, nil
}

func prefixRange(prefix netip.Prefix) (Address, Address) {
	prefix = prefix.Masked()
	start := AddressFromAddr(prefix.Addr())
	end := start
	bits := 128
	offset := 0
	if prefix.Addr().Is4() {
		bits = 32
		offset = 12
	}
	for bit := prefix.Bits(); bit < bits; bit++ {
		byteIndex := offset + bit/8
		end[byteIndex] |= 1 << uint(7-bit%8)
	}
	return start, end
}

func parseASN(value string) uint32 {
	value = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(value)), "AS")
	number, err := strconv.ParseUint(value, 10, 32)
	if err != nil || number == 0 {
		return 0
	}
	return uint32(number)
}

func at(row []string, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[index])
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
