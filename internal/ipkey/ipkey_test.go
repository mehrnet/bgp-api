package ipkey

import "testing"

func TestParseIPv4UsesMappedSortKey(t *testing.T) {
	ip, ok := Parse("1.1.1.1")
	if !ok {
		t.Fatal("expected IPv4 to parse")
	}
	if ip.Canonical != "1.1.1.1" || ip.Version != 4 {
		t.Fatalf("unexpected parsed IP: %#v", ip)
	}
	if got := SortKeyToIP(ip.SortKey, 4); got != "1.1.1.1" {
		t.Fatalf("round trip = %q", got)
	}
	keys := PrefixKeysForIP(ip)
	if len(keys) != 33 || keys[0][len(keys[0])-3:] != "/32" {
		t.Fatalf("unexpected IPv4 prefix keys: %d %q", len(keys), keys[0])
	}
}

func TestParseIPv6AndRangePrefixes(t *testing.T) {
	ip, ok := Parse("2001:0db8::1")
	if !ok || ip.Canonical != "2001:db8::1" || ip.Version != 6 {
		t.Fatalf("unexpected parsed IPv6: %#v", ip)
	}
	prefixes, err := RangeToPrefixes("000000000000000000000000000000000000001", "000000000000000000000000000000000000002", 6)
	if err != nil || len(prefixes) != 2 {
		t.Fatalf("range prefixes = %#v, %v", prefixes, err)
	}
}

func TestParseRejectsInvalidIP(t *testing.T) {
	if _, ok := Parse("not-an-ip"); ok {
		t.Fatal("invalid IP parsed")
	}
}

func TestParsePrefixNormalizesAndCalculatesBounds(t *testing.T) {
	prefix, ok := ParsePrefix("1.1.1.42/24")
	if !ok {
		t.Fatal("expected IPv4 prefix to parse")
	}
	if prefix.Canonical != "1.1.1.0/24" || prefix.Start.Canonical != "1.1.1.0" || prefix.End.Canonical != "1.1.1.255" || prefix.AddressCount != "256" {
		t.Fatalf("unexpected prefix: %#v", prefix)
	}

	v6, ok := ParsePrefix("2001:db8::1/64")
	if !ok || v6.Canonical != "2001:db8::/64" || v6.End.Canonical != "2001:db8::ffff:ffff:ffff:ffff" || v6.AddressCount != "18446744073709551616" {
		t.Fatalf("unexpected IPv6 prefix: %#v", v6)
	}
}

func TestParseRangeCalculatesAddressCount(t *testing.T) {
	rangeValue, ok := ParseRange("1.1.1.10", "1.1.1.12")
	if !ok || rangeValue.Version != 4 || rangeValue.AddressCount != "3" {
		t.Fatalf("unexpected range: %#v", rangeValue)
	}
	if _, ok := ParseRange("1.1.1.12", "1.1.1.10"); ok {
		t.Fatal("descending range parsed")
	}
	if _, ok := ParseRange("1.1.1.1", "2001:db8::1"); ok {
		t.Fatal("mixed IP versions parsed")
	}
}

func TestSummaryPrefixKeysUseBoundedIPv4Buckets(t *testing.T) {
	for _, test := range []struct {
		start string
		end   string
		first string
		count int
	}{
		{"80.0.0.0", "80.255.255.255", "80.0.0.0/8", 1},
		{"80.0.0.0", "81.255.255.255", "80.0.0.0/8", 2},
		{"80.0.0.0", "80.31.255.255", "80.0.0.0/16", 32},
	} {
		rangeValue, ok := ParseRange(test.start, test.end)
		if !ok {
			t.Fatalf("parse range %s-%s", test.start, test.end)
		}
		keys, ok := SummaryPrefixKeys(rangeValue)
		if !ok || len(keys) != test.count || keys[0] != test.first {
			t.Fatalf("summary keys for %s-%s = %#v, %v", test.start, test.end, keys, ok)
		}
	}

	partial, ok := ParseRange("80.0.0.1", "80.255.255.255")
	if !ok {
		t.Fatal("parse partial range")
	}
	if _, ok := SummaryPrefixKeys(partial); ok {
		t.Fatal("non-CIDR range received a generated summary")
	}
}
