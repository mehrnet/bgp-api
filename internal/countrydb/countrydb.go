// Package countrydb builds and reads the MNCC format: a minimal, read-only
// IP -> ISO 3166-1 alpha-2 country code database intended to be bundled into
// small clients (for example an Android app).
//
// The address space is quantized: IPv4 to /v4Bits (default /24) and IPv6 to
// /v6Bits (default /48). A source range is used only when it is aligned to
// that granularity; narrower records are ignored, so a more general range
// still answers for those addresses. Adjacent slots with the same country are
// merged and the remaining range starts are delta/varint encoded in blocks
// with a small absolute index for binary search.
//
// File layout (all integers little endian, varints are LEB128 uvarint):
//
//	"MNCC"                      magic
//	u8 format (=1)
//	u8 v4Bits, u8 v6Bits
//	u8 blockSize                entries per block
//	u8 countryCount             n, country index 0 is "unknown"
//	n * 2 bytes                 ASCII country codes, index 1..n
//	family v4, then family v6:
//	  u32 entryCount
//	  u32 dataLen
//	  blocks * (key, u32 offset)  key = ceil(bits/8) bytes, first block key is 0
//	  data                      per entry: uvarint(start delta), u8 country index
//
// An entry's range runs from its start to the next entry's start. The first
// entry of every block has delta 0 (its start is the block key).
package countrydb

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
)

const (
	magic            = "MNCC"
	formatVersion    = 1
	DefaultV4Bits    = 24
	DefaultV6Bits    = 48
	DefaultBlockSize = 64
	maxBits          = 62
)

// Layer priorities: a higher layer always wins over a lower one.
const (
	LayerAllocation = 1
	LayerGeofeed    = 2
)

// Skip reasons returned by Builder.Add.
type Result int

const (
	Added Result = iota
	SkippedCountry
	SkippedGranularity
	SkippedAddress
)

type interval struct {
	start, end uint64
	country    uint16
	layer      int
	seq        int
}

// Builder accumulates source ranges and encodes the final database.
type Builder struct {
	v4Bits, v6Bits int
	blockSize      int
	v4, v6         []interval
	seq            int
}

type Stats struct {
	V4Entries, V6Entries int
	Countries            int
	Bytes                int
}

func NewBuilder(v4Bits, v6Bits int) (*Builder, error) {
	if v4Bits < 1 || v4Bits > 32 {
		return nil, fmt.Errorf("v4 prefix must be 1..32, got %d", v4Bits)
	}
	if v6Bits < 1 || v6Bits > maxBits {
		return nil, fmt.Errorf("v6 prefix must be 1..%d, got %d", maxBits, v6Bits)
	}
	return &Builder{v4Bits: v4Bits, v6Bits: v6Bits, blockSize: DefaultBlockSize}, nil
}

func packCountry(code string) (uint16, bool) {
	if len(code) != 2 {
		return 0, false
	}
	for i := 0; i < 2; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return 0, false
		}
	}
	return uint16(code[0])<<8 | uint16(code[1]), true
}

// slotRange converts an inclusive address range to quantized slot bounds.
// ok is false when the range is not aligned to the granularity.
func slotRange(start, end netip.Addr, v4Bits, v6Bits int) (family int, lo, hi uint64, ok bool) {
	start, end = start.Unmap(), end.Unmap()
	if !start.IsValid() || !end.IsValid() || start.Is4() != end.Is4() || start.Compare(end) > 0 {
		return 0, 0, 0, false
	}
	if start.Is4() {
		a, b := start.As4(), end.As4()
		s, e := uint64(binary.BigEndian.Uint32(a[:])), uint64(binary.BigEndian.Uint32(b[:]))
		shift := uint(32 - v4Bits)
		if s&(1<<shift-1) != 0 || e&(1<<shift-1) != 1<<shift-1 {
			return 4, 0, 0, false
		}
		return 4, s >> shift, e >> shift, true
	}
	a, b := start.As16(), end.As16()
	sHi, sLo := binary.BigEndian.Uint64(a[:8]), binary.BigEndian.Uint64(a[8:])
	eHi, eLo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	shift := uint(64 - v6Bits)
	if sLo != 0 || eLo != ^uint64(0) || sHi&(1<<shift-1) != 0 || eHi&(1<<shift-1) != 1<<shift-1 {
		return 6, 0, 0, false
	}
	return 6, sHi >> shift, eHi >> shift, true
}

// Add registers one source range (inclusive bounds). country must be exactly
// two uppercase ASCII letters.
func (b *Builder) Add(start, end netip.Addr, country string, layer int) Result {
	cc, ok := packCountry(country)
	if !ok {
		return SkippedCountry
	}
	family, lo, hi, ok := slotRange(start, end, b.v4Bits, b.v6Bits)
	if family == 0 {
		return SkippedAddress
	}
	if !ok {
		return SkippedGranularity
	}
	b.seq++
	iv := interval{start: lo, end: hi, country: cc, layer: layer, seq: b.seq}
	if family == 4 {
		b.v4 = append(b.v4, iv)
	} else {
		b.v6 = append(b.v6, iv)
	}
	return Added
}

