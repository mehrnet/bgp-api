package boltstore

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestRouteLPMResolvesMostSpecificIPv4AndIPv6Prefixes(t *testing.T) {
	for _, test := range []struct {
		name    string
		version uint8
		entries []struct {
			prefix string
			ids    []uint32
		}
		queries []struct {
			address string
			ids     []uint32
		}
	}{
		{
			name:    "IPv4",
			version: 4,
			entries: []struct {
				prefix string
				ids    []uint32
			}{
				{"0.0.0.0/0", []uint32{1}},
				{"1.0.0.0/8", []uint32{2}},
				{"1.1.0.0/16", []uint32{3}},
				{"1.1.1.0/24", []uint32{4, 5}},
			},
			queries: []struct {
				address string
				ids     []uint32
			}{
				{"1.1.1.9", []uint32{4, 5}},
				{"1.1.2.9", []uint32{3}},
				{"1.2.2.9", []uint32{2}},
				{"2.2.2.2", []uint32{1}},
			},
		},
		{
			name:    "IPv6",
			version: 6,
			entries: []struct {
				prefix string
				ids    []uint32
			}{
				{"::/0", []uint32{10}},
				{"2001:db8::/32", []uint32{11}},
				{"2001:db8:1234::/48", []uint32{12}},
			},
			queries: []struct {
				address string
				ids     []uint32
			}{
				{"2001:db8:1234::1", []uint32{12}},
				{"2001:db8:9876::1", []uint32{11}},
				{"2001:4860::1", []uint32{10}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			builder := newRouteLPMBuilder(test.version)
			for _, entry := range test.entries {
				if err := builder.Insert(netip.MustParsePrefix(entry.prefix), entry.ids); err != nil {
					t.Fatal(err)
				}
			}
			encoded, stats, err := builder.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if stats.Nodes < uint64(len(test.entries)) || stats.IDs == 0 || stats.Bytes != uint64(len(encoded)) {
				t.Fatalf("unexpected route LPM stats: %#v", stats)
			}
			for _, query := range test.queries {
				ids, err := LookupRouteLPM(encoded, netip.MustParseAddr(query.address))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(ids, query.ids) {
					t.Fatalf("lookup %s = %v, want %v", query.address, ids, query.ids)
				}
			}
		})
	}
}

func TestRouteLPMRejectsInvalidInputs(t *testing.T) {
	builder := newRouteLPMBuilder(4)
	if err := builder.Insert(netip.MustParsePrefix("2001:db8::/32"), []uint32{1}); err == nil {
		t.Fatal("accepted IPv6 prefix in IPv4 LPM")
	}
	if err := builder.Insert(netip.MustParsePrefix("1.1.1.0/24"), nil); err == nil {
		t.Fatal("accepted empty route ID list")
	}
	if _, err := LookupRouteLPM(nil, netip.MustParseAddr("1.1.1.1")); err == nil {
		t.Fatal("accepted empty LPM payload")
	}
}
