package boltstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	SchemaVersion     = 2
	PrefixKeySize     = 18
	IndexKeySize      = PrefixKeySize + 8
	RangeIndexKeySize = 1 + 16 + 16 + 8
)

var (
	BucketMetadata        = []byte("metadata")
	BucketAllocations     = []byte("allocations")
	BucketRoutes          = []byte("routes")
	BucketGeofeeds        = []byte("geofeeds")
	BucketAutnums         = []byte("autnums")
	BucketAllocationIndex = []byte("allocation_prefixes")
	BucketRouteIndex      = []byte("route_prefixes")
	BucketGeofeedIndex    = []byte("geofeed_prefixes")
	BucketAllocationRange = []byte("allocation_ranges")
	BucketRouteRange      = []byte("route_ranges")
	BucketASNRoutes       = []byte("asn_routes")
	BucketRangeSummaries  = []byte("range_summaries")
	KeySchemaVersion      = []byte("schema_version")
	KeyReleaseTag         = []byte("release_tag")
	KeyBuiltAt            = []byte("built_at")
	KeySourceCommit       = []byte("source_commit")
	RequiredBuckets       = [][]byte{BucketMetadata, BucketAllocations, BucketRoutes, BucketGeofeeds, BucketAutnums, BucketAllocationIndex, BucketRouteIndex, BucketGeofeedIndex, BucketAllocationRange, BucketRouteRange, BucketASNRoutes, BucketRangeSummaries}
)

type Address [16]byte

type Allocation struct {
	Start, End                                                             Address
	Version                                                                uint8
	Registry, Country, Name, Status, AllocationDate, Created, LastModified string
	Source, Maintainers, Organization, AbuseContact, Description           string
}

type Route struct {
	Network                                                              Address
	Version, PrefixLength                                                uint8
	ASNumber                                                             uint32
	OriginASN, Registry, Source, Maintainers, Organization, AbuseContact string
	Description                                                          string
}

type Geofeed struct {
	Start, End            Address
	Version               uint8
	Country, Region, City string
}

type Autnum struct {
	ASNumber                                                     uint32
	ASN, Registry, Country, Name, Organization, Status, Created  string
	LastModified, Source, Maintainers, AbuseContact, Description string
}

type Facet struct {
	Value string
	Count uint64
}

type RangeSummary struct {
	PrefixLength              uint8
	AllocationRecords, Routes uint64
	Countries, ASNs           []Facet
}

func AddressFromAddr(value netip.Addr) Address {
	value = value.Unmap()
	var result Address
	if value.Is4() {
		bytes := value.As4()
		copy(result[12:], bytes[:])
		return result
	}
	bytes := value.As16()
	copy(result[:], bytes[:])
	return result
}

func (value Address) Addr(version int) netip.Addr {
	if version == 4 {
		var bytes [4]byte
		copy(bytes[:], value[12:])
		return netip.AddrFrom4(bytes)
	}
	return netip.AddrFrom16([16]byte(value))
}

func IDKey(id uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, id)
	return key
}

func ASNKey(asn uint32) []byte {
	key := make([]byte, 4)
	binary.BigEndian.PutUint32(key, asn)
	return key
}

func PrefixKey(prefix netip.Prefix) []byte {
	prefix = prefix.Masked()
	key := make([]byte, PrefixKeySize)
	return PutPrefixKey(key, prefix)
}

func PutPrefixKey(key []byte, prefix netip.Prefix) []byte {
	if len(key) < PrefixKeySize {
		panic("bbolt prefix key buffer is too small")
	}
	prefix = prefix.Masked()
	if prefix.Addr().Is4() {
		key[0] = 4
	} else {
		key[0] = 6
	}
	address := AddressFromAddr(prefix.Addr())
	copy(key[1:17], address[:])
	key[17] = byte(prefix.Bits())
	return key[:PrefixKeySize]
}

func IndexKey(prefix netip.Prefix, id uint64) []byte {
	key := make([]byte, IndexKeySize)
	copy(key, PrefixKey(prefix))
	binary.BigEndian.PutUint64(key[PrefixKeySize:], id)
	return key
}

func IndexID(key []byte) (uint64, bool) {
	if len(key) != IndexKeySize {
		return 0, false
	}
	return binary.BigEndian.Uint64(key[PrefixKeySize:]), true
}

// RangeIndexKey orders original source records by IP version, start address,
// end address, and stable record ID. It allows a range page to seek directly
// to its next record instead of rebuilding a full overlap result set.
func RangeIndexKey(version uint8, start, end Address, id uint64) []byte {
	key := make([]byte, RangeIndexKeySize)
	return PutRangeIndexKey(key, version, start, end, id)
}

