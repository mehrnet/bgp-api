package main

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
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
	input, err := os.Open("final_data/geofeeds.txt")
	if err != nil {
		panic(err)
	}
	defer input.Close()
	output, err := os.Create("final_data/geolocations.csv")
	if err != nil {
		panic(err)
	}
	defer output.Close()
	writer := csv.NewWriter(output)
	defer writer.Flush()

	client := &http.Client{Timeout: 10 * time.Second}
	semaphore := make(chan struct{}, 50)
	var mutex sync.Mutex
	var waitGroup sync.WaitGroup
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		url := strings.TrimSpace(scanner.Text())
		if url == "" || !strings.HasPrefix(url, "https://") {
			continue
		}
		waitGroup.Add(1)
		semaphore <- struct{}{}
		go func(url string) {
			defer waitGroup.Done()
			defer func() { <-semaphore }()
			response, err := client.Get(url)
			if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
				if response != nil {
					response.Body.Close()
				}
				return
			}
			defer response.Body.Close()
			feed := bufio.NewScanner(response.Body)
			feed.Buffer(make([]byte, 64*1024), 1024*1024)
			for feed.Scan() {
				parts := strings.Split(feed.Text(), ",")
				if len(parts) < 4 || strings.HasPrefix(parts[0], "#") {
					continue
				}
				start, end, err := cidrToRange(parts[0])
				if err != nil || start == "" {
					continue
				}
				version := "4"
				if strings.Contains(parts[0], ":") {
					version = "6"
				}
				mutex.Lock()
				_ = writer.Write([]string{start, end, version, strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])})
				mutex.Unlock()
			}
		}(url)
	}
	waitGroup.Wait()
}
