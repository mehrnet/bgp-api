package main

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func padIP(ipStr string) string {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return ""
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return ""
	}
	value := new(big.Int).SetBytes(ip16)
	return fmt.Sprintf("%039s", value.String())
}

func cidrToRange(cidr string) (string, string, error) {
	_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return "", "", err
	}

	ip, mask := network.IP, network.Mask
	if ipv4 := ip.To4(); ipv4 != nil {
		ip = ipv4
	} else {
		ip = ip.To16()
	}
	if ip == nil || len(ip) != len(mask) {
		return "", "", fmt.Errorf("network mask mismatch")
	}

	end := make(net.IP, len(ip))
	for index := range ip {
		end[index] = ip[index] | ^mask[index]
	}
	return padIP(ip.String()), padIP(end.String()), nil
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: go run scripts/go/parse-rir.go <rir>")
	}

	rirLower := strings.ToLower(os.Args[1])
	rirUpper := strings.ToUpper(os.Args[1])
	allocationFile, err := os.Create(fmt.Sprintf("allocations_%s.csv", rirLower))
	if err != nil {
		log.Fatal(err)
	}
	defer allocationFile.Close()
	routeFile, err := os.Create(fmt.Sprintf("routes_%s.csv", rirLower))
	if err != nil {
		log.Fatal(err)
	}
	defer routeFile.Close()
	autnumFile, err := os.Create(fmt.Sprintf("autnums_%s.csv", rirLower))
	if err != nil {
		log.Fatal(err)
	}
	defer autnumFile.Close()
	geoFile, err := os.Create(fmt.Sprintf("geofeeds_%s.txt", rirLower))
	if err != nil {
		log.Fatal(err)
	}
	defer geoFile.Close()

	allocations := csv.NewWriter(allocationFile)
	routes := csv.NewWriter(routeFile)
	autnums := csv.NewWriter(autnumFile)
	defer allocations.Flush()
	defer routes.Flush()
	defer autnums.Flush()

	files, err := os.ReadDir("raw_data")
	if err != nil {
		log.Fatal(err)
	}
	for _, file := range files {
		path := filepath.Join("raw_data", file.Name())
		if strings.Contains(file.Name(), "delegated-arin") {
			parseDelegated(path, rirUpper, allocations)
		} else {
			parseRPSL(path, rirUpper, allocations, routes, autnums, geoFile)
		}
	}
}

func parseRPSL(path, registry string, allocations, routes, autnums *csv.Writer, geoFile *os.File) {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("open %s: %v", path, err)
		return
	}
	defer file.Close()

	var reader io.Reader = file
	if strings.HasSuffix(path, ".gz") {
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			log.Printf("gzip %s: %v", path, err)
			return
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var block []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(block) > 0 {
				processRPSLBlock(block, registry, allocations, routes, autnums, geoFile)
				block = block[:0]
			}
			continue
		}
		block = append(block, line)
	}
	if len(block) > 0 {
		processRPSLBlock(block, registry, allocations, routes, autnums, geoFile)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("scan %s: %v", path, err)
	}
}

func processRPSLBlock(lines []string, registry string, allocations, routes, autnums *csv.Writer, geoFile *os.File) {
	attributes := make(map[string][]string)
	for _, line := range lines {
		key, value, ok := rpslAttribute(line)
		if !ok {
			continue
		}
		if key == "geofeed" || (key == "remarks" && strings.Contains(strings.ToLower(value), "geofeed")) {
			if index := strings.Index(value, "https://"); index >= 0 {
				_, _ = geoFile.WriteString(strings.TrimSpace(value[index:]) + "\n")
			}
		}
		attributes[key] = append(attributes[key], value)
	}

	first := func(key string) string { return firstAttribute(attributes[key]) }
	metadata := rpslMetadata{
		country:      strings.ToUpper(first("country")),
		status:       first("status"),
		created:      first("created"),
		lastModified: first("last-modified"),
		source:       sourceName(first("source")),
		mntBy:        joinAttributes(attributes["mnt-by"]),
		org:          first("org"),
		abuseContact: firstNonEmpty(joinAttributes(attributes["abuse-c"]), joinAttributes(attributes["abuse-mailbox"])),
		description:  joinAttributes(attributes["descr"]),
	}

	if inetnum := first("inetnum"); inetnum != "" {
		parts := strings.Split(inetnum, "-")
		if len(parts) == 2 {
			netname := first("netname")
			if !isGlobalIANAIPv4Placeholder(parts[0], parts[1], netname) {
				writeAllocation(allocations, padIP(parts[0]), padIP(parts[1]), 4, registry, metadata, netname, "")
			}
		}
		return
	}
	if inet6num := first("inet6num"); inet6num != "" {
		start, end, err := cidrToRange(inet6num)
		if err == nil {
			writeAllocation(allocations, start, end, 6, registry, metadata, first("netname"), "")
		}
		return
	}
	if cidr := first("route"); cidr != "" {
		writeRoute(routes, cidr, 4, first("origin"), registry, metadata)
		return
	}
	if cidr := first("route6"); cidr != "" {
		writeRoute(routes, cidr, 6, first("origin"), registry, metadata)
		return
	}
	if asn := first("aut-num"); asn != "" {
		_ = autnums.Write([]string{
			strings.ToUpper(asn), registry, metadata.country, first("as-name"), metadata.org,
			metadata.status, metadata.created, metadata.lastModified, metadata.source, metadata.mntBy, metadata.abuseContact, metadata.description,
		})
	}
}