func PutRangeIndexKey(key []byte, version uint8, start, end Address, id uint64) []byte {
	if len(key) < RangeIndexKeySize {
		panic("bbolt range index key buffer is too small")
	}
	key[0] = version
	copy(key[1:17], start[:])
	copy(key[17:33], end[:])
	binary.BigEndian.PutUint64(key[33:41], id)
	return key[:RangeIndexKeySize]
}

func RangeIndexStartKey(key []byte, version uint8, start Address) []byte {
	if len(key) < RangeIndexKeySize {
		panic("bbolt range index key buffer is too small")
	}
	key[0] = version
	copy(key[1:17], start[:])
	clear(key[17:RangeIndexKeySize])
	return key[:RangeIndexKeySize]
}

func RangeIndexStart(key []byte) (Address, bool) {
	if len(key) != RangeIndexKeySize {
		return Address{}, false
	}
	var start Address
	copy(start[:], key[1:17])
	return start, true
}

func RangeIndexID(key []byte) (uint64, bool) {
	if len(key) != RangeIndexKeySize {
		return 0, false
	}
	return binary.BigEndian.Uint64(key[33:41]), true
}

func EncodeIDs(ids []uint64) []byte {
	value := make([]byte, len(ids)*8)
	for index, id := range ids {
		binary.BigEndian.PutUint64(value[index*8:], id)
	}
	return value
}

func DecodeIDs(value []byte) ([]uint64, error) {
	if len(value)%8 != 0 {
		return nil, errors.New("invalid ID list")
	}
	ids := make([]uint64, len(value)/8)
	for index := range ids {
		ids[index] = binary.BigEndian.Uint64(value[index*8:])
	}
	return ids, nil
}

type encoder struct{ value []byte }

func (writer *encoder) byte(value byte) { writer.value = append(writer.value, value) }
func (writer *encoder) uint32(value uint32) {
	writer.value = binary.BigEndian.AppendUint32(writer.value, value)
}
func (writer *encoder) uint64(value uint64) {
	writer.value = binary.BigEndian.AppendUint64(writer.value, value)
}
func (writer *encoder) address(value Address) { writer.value = append(writer.value, value[:]...) }
func (writer *encoder) string(value string) {
	writer.value = binary.AppendUvarint(writer.value, uint64(len(value)))
	writer.value = append(writer.value, value...)
}

type decoder struct {
	value  []byte
	offset int
}

func (reader *decoder) take(size int) ([]byte, error) {
	if size < 0 || reader.offset+size > len(reader.value) {
		return nil, errors.New("truncated bbolt record")
	}
	value := reader.value[reader.offset : reader.offset+size]
	reader.offset += size
	return value, nil
}

func (reader *decoder) byte() (byte, error) {
	value, err := reader.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (reader *decoder) uint32() (uint32, error) {
	value, err := reader.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(value), nil
}

func (reader *decoder) uint64() (uint64, error) {
	value, err := reader.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(value), nil
}

func (reader *decoder) address() (Address, error) {
	value, err := reader.take(16)
	if err != nil {
		return Address{}, err
	}
	var result Address
	copy(result[:], value)
	return result, nil
}

func (reader *decoder) string() (string, error) {
	length, read := binary.Uvarint(reader.value[reader.offset:])
	if read <= 0 {
		return "", errors.New("invalid bbolt string length")
	}
	reader.offset += read
	value, err := reader.take(int(length))
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func (reader *decoder) done() error {
	if reader.offset != len(reader.value) {
		return fmt.Errorf("bbolt record has %d trailing bytes", len(reader.value)-reader.offset)
	}
	return nil
}

func EncodeAllocation(value Allocation) []byte {
	w := encoder{}
	w.byte(value.Version)
	w.address(value.Start)
	w.address(value.End)
	for _, item := range []string{value.Registry, value.Country, value.Name, value.Status, value.AllocationDate, value.Created, value.LastModified, value.Source, value.Maintainers, value.Organization, value.AbuseContact, value.Description} {
		w.string(item)
	}
	return w.value
}

func DecodeAllocation(value []byte) (Allocation, error) {
	r := decoder{value: value}
	result := Allocation{}
	var err error
	if result.Version, err = r.byte(); err != nil {
		return result, err
	}
	if result.Start, err = r.address(); err != nil {
		return result, err
	}
	if result.End, err = r.address(); err != nil {
		return result, err
	}
	items := []*string{&result.Registry, &result.Country, &result.Name, &result.Status, &result.AllocationDate, &result.Created, &result.LastModified, &result.Source, &result.Maintainers, &result.Organization, &result.AbuseContact, &result.Description}
	for _, item := range items {
		if *item, err = r.string(); err != nil {
			return result, err
		}
	}
	return result, r.done()
}

func EncodeRoute(value Route) []byte {
	w := encoder{}
	w.byte(value.Version)
	w.byte(value.PrefixLength)
	w.address(value.Network)
	w.uint32(value.ASNumber)
	for _, item := range []string{value.OriginASN, value.Registry, value.Source, value.Maintainers, value.Organization, value.AbuseContact, value.Description} {
		w.string(item)
	}
	return w.value
}

func DecodeRoute(value []byte) (Route, error) {
	r := decoder{value: value}
	result := Route{}
	var err error
	if result.Version, err = r.byte(); err != nil {
		return result, err
	}
	if result.PrefixLength, err = r.byte(); err != nil {
		return result, err
	}
	if result.Network, err = r.address(); err != nil {
		return result, err
	}
	if result.ASNumber, err = r.uint32(); err != nil {
		return result, err
	}
	items := []*string{&result.OriginASN, &result.Registry, &result.Source, &result.Maintainers, &result.Organization, &result.AbuseContact, &result.Description}
	for _, item := range items {
		if *item, err = r.string(); err != nil {
			return result, err
		}
	}
	return result, r.done()
}

func EncodeGeofeed(value Geofeed) []byte {
	w := encoder{}
	w.byte(value.Version)
	w.address(value.Start)
	w.address(value.End)
	w.string(value.Country)
	w.string(value.Region)
	w.string(value.City)
	return w.value
}

func DecodeGeofeed(value []byte) (Geofeed, error) {
	r := decoder{value: value}
	result := Geofeed{}
	var err error
	if result.Version, err = r.byte(); err != nil {
		return result, err
	}
	if result.Start, err = r.address(); err != nil {
		return result, err
	}
	if result.End, err = r.address(); err != nil {
		return result, err
	}
	for _, item := range []*string{&result.Country, &result.Region, &result.City} {
		if *item, err = r.string(); err != nil {
			return result, err
		}
	}
	return result, r.done()
}

func EncodeAutnum(value Autnum) []byte {
	w := encoder{}
	w.uint32(value.ASNumber)
	for _, item := range []string{value.ASN, value.Registry, value.Country, value.Name, value.Organization, value.Status, value.Created, value.LastModified, value.Source, value.Maintainers, value.AbuseContact, value.Description} {
		w.string(item)
	}
	return w.value
}

func DecodeAutnum(value []byte) (Autnum, error) {
	r := decoder{value: value}
	result := Autnum{}
	var err error
	if result.ASNumber, err = r.uint32(); err != nil {
		return result, err
	}
	items := []*string{&result.ASN, &result.Registry, &result.Country, &result.Name, &result.Organization, &result.Status, &result.Created, &result.LastModified, &result.Source, &result.Maintainers, &result.AbuseContact, &result.Description}
	for _, item := range items {
		if *item, err = r.string(); err != nil {
			return result, err
		}
	}
	return result, r.done()
}

func EncodeSummary(value RangeSummary) []byte {
	w := encoder{}
	w.byte(value.PrefixLength)
	w.uint64(value.AllocationRecords)
	w.uint64(value.Routes)
	for _, facets := range [][]Facet{value.Countries, value.ASNs} {
		w.uint32(uint32(len(facets)))
		for _, facet := range facets {
			w.string(facet.Value)
			w.uint64(facet.Count)
		}
	}
	return w.value
}

func DecodeSummary(value []byte) (RangeSummary, error) {
	r := decoder{value: value}
	result := RangeSummary{}
	var err error
	if result.PrefixLength, err = r.byte(); err != nil {
		return result, err
	}
	if result.AllocationRecords, err = r.uint64(); err != nil {
		return result, err
	}
	if result.Routes, err = r.uint64(); err != nil {
		return result, err
	}
	for _, destination := range []*[]Facet{&result.Countries, &result.ASNs} {
		count, readErr := r.uint32()
		if readErr != nil {
			return result, readErr
		}
		if count > 1_000_000 {
			return result, fmt.Errorf("invalid facet count %d", count)
		}
		items := make([]Facet, 0, count)
		for index := uint32(0); index < count; index++ {
			value, valueErr := r.string()
			if valueErr != nil {
				return result, valueErr
			}
			number, numberErr := r.uint64()
			if numberErr != nil {
				return result, numberErr
			}
			items = append(items, Facet{Value: value, Count: number})
		}
		*destination = items
	}
	return result, r.done()
}
