package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type headerValues []string

func (values *headerValues) String() string { return strings.Join(*values, ",") }

func (values *headerValues) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	var target, connectIP string
	var requests, concurrency, warmup int
	var headers headerValues
	flag.StringVar(&target, "url", "", "absolute HTTP(S) endpoint")
	flag.StringVar(&connectIP, "connect-ip", "", "override the target hostname's TCP destination while preserving the URL host and HTTPS SNI")
	flag.IntVar(&requests, "requests", 5000, "measured request count")
	flag.IntVar(&concurrency, "concurrency", 32, "parallel request count")
	flag.IntVar(&warmup, "warmup", 200, "unmeasured warmup requests")
	flag.Var(&headers, "header", "request header in Name: Value form; repeatable")
	flag.Parse()
	if target == "" || requests < 1 || concurrency < 1 || warmup < 0 {
		fmt.Fprintln(os.Stderr, "url is required; requests and concurrency must be positive; warmup cannot be negative")
		os.Exit(2)
	}
	targetURL, err := url.ParseRequestURI(target)
	if err != nil || (targetURL.Scheme != "http" && targetURL.Scheme != "https") || targetURL.Hostname() == "" {
		fmt.Fprintln(os.Stderr, "url must be an absolute HTTP or HTTPS endpoint")
		os.Exit(2)
	}
	if connectIP != "" && net.ParseIP(connectIP) == nil {
		fmt.Fprintln(os.Stderr, "connect-ip must be a valid IPv4 or IPv6 address")
		os.Exit(2)
	}

	requestHeaders, err := parseHeaders(headers)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = concurrency * 2
	transport.MaxIdleConnsPerHost = concurrency * 2
	transport.MaxConnsPerHost = concurrency * 2
	if connectIP != "" {
		configureDialOverride(transport, targetURL, connectIP)
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	for index := 0; index < warmup; index++ {
		if err := request(client, target, requestHeaders); err != nil {
			fmt.Fprintf(os.Stderr, "warmup request %d: %v\n", index+1, err)
			os.Exit(1)
		}
	}

	durations := make([]time.Duration, requests)
	var next, failures uint64
	started := time.Now()
	var group sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(atomic.AddUint64(&next, 1) - 1)
				if index >= requests {
					return
				}
				began := time.Now()
				if err := request(client, target, requestHeaders); err != nil {
					atomic.AddUint64(&failures, 1)
					continue
				}
				durations[index] = time.Since(began)
			}
		}()
	}
	group.Wait()
	elapsed := time.Since(started)
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "benchmark failed: %d of %d requests did not return HTTP 200\n", failures, requests)
		os.Exit(1)
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	var total time.Duration
	for _, duration := range durations {
		total += duration
	}
	fmt.Printf("target: %s\n", target)
	if connectIP != "" {
		fmt.Printf("connect_ip: %s (TLS SNI and Host remain %s)\n", connectIP, targetURL.Hostname())
	}
	fmt.Printf("requests: %d  concurrency: %d  keep_alive: true  warmup: %d\n", requests, concurrency, warmup)
	fmt.Printf("throughput: %.1f req/s\n", float64(requests)/elapsed.Seconds())
	fmt.Printf("latency_ms: min=%.3f avg=%.3f p50=%.3f p95=%.3f p99=%.3f max=%.3f\n",
		milliseconds(durations[0]),
		float64(total)/float64(requests)/float64(time.Millisecond),
		milliseconds(percentile(durations, 50)),
		milliseconds(percentile(durations, 95)),
		milliseconds(percentile(durations, 99)),
		milliseconds(durations[len(durations)-1]),
	)
}

func configureDialOverride(transport *http.Transport, target *url.URL, connectIP string) {
	targetHost := target.Hostname()
	targetPort := target.Port()
	if targetPort == "" {
		if target.Scheme == "https" {
			targetPort = "443"
		} else {
			targetPort = "80"
		}
	}
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err == nil && strings.EqualFold(host, targetHost) && port == targetPort {
			address = net.JoinHostPort(connectIP, port)
		}
		return dialer.DialContext(ctx, network, address)
	}
}

func parseHeaders(values []string) (http.Header, error) {
	result := make(http.Header, len(values))
	for _, value := range values {
		name, content, ok := strings.Cut(value, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid header %q; use Name: Value", value)
		}
		result.Add(strings.TrimSpace(name), strings.TrimSpace(content))
	}
	return result, nil
}

func request(client *http.Client, target string, headers http.Header) error {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header = headers.Clone()
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	return nil
}

func percentile(values []time.Duration, percent int) time.Duration {
	index := int(math.Ceil(float64(percent)/100*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}

func milliseconds(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }
