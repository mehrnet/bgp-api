package ipkey

import (
	"fmt"
	"math/big"
	"net/netip"
	"strings"
)

const sortKeyWidth = 39

var (
	ipv4Mask   = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 32), big.NewInt(1))
	ipv4Mapped = new(big.Int).Lsh(big.NewInt(0xffff), 32)
)

type Parsed struct {
	Canonical string
	Version   int
	SortKey   string
}

type RangePrefix struct {
	Key    string
	Length int
}

func pad(value *big.Int) string {
	return fmt.Sprintf("%0*s", sortKeyWidth, value.String())
}

func prefixKey(version int, value *big.Int, length int) string {
	return fmt.Sprintf("%d:%s/%d", version, pad(value), length)
}

func Parse(input string) (Parsed, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(input))
	if err != nil || addr.Zone() != "" {
		return Parsed{}, false
	}
	if addr.Is4() {
		bytes := addr.As4()
		value := new(big.Int).SetBytes(bytes[:])
		value.Or(value, ipv4Mapped)
		return Parsed{Canonical: addr.String(), Version: 4, SortKey: pad(value)}, true
	}
	if !addr.Is6() {
		return Parsed{}, false
	}
	bytes := addr.As16()
	value := new(big.Int).SetBytes(bytes[:])
	return Parsed{Canonical: addr.String(), Version: 6, SortKey: pad(value)}, true
}

func SortKeyToIP(sortKey string, version int) string {
	value, ok := new(big.Int).SetString(sortKey, 10)
	if !ok {
		return ""
	}
	if version == 4 {
		value.And(value, ipv4Mask)
		bytes := value.FillBytes(make([]byte, 4))
		return netip.AddrFrom4([4]byte(bytes)).String()
	}
	bytes := value.FillBytes(make([]byte, 16))
	return netip.AddrFrom16([16]byte(bytes)).String()
}

func PrefixKeysForIP(ip Parsed) []string {
	bits := 128
	if ip.Version == 4 {
		bits = 32
	}
	value, _ := new(big.Int).SetString(ip.SortKey, 10)
	addressMask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits)), big.NewInt(1))
	value.And(value, addressMask)
	mapped := big.NewInt(0)
	if ip.Version == 4 {
		mapped.Set(ipv4Mapped)
	}

	keys := make([]string, 0, bits+1)
	for length := bits; length >= 0; length-- {
		hostBits := bits - length
		network := new(big.Int).Set(value)
		if hostBits > 0 {
			mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(hostBits)), big.NewInt(1))
			network.AndNot(network, mask)
		}
		full := new(big.Int).Or(network, mapped)
		keys = append(keys, prefixKey(ip.Version, full, length))
	}
	return keys
}

func RangeToPrefixes(startSortKey, endSortKey string, version int) ([]RangePrefix, error) {
	bits := 128
	if version == 4 {
		bits = 32
	}
	start, ok := new(big.Int).SetString(startSortKey, 10)
	if !ok {
		return nil, fmt.Errorf("invalid range start")
	}
	end, ok := new(big.Int).SetString(endSortKey, 10)
	if !ok {
		return nil, fmt.Errorf("invalid range end")
	}
	addressMask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits)), big.NewInt(1))
	start.And(start, addressMask)
	end.And(end, addressMask)
	if start.Cmp(end) > 0 {
		return nil, fmt.Errorf("range start is greater than end")
	}
	mapped := big.NewInt(0)
	if version == 4 {
		mapped.Set(ipv4Mapped)
	}

	current := new(big.Int).Set(start)
	result := make([]RangePrefix, 0)
	for current.Cmp(end) <= 0 {
		blockSize := new(big.Int)
		if current.Sign() == 0 {
			blockSize.Lsh(big.NewInt(1), uint(bits))
		} else {
			negative := new(big.Int).Neg(current)
			blockSize.And(current, negative)
		}
		remaining := new(big.Int).Sub(end, current)
		remaining.Add(remaining, big.NewInt(1))
		for blockSize.Cmp(remaining) > 0 {
			blockSize.Rsh(blockSize, 1)
		}
		length := bits - (blockSize.BitLen() - 1)
		full := new(big.Int).Or(current, mapped)
		result = append(result, RangePrefix{Key: prefixKey(version, full, length), Length: length})
		current.Add(current, blockSize)
	}
	return result, nil
}
