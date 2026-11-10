// build-country-db turns the producer CSVs into a tiny IP -> country code
// database (MNCC format, see internal/countrydb) for bundling into clients.
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/mehrnet/bgp-api/internal/countrydb"
)

// addrFromSortKey decodes the producers' zero-padded decimal 128-bit keys.
func addrFromSortKey(value, version string) (netip.Addr, bool) {
	number, ok := new(big.Int).SetString(strings.TrimSpace(value), 10)
	if !ok || number.Sign() < 0 || number.BitLen() > 128 {
		return netip.Addr{}, false
	}
	var raw [16]byte
	number.FillBytes(raw[:])
	switch strings.TrimSpace(version) {
	case "4":
		return netip.AddrFrom4([4]byte(raw[12:])), true
	case "6":
		return netip.AddrFrom16(raw), true
	}
	return netip.Addr{}, false
}

type counts struct{ added, country, granularity, address, malformed int }

func load(path string, countryColumn, layer int, builder *countrydb.Builder) (counts, error) {
	var c counts
	file, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer file.Close()
	reader := csv.NewReader(bufio.NewReaderSize(file, 1<<20))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = true
	for {
		row, err := reader.Read()
		if err == io.EOF {
			return c, nil
		}
		if err != nil {
			return c, fmt.Errorf("read %s: %w", path, err)
		}
		if len(row) <= countryColumn {
			c.malformed++
			continue
		}
		start, ok1 := addrFromSortKey(row[0], row[2])
		end, ok2 := addrFromSortKey(row[1], row[2])
		if !ok1 || !ok2 {
			c.malformed++
			continue
		}
		switch builder.Add(start, end, strings.ToUpper(strings.TrimSpace(row[countryColumn])), layer) {
		case countrydb.Added:
			c.added++
		case countrydb.SkippedCountry:
			c.country++
		case countrydb.SkippedGranularity:
			c.granularity++
		case countrydb.SkippedAddress:
			c.address++
		}
	}
}

func main() {
	input := flag.String("input", "final_data", "directory with allocations.csv and geolocations.csv")
	output := flag.String("output", "country.mncc", "output database path")
	v4Bits := flag.Int("v4-prefix", countrydb.DefaultV4Bits, "IPv4 granularity as a prefix length")
	v6Bits := flag.Int("v6-prefix", countrydb.DefaultV6Bits, "IPv6 granularity as a prefix length")
	geofeeds := flag.Bool("geofeeds", true, "let geofeed countries override RIR allocation countries")
	flag.Parse()

	builder, err := countrydb.NewBuilder(*v4Bits, *v6Bits)
	if err != nil {
		log.Fatal(err)
	}
	sources := []struct {
		name   string
		column int
		layer  int
		skip   bool
	}{
		{"allocations.csv", 4, countrydb.LayerAllocation, false},
		{"geolocations.csv", 3, countrydb.LayerGeofeed, !*geofeeds},
	}
	for _, source := range sources {
		if source.skip {
			continue
		}
		c, err := load(filepath.Join(*input, source.name), source.column, source.layer, builder)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: used=%d no_iso_country=%d finer_than_granularity=%d bad_address=%d malformed=%d\n",
			source.name, c.added, c.country, c.granularity, c.address, c.malformed)
	}

	temporary := *output + ".tmp"
	file, err := os.Create(temporary)
	if err != nil {
		log.Fatal(err)
	}
	stats, err := builder.Encode(file)
	if err == nil {
		err = file.Close()
	} else {
		file.Close()
	}
	if err != nil {
		os.Remove(temporary)
		log.Fatal(err)
	}
	if err := os.Rename(temporary, *output); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s: %d bytes, ipv4_ranges=%d ipv6_ranges=%d countries=%d\n",
		*output, stats.Bytes, stats.V4Entries, stats.V6Entries, stats.Countries)
}
