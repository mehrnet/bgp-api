// Package enrichment supplies optional network metadata that is not present in
// the daily RIR dataset. All calls are short-lived and cached; lookup callers
// never receive an upstream failure as an API error.
package enrichment

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/mehrnet/bgp-api/internal/api"
)

const (
	prefixTTL = 15 * time.Minute
	asnTTL    = 15 * time.Minute
	cacheSize = 4096
)

type Client struct {
	httpClient *http.Client

	mu      sync.Mutex
	prefix  map[string]prefixCacheEntry
	asn     map[uint32]asnCacheEntry
	flights singleflight.Group
}

type prefixCacheEntry struct {
	value   *api.PrefixEnrichment
	expires time.Time
}

type asnCacheEntry struct {
	value   *api.ASNEnrichment
	expires time.Time
}

func New() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 2500 * time.Millisecond},
		prefix:     make(map[string]prefixCacheEntry),
		asn:        make(map[uint32]asnCacheEntry),
	}
}

func (client *Client) Prefix(_ context.Context, prefix string) *api.PrefixEnrichment {
	if value, ok := client.cachedPrefix(prefix); ok {
		return value
	}
	value, _, _ := client.flights.Do("prefix:"+prefix, func() (any, error) {
		if cached, ok := client.cachedPrefix(prefix); ok {
			return cached, nil
		}
		result := client.fetchPrefix(prefix)
		if result == nil {
			return nil, nil
		}
		client.mu.Lock()
		client.trimPrefixCache()
		client.prefix[prefix] = prefixCacheEntry{value: result, expires: time.Now().Add(prefixTTL)}
		client.mu.Unlock()
		return result, nil
	})
	if value == nil {
		return nil
	}
	return value.(*api.PrefixEnrichment)
}

func (client *Client) ASN(_ context.Context, asn uint32) *api.ASNEnrichment {
	if value, ok := client.cachedASN(asn); ok {
		return value
	}
	value, _, _ := client.flights.Do("asn:"+strconv.FormatUint(uint64(asn), 10), func() (any, error) {
		if cached, ok := client.cachedASN(asn); ok {
			return cached, nil
		}
		result := client.fetchASN(asn)
		if result == nil {
			return nil, nil
		}
		client.mu.Lock()
		client.trimASNCache()
		client.asn[asn] = asnCacheEntry{value: result, expires: time.Now().Add(asnTTL)}
		client.mu.Unlock()
		return result, nil
	})
	if value == nil {
		return nil
	}
	return value.(*api.ASNEnrichment)
}

func (client *Client) trimPrefixCache() {
	if len(client.prefix) < cacheSize {
		return
	}
	for key, entry := range client.prefix {
		if time.Now().After(entry.expires) {
			delete(client.prefix, key)
		}
	}
	for key := range client.prefix {
		if len(client.prefix) < cacheSize {
			return
		}
		delete(client.prefix, key)
	}
}

func (client *Client) trimASNCache() {
	if len(client.asn) < cacheSize {
		return
	}
	for key, entry := range client.asn {
		if time.Now().After(entry.expires) {
			delete(client.asn, key)
		}
	}
	for key := range client.asn {
		if len(client.asn) < cacheSize {
			return
		}
		delete(client.asn, key)
	}
}

func (client *Client) cachedPrefix(prefix string) (*api.PrefixEnrichment, bool) {
	client.mu.Lock()
	defer client.mu.Unlock()
	entry, ok := client.prefix[prefix]
	if !ok || time.Now().After(entry.expires) {
		delete(client.prefix, prefix)
		return nil, false
	}
	return entry.value, true
}

func (client *Client) cachedASN(asn uint32) (*api.ASNEnrichment, bool) {
	client.mu.Lock()
	defer client.mu.Unlock()
	entry, ok := client.asn[asn]
	if !ok || time.Now().After(entry.expires) {
		delete(client.asn, asn)
		return nil, false
	}
	return entry.value, true
}