type segment struct {
	start   uint64
	country uint16 // 0 = unknown
}

type activeHeap []interval

func (h activeHeap) Len() int { return len(h) }
func (h activeHeap) Less(i, j int) bool { // true when i outranks j
	a, b := h[i], h[j]
	if a.layer != b.layer {
		return a.layer > b.layer
	}
	if aw, bw := a.end-a.start, b.end-b.start; aw != bw {
		return aw < bw
	}
	return a.seq > b.seq
}
func (h activeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *activeHeap) Push(x any)   { *h = append(*h, x.(interval)) }
func (h *activeHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// flatten resolves overlaps into a partition of [0, 1<<bits) starting at 0.
func flatten(ivs []interval, bits int) []segment {
	limit := uint64(1) << uint(bits)
	sort.Slice(ivs, func(i, j int) bool {
		if ivs[i].start != ivs[j].start {
			return ivs[i].start < ivs[j].start
		}
		return ivs[i].seq < ivs[j].seq
	})
	out := []segment{{0, 0}}
	emit := func(at uint64, country uint16) {
		if at >= limit {
			return
		}
		if out[len(out)-1].country == country {
			return
		}
		if out[len(out)-1].start == at {
			out[len(out)-1].country = country
			// The replacement may now equal the previous segment.
			if len(out) > 1 && out[len(out)-2].country == country {
				out = out[:len(out)-1]
			}
			return
		}
		out = append(out, segment{at, country})
	}
	var active activeHeap
	i, n := 0, len(ivs)
	var p uint64
	for i < n || active.Len() > 0 {
		if active.Len() == 0 {
			p = ivs[i].start
		}
		for i < n && ivs[i].start <= p {
			heap.Push(&active, ivs[i])
			i++
		}
		for active.Len() > 0 && active[0].end < p {
			heap.Pop(&active)
		}
		if active.Len() == 0 {
			emit(p, 0)
			continue
		}
		top := active[0]
		emit(p, top.country)
		next := top.end + 1
		if i < n && ivs[i].start < next {
			next = ivs[i].start
		}
		p = next
	}
	return out
}

func keyBytes(bits int) int { return (bits + 7) / 8 }

func putKey(dst []byte, v uint64) {
	for i := range dst {
		dst[i] = byte(v >> (8 * uint(i)))
	}
}

func getKey(src []byte) uint64 {
	var v uint64
	for i := len(src) - 1; i >= 0; i-- {
		v = v<<8 | uint64(src[i])
	}
	return v
}

func encodeFamily(buf *bytes.Buffer, segs []segment, bits, blockSize int, index map[uint16]byte) {
	kb := keyBytes(bits)
	blocks := (len(segs) + blockSize - 1) / blockSize
	var data bytes.Buffer
	type blockInfo struct {
		key    uint64
		offset uint32
	}
	infos := make([]blockInfo, 0, blocks)
	var tmp [binary.MaxVarintLen64]byte
	var prev uint64
	for i, s := range segs {
		if i%blockSize == 0 {
			infos = append(infos, blockInfo{s.start, uint32(data.Len())})
			prev = s.start
		}
		n := binary.PutUvarint(tmp[:], s.start-prev)
		data.Write(tmp[:n])
		data.WriteByte(index[s.country])
		prev = s.start
	}
	var word [4]byte
	binary.LittleEndian.PutUint32(word[:], uint32(len(segs)))
	buf.Write(word[:])
	binary.LittleEndian.PutUint32(word[:], uint32(data.Len()))
	buf.Write(word[:])
	key := make([]byte, kb)
	for _, info := range infos {
		putKey(key, info.key)
		buf.Write(key)
		binary.LittleEndian.PutUint32(word[:], info.offset)
		buf.Write(word[:])
	}
	buf.Write(data.Bytes())
}

// Encode flattens all added ranges and writes the database.
func (b *Builder) Encode(w io.Writer) (Stats, error) {
	segs4 := flatten(b.v4, b.v4Bits)
	segs6 := flatten(b.v6, b.v6Bits)

	seen := map[uint16]struct{}{}
	for _, list := range [][]segment{segs4, segs6} {
		for _, s := range list {
			if s.country != 0 {
				seen[s.country] = struct{}{}
			}
		}
	}
	codes := make([]uint16, 0, len(seen))
	for code := range seen {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	if len(codes) > 255 {
		return Stats{}, errors.New("more than 255 distinct country codes")
	}
	index := map[uint16]byte{0: 0}
	for i, code := range codes {
		index[code] = byte(i + 1)
	}

	var buf bytes.Buffer
	buf.WriteString(magic)
	buf.Write([]byte{formatVersion, byte(b.v4Bits), byte(b.v6Bits), byte(b.blockSize), byte(len(codes))})
	for _, code := range codes {
		buf.Write([]byte{byte(code >> 8), byte(code)})
	}
	encodeFamily(&buf, segs4, b.v4Bits, b.blockSize, index)
	encodeFamily(&buf, segs6, b.v6Bits, b.blockSize, index)
	if _, err := w.Write(buf.Bytes()); err != nil {
		return Stats{}, err
	}
	return Stats{V4Entries: len(segs4), V6Entries: len(segs6), Countries: len(codes), Bytes: buf.Len()}, nil
}

// DB is a zero-copy reader over an MNCC file.
type DB struct {
	v4Bits, v6Bits int
	blockSize      int
	countries      []byte // 2 bytes per country, index 1..n
	v4, v6         family
}

type family struct {
	bits    int
	entries int
	index   []byte // blocks * (keyBytes + 4)
	data    []byte
}

var ErrFormat = errors.New("countrydb: invalid or truncated database")

// Open parses data (which must outlive the DB).
func Open(data []byte) (*DB, error) {
	if len(data) < 9 || string(data[:4]) != magic || data[4] != formatVersion {
		return nil, ErrFormat
	}
	db := &DB{v4Bits: int(data[5]), v6Bits: int(data[6]), blockSize: int(data[7])}
	if db.v4Bits < 1 || db.v4Bits > 32 || db.v6Bits < 1 || db.v6Bits > maxBits || db.blockSize < 1 {
		return nil, ErrFormat
	}
	n := int(data[8])
	pos := 9
	if len(data) < pos+2*n {
		return nil, ErrFormat
	}
	db.countries = data[pos : pos+2*n]
	pos += 2 * n
	var err error
	if db.v4, pos, err = openFamily(data, pos, db.v4Bits, db.blockSize); err != nil {
		return nil, err
	}
	if db.v6, _, err = openFamily(data, pos, db.v6Bits, db.blockSize); err != nil {
		return nil, err
	}
	return db, nil
}

func openFamily(data []byte, pos, bits, blockSize int) (family, int, error) {
	if len(data) < pos+8 {
		return family{}, 0, ErrFormat
	}
	entries := int(binary.LittleEndian.Uint32(data[pos:]))
	dataLen := int(binary.LittleEndian.Uint32(data[pos+4:]))
	pos += 8
	if entries < 1 {
		return family{}, 0, ErrFormat
	}
	indexLen := (entries + blockSize - 1) / blockSize * (keyBytes(bits) + 4)
	if len(data) < pos+indexLen+dataLen {
		return family{}, 0, ErrFormat
	}
	f := family{bits: bits, entries: entries, index: data[pos : pos+indexLen], data: data[pos+indexLen : pos+indexLen+dataLen]}
	return f, pos + indexLen + dataLen, nil
}

func (db *DB) lookup(f family, key uint64) (string, bool) {
	kb := keyBytes(f.bits)
	stride := kb + 4
	blocks := len(f.index) / stride
	// Last block whose key <= lookup key.
	b := sort.Search(blocks, func(i int) bool { return getKey(f.index[i*stride:i*stride+kb]) > key }) - 1
	if b < 0 {
		return "", false
	}
	start := getKey(f.index[b*stride : b*stride+kb])
	offset := int(binary.LittleEndian.Uint32(f.index[b*stride+kb:]))
	end := len(f.data)
	if b+1 < blocks {
		end = int(binary.LittleEndian.Uint32(f.index[(b+1)*stride+kb:]))
	}
	if offset > end || end > len(f.data) {
		return "", false
	}
	var country byte
	for offset < end {
		delta, n := binary.Uvarint(f.data[offset:end])
		if n <= 0 || offset+n >= end {
			return "", false
		}
		start += delta
		if start > key {
			break
		}
		country = f.data[offset+n]
		offset += n + 1
	}
	if country == 0 || int(country) > len(db.countries)/2 {
		return "", false
	}
	i := (int(country) - 1) * 2
	return string(db.countries[i : i+2]), true
}

// Lookup returns the country code for addr, or ok=false when unknown.
func (db *DB) Lookup(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	switch {
	case addr.Is4():
		a := addr.As4()
		return db.lookup(db.v4, uint64(binary.BigEndian.Uint32(a[:]))>>uint(32-db.v4Bits))
	case addr.Is6():
		a := addr.As16()
		return db.lookup(db.v6, binary.BigEndian.Uint64(a[:8])>>uint(64-db.v6Bits))
	}
	return "", false
}
