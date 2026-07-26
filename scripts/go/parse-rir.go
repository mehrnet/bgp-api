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
	geoFile, err := os.Create(fmt.Sprintf("geofeeds_%s.txt", rirLower))
	if err != nil {
		log.Fatal(err)
	}
	defer geoFile.Close()

	allocations := csv.NewWriter(allocationFile)
	routes := csv.NewWriter(routeFile)
	defer allocations.Flush()
	defer routes.Flush()

	files, err := os.ReadDir("raw_data")
	if err != nil {
		log.Fatal(err)
	}
	for _, file := range files {
		path := filepath.Join("raw_data", file.Name())
		if strings.Contains(file.Name(), "delegated-arin") {
			parseDelegated(path, rirUpper, allocations)
		} else {
			parseRPSL(path, rirUpper, allocations, routes, geoFile)
		}
	}
}

func parseRPSL(path, registry string, allocations, routes *csv.Writer, geoFile *os.File) {
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
				processRPSLBlock(block, registry, allocations, routes, geoFile)
				block = block[:0]
			}
			continue
		}
		block = append(block, line)
	}
	if len(block) > 0 {
		processRPSLBlock(block, registry, allocations, routes, geoFile)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("scan %s: %v", path, err)
	}
}

func processRPSLBlock(lines []string, registry string, allocations, routes *csv.Writer, geoFile *os.File) {
	var start, end, country, netname, cidr, asn string
	version := 0
	isAllocation, isRoute := false, false

	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "geofeed:") || strings.Contains(lower, "remarks: geofeed") {
			if index := strings.Index(line, "https://"); index >= 0 {
				_, _ = geoFile.WriteString(strings.TrimSpace(line[index:]) + "\n")
			}
		}

		switch {
		case strings.HasPrefix(lower, "inetnum:"):
			isAllocation, version = true, 4
			parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(lower, "inetnum:")), "-")
			if len(parts) == 2 {
				start, end = padIP(parts[0]), padIP(parts[1])
			}
		case strings.HasPrefix(lower, "inet6num:"):
			isAllocation, version = true, 6
			start, end, _ = cidrToRange(strings.TrimSpace(strings.TrimPrefix(lower, "inet6num:")))
		case strings.HasPrefix(lower, "route:"):
			isRoute, version = true, 4
			cidr = strings.TrimSpace(strings.TrimPrefix(lower, "route:"))
			start, end, _ = cidrToRange(cidr)
		case strings.HasPrefix(lower, "route6:"):
			isRoute, version = true, 6
			cidr = strings.TrimSpace(strings.TrimPrefix(lower, "route6:"))
			start, end, _ = cidrToRange(cidr)
		case strings.HasPrefix(lower, "origin:"):
			asn = strings.TrimSpace(strings.TrimPrefix(lower, "origin:"))
		case strings.HasPrefix(lower, "country:"):
			country = strings.TrimSpace(strings.TrimPrefix(lower, "country:"))
		case strings.HasPrefix(lower, "netname:"):
			netname = strings.TrimSpace(strings.TrimPrefix(lower, "netname:"))
		}
	}

	if start == "" || end == "" {
		return
	}
	if isAllocation {
		_ = allocations.Write([]string{start, end, fmt.Sprintf("%d", version), registry, strings.ToUpper(country), netname})
	} else if isRoute && asn != "" {
		_ = routes.Write([]string{start, end, fmt.Sprintf("%d", version), cidr, strings.ToUpper(asn)})
	}
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
			_ = allocations.Write([]string{start, end, fmt.Sprintf("%d", version), registry, strings.ToUpper(country), ""})
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("scan %s: %v", path, err)
	}
}