func (client *Client) fetchPrefix(prefix string) *api.PrefixEnrichment {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := &api.PrefixEnrichment{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		result.RDAP = client.rdap(ctx, prefixAddress(prefix))
	}()
	go func() {
		defer wg.Done()
		result.RoutingStatus = client.routingStatus(ctx, prefix)
	}()
	wg.Wait()
	if result.RDAP == nil && result.RoutingStatus == nil {
		return nil
	}
	return result
}

func (client *Client) fetchASN(asn uint32) *api.ASNEnrichment {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := &api.ASNEnrichment{RoutingStatus: client.asOverview(ctx, asn)}
	if result.RoutingStatus == nil {
		return nil
	}
	return result
}

func prefixAddress(prefix string) string {
	parsed, err := netip.ParsePrefix(prefix)
	if err != nil {
		return prefix
	}
	return parsed.Masked().Addr().String()
}

func (client *Client) rdap(ctx context.Context, address string) *api.RDAPNetwork {
	var response struct {
		Handle       string   `json:"handle"`
		Name         string   `json:"name"`
		Type         string   `json:"type"`
		StartAddress string   `json:"startAddress"`
		EndAddress   string   `json:"endAddress"`
		Country      string   `json:"country"`
		Status       []string `json:"status"`
		Events       []struct {
			Action string `json:"eventAction"`
			Date   string `json:"eventDate"`
		} `json:"events"`
	}
	if !client.getJSON(ctx, "https://rdap.org/ip/"+url.PathEscape(address), &response) {
		return nil
	}
	result := &api.RDAPNetwork{
		Handle: pointer(response.Handle), Name: pointer(response.Name), Type: pointer(response.Type),
		StartIP: pointer(response.StartAddress), EndIP: pointer(response.EndAddress),
		CountryCode: pointer(strings.ToUpper(response.Country)), Status: response.Status,
	}
	for _, event := range response.Events {
		switch strings.ToLower(event.Action) {
		case "registration":
			result.Created = pointer(event.Date)
		case "last changed":
			result.LastChanged = pointer(event.Date)
		}
	}
	return result
}

func (client *Client) routingStatus(ctx context.Context, prefix string) *api.RoutingStatus {
	var response struct {
		Data struct {
			FirstSeen struct {
				Time string `json:"time"`
			} `json:"first_seen"`
			LastSeen struct {
				Time string `json:"time"`
			} `json:"last_seen"`
			Origins []struct {
				Origin json.Number `json:"origin"`
			} `json:"origins"`
		} `json:"data"`
	}
	if !client.getJSON(ctx, "https://stat.ripe.net/data/routing-status/data.json?resource="+url.QueryEscape(prefix), &response) {
		return nil
	}
	origins := make([]string, 0, len(response.Data.Origins))
	for _, origin := range response.Data.Origins {
		if number := strings.TrimSpace(origin.Origin.String()); number != "" {
			origins = append(origins, "AS"+number)
		}
	}
	return &api.RoutingStatus{
		FirstSeen: pointer(response.Data.FirstSeen.Time), LastSeen: pointer(response.Data.LastSeen.Time), Origins: origins,
	}
}

func (client *Client) asOverview(ctx context.Context, asn uint32) *api.ASNRoutingStatus {
	var response struct {
		Data struct {
			Holder    string `json:"holder"`
			Announced *bool  `json:"announced"`
		} `json:"data"`
	}
	resource := "AS" + strconv.FormatUint(uint64(asn), 10)
	if !client.getJSON(ctx, "https://stat.ripe.net/data/as-overview/data.json?resource="+url.QueryEscape(resource), &response) {
		return nil
	}
	return &api.ASNRoutingStatus{Holder: pointer(response.Data.Holder), Announced: response.Data.Announced}
}

func (client *Client) getJSON(ctx context.Context, endpoint string, target any) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	request.Header.Set("Accept", "application/rdap+json, application/json")
	request.Header.Set("User-Agent", "MehrNet-BGP-API/1.0")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(target) == nil
}

func pointer(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
