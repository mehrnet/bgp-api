package countrydb

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"net/netip"
	"testing"
)

func v4Addr(slot uint64, bits int, last bool) netip.Addr {
	shift := uint(32 - bits)
	v := slot << shift
	if last {
		v |= 1<<shift - 1
	}
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], uint32(v))
	return netip.AddrFrom4(a)
}

func v6Addr(slot uint64, bits int, last bool) netip.Addr {
	shift := uint(64 - bits)
	hi := slot << shift
	lo := uint64(0)
	if last {
		hi |= 1<<shift - 1
		lo = ^uint64(0)
	}
	var a [16]byte
	binary.BigEndian.PutUint64(a[:8], hi)
	binary.BigEndian.PutUint64(a[8:], lo)
	return netip.AddrFrom16(a)
}

// TestRandomAgainstBruteForce compares the encoded database with a per-slot
// reference model (layer, then narrowest, then latest insertion wins).
func TestRandomAgainstBruteForce(t *testing.T) {
	const v4Bits, v6Bits = 14, 14
	codes := []string{"US", "DE", "IR", "FR", "JP"}
	for seed := int64(0); seed < 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		builder, err := NewBuilder(v4Bits, v6Bits)
		if err != nil {
			t.Fatal(err)
		}
		type rec struct {
			lo, hi  uint64
			cc      string
			layer   int
			seq     int
			version int
		}
		var recs []rec
		for i := 0; i < 300; i++ {
			version := 4
			if rng.Intn(2) == 0 {
				version = 6
			}
			size := uint64(1) << uint(rng.Intn(8))
			lo := uint64(rng.Intn(1<<14)) &^ (size - 1)
			hi := lo + size - 1
			cc := codes[rng.Intn(len(codes))]
			layer := 1 + rng.Intn(2)
			var res Result
			if version == 4 {
				res = builder.Add(v4Addr(lo, v4Bits, false), v4Addr(hi, v4Bits, true), cc, layer)
			} else {
				res = builder.Add(v6Addr(lo, v6Bits, false), v6Addr(hi, v6Bits, true), cc, layer)
			}
			if res != Added {
				t.Fatalf("unexpected skip %v", res)
			}
			recs = append(recs, rec{lo, hi, cc, layer, i, version})
		}
		var buf bytes.Buffer
		if _, err := builder.Encode(&buf); err != nil {
			t.Fatal(err)
		}
		db, err := Open(buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		for version := 4; version <= 6; version += 2 {
			for slot := uint64(0); slot < 1<<14; slot++ {
				var best *rec
				for i := range recs {
					r := &recs[i]
					if r.version != version || slot < r.lo || slot > r.hi {
						continue
					}
					if best == nil || r.layer > best.layer ||
						(r.layer == best.layer && (r.hi-r.lo < best.hi-best.lo ||
							(r.hi-r.lo == best.hi-best.lo && r.seq > best.seq))) {
						best = r
					}
				}
				var addr netip.Addr
				if version == 4 {
					addr = v4Addr(slot, v4Bits, rng.Intn(2) == 0)
				} else {
					addr = v6Addr(slot, v6Bits, rng.Intn(2) == 0)
				}
				got, ok := db.Lookup(addr)
				if best == nil {
					if ok {
						t.Fatalf("seed %d v%d slot %d: got %s, want unknown", seed, version, slot, got)
					}
					continue
				}
				if !ok || got != best.cc {
					t.Fatalf("seed %d v%d slot %d: got %q ok=%v, want %s", seed, version, slot, got, ok, best.cc)
				}
			}
		}
	}
}

func TestDefaultsAndSkips(t *testing.T) {
	builder, err := NewBuilder(DefaultV4Bits, DefaultV6Bits)
	if err != nil {
		t.Fatal(err)
	}
	mustAdd := func(start, end, country string, layer int, want Result) {
		t.Helper()
		if got := builder.Add(netip.MustParseAddr(start), netip.MustParseAddr(end), country, layer); got != want {
			t.Fatalf("Add(%s-%s %s) = %v, want %v", start, end, country, got, want)
		}
	}
	mustAdd("5.0.0.0", "5.255.255.255", "DE", LayerAllocation, Added)
	mustAdd("5.1.2.0", "5.1.2.255", "FR", LayerGeofeed, Added)
	mustAdd("5.9.9.0", "5.9.9.127", "US", LayerGeofeed, SkippedGranularity)
	mustAdd("6.0.0.0", "6.0.0.255", "EU # x", LayerAllocation, SkippedCountry)
	mustAdd("2001:db8::", "2001:db8::ffff:ffff:ffff:ffff", "IR", LayerAllocation, SkippedGranularity)
	mustAdd("2a00::", "2a00:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "GB", LayerAllocation, Added)
	mustAdd("2a00:1::", "2a00:1:0:ffff:ffff:ffff:ffff:ffff", "NL", LayerGeofeed, Added)

	var buf bytes.Buffer
	if _, err := builder.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	db, err := Open(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]string{
		"5.1.1.1":         "DE",
		"5.1.2.200":       "FR",
		"5.9.9.1":         "DE",
		"6.0.0.1":         "",
		"1.1.1.1":         "",
		"255.255.255.255": "",
		"2a00:5::1":       "GB",
		"2a00:1:0:1::1":   "NL",
		"2001:db8::1":     "",
		"::ffff:5.1.2.3":  "FR",
	} {
		got, ok := db.Lookup(netip.MustParseAddr(addr))
		if got != want || ok != (want != "") {
			t.Errorf("Lookup(%s) = %q,%v want %q", addr, got, ok, want)
		}
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	if _, err := Open([]byte("nope")); err == nil {
		t.Fatal("expected error")
	}
	builder, _ := NewBuilder(DefaultV4Bits, DefaultV6Bits)
	builder.Add(netip.MustParseAddr("5.0.0.0"), netip.MustParseAddr("5.255.255.255"), "DE", LayerAllocation)
	var buf bytes.Buffer
	if _, err := builder.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut < buf.Len(); cut++ {
		// Truncated files must never panic; lookups may fail but not crash.
		if db, err := Open(buf.Bytes()[:cut]); err == nil {
			db.Lookup(netip.MustParseAddr("5.1.1.1"))
			db.Lookup(netip.MustParseAddr("2a00::1"))
		}
	}
}
