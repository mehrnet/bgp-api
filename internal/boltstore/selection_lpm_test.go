package boltstore

import (
	"net/netip"
	"testing"
)

func TestSelectionLPMChoosesCompactResponseRecord(t *testing.T) {
	builder := newSelectionLPMBuilder(4)
	insertSelectionRange(t, builder, 1, "0.0.0.0", "255.255.255.255")
	insertSelectionRange(t, builder, 2, "1.0.0.0", "1.255.255.255")
	insertSelectionRange(t, builder, 3, "1.1.0.0", "1.1.255.255")

	// Two equal-width overlapping records use the component with the greater
	// prefix length, matching the prior descending prefix-index traversal.
	insertSelectionRange(t, builder, 100, "4.0.0.1", "4.0.0.2")
	insertSelectionRange(t, builder, 1, "4.0.0.0", "4.0.0.1")

	// Records sharing a component prefer the narrower original source range,
	// then the lower stable source-record ID.
	insertSelectionRange(t, builder, 20, "3.0.0.0", "3.255.255.255")
	insertSelectionRange(t, builder, 5, "3.0.0.0", "3.255.255.255")
	insertSelectionRange(t, builder, 1, "3.0.0.0", "4.255.255.255")

	encoded, stats, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Nodes == 0 || stats.Bytes != uint64(len(encoded)) {
		t.Fatalf("unexpected selection LPM stats: %#v", stats)
	}
	for _, test := range []struct {
		address string
		want    uint64
	}{
		{"1.1.1.1", 3},
		{"1.2.1.1", 2},
		{"2.2.2.2", 1},
		{"3.1.1.1", 5},
		{"4.0.0.1", 100},
	} {
		got, err := LookupSelectionLPM(encoded, netip.MustParseAddr(test.address))
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("lookup %s = %d, want %d", test.address, got, test.want)
		}
	}
}

func TestSelectionLPMSupportsIPv6(t *testing.T) {
	builder := newSelectionLPMBuilder(6)
	insertSelectionRange(t, builder, 1, "::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")
	insertSelectionRange(t, builder, 2, "2001:db8::", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff")
	encoded, _, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LookupSelectionLPM(encoded, netip.MustParseAddr("2001:db8:1234::1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("lookup = %d, want 2", got)
	}
}

func TestSelectionLPMRejectsInvalidInputs(t *testing.T) {
	builder := newSelectionLPMBuilder(4)
	if err := builder.Insert(netip.MustParsePrefix("2001:db8::/32"), 1, Address{}, Address{}); err == nil {
		t.Fatal("accepted IPv6 prefix in IPv4 selector")
	}
	if err := builder.Insert(netip.MustParsePrefix("1.1.1.0/24"), 0, Address{}, Address{}); err == nil {
		t.Fatal("accepted zero selector ID")
	}
	if _, err := LookupSelectionLPM(nil, netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("accepted empty selector payload")
	}
}

func insertSelectionRange(t *testing.T, builder *selectionLPMBuilder, id uint64, startValue, endValue string) {
	t.Helper()
	start, end := AddressFromAddr(netip.MustParseAddr(startValue)), AddressFromAddr(netip.MustParseAddr(endValue))
	prefixes, err := rangePrefixes(start, end, int(builder.version))
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range prefixes {
		if err := builder.Insert(prefix, id, start, end); err != nil {
			t.Fatal(err)
		}
	}
}