// RIR databases repeat IANA's full IPv4 space as administrative metadata.
// It is not a usable allocation record for an address or range lookup.
func isGlobalIANAIPv4Placeholder(start, end, netname string) bool {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(netname)), "iana-") {
		return false
	}
	startIP := net.ParseIP(strings.TrimSpace(start)).To4()
	endIP := net.ParseIP(strings.TrimSpace(end)).To4()
	return startIP != nil && endIP != nil && startIP.Equal(net.IPv4zero) && endIP.Equal(net.IPv4(255, 255, 255, 255))
}

type rpslMetadata struct {
	country, status, created, lastModified, source, mntBy, org, abuseContact, description string
}

func rpslAttribute(line string) (string, string, bool) {
	if len(line) == 0 || line[0] == '#' || line[0] == '%' || line[0] == ' ' || line[0] == '\t' {
		return "", "", false
	}
	key, value, found := strings.Cut(line, ":")
	if !found {
		return "", "", false
	}
	key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
	if key == "" || value == "" {
		return "", "", false
	}
	return key, value, true
}

func firstAttribute(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func joinAttributes(values []string) string {
	unique := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, found := unique[value]; found {
			continue
		}
		unique[value] = struct{}{}
		result = append(result, value)
	}
	return strings.Join(result, " | ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func sourceName(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func writeAllocation(writer *csv.Writer, start, end string, version int, registry string, metadata rpslMetadata, netname, allocationDate string) {
	if start == "" || end == "" {
		return
	}
	_ = writer.Write([]string{
		start, end, fmt.Sprintf("%d", version), registry, metadata.country, netname, metadata.status, allocationDate,
		metadata.created, metadata.lastModified, metadata.source, metadata.mntBy, metadata.org, metadata.abuseContact, metadata.description,
	})
}

func writeRoute(writer *csv.Writer, cidr string, version int, asn, registry string, metadata rpslMetadata) {
	if asn == "" {
		return
	}
	start, end, err := cidrToRange(cidr)
	if err != nil || start == "" || end == "" {
		return
	}
	_ = writer.Write([]string{
		start, end, fmt.Sprintf("%d", version), cidr, strings.ToUpper(asn), registry,
		metadata.source, metadata.mntBy, metadata.org, metadata.abuseContact, metadata.description,
	})
}

func parseDelegated(path, registry string, allocations *csv.Writer) {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("open %s: %v", path, err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 7 || (parts[6] != "allocated" && parts[6] != "assigned") {
			continue
		}

		country, ipType, startString, valueString := parts[1], parts[2], parts[3], parts[4]
		version := 0
		var start, end string
		switch ipType {
		case "ipv4":
			startIP := net.ParseIP(startString).To4()
			count, err := strconv.ParseUint(valueString, 10, 32)
			if startIP == nil || err != nil || count == 0 {
				continue
			}
			startNumber := binary.BigEndian.Uint32(startIP)
			endNumber := startNumber + uint32(count) - 1
			endIP := make(net.IP, 4)
			binary.BigEndian.PutUint32(endIP, endNumber)
			start, end, version = padIP(startString), padIP(endIP.String()), 4
		case "ipv6":
			start, end, _ = cidrToRange(startString + "/" + valueString)
			version = 6
		}
		if start != "" && end != "" {
			writeAllocation(allocations, start, end, version, registry, rpslMetadata{
				country: strings.ToUpper(country),
				status:  parts[6],
			}, "", parts[5])
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("scan %s: %v", path, err)
	}
}
