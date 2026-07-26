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
