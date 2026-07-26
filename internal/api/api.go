package api

import (
	"context"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/mehrnet/bgp-api/internal/ipkey"
)

type Repository interface {
	Lookup(context.Context, ipkey.Parsed) (*LookupResponse, error)
}

type Config struct {
	AllowedOrigins  map[string]struct{}
	OriginAuthToken string
	DatabaseEngine  string
	TrustedProxies  []netip.Prefix
}

type nullableString = *string

type LookupResponse struct {
	IP               string         `json:"ip"`
	Version          int            `json:"version"`
	Registry         nullableString `json:"registry"`
	AllocationDate   nullableString `json:"allocation_date"`
	AllocationStatus nullableString `json:"allocation_status"`
	Network          struct {
		CIDR     nullableString `json:"cidr"`
		StartIP  nullableString `json:"start_ip"`
		EndIP    nullableString `json:"end_ip"`
		ASN      nullableString `json:"asn"`
		ASNs     []string       `json:"asns"`
		ASNumber *int           `json:"as_number"`
		Name     nullableString `json:"name"`
		Status   nullableString `json:"status"`
	} `json:"network"`
	Allocation struct {
		StartIP        nullableString `json:"start_ip"`
		EndIP          nullableString `json:"end_ip"`
		Registry       nullableString `json:"registry"`
		CountryCode    nullableString `json:"country_code"`
		CountryRaw     nullableString `json:"country_raw"`
		Name           nullableString `json:"name"`
		AllocationDate nullableString `json:"allocation_date"`
		Status         nullableString `json:"status"`
	} `json:"allocation"`
	Location struct {
		CountryCode nullableString `json:"country_code"`
		Region      nullableString `json:"region"`
		City        nullableString `json:"city"`
	} `json:"location"`
	Sources struct {
		Allocation bool `json:"allocation"`
		Route      bool `json:"route"`
		Geofeed    bool `json:"geofeed"`
	} `json:"sources"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func New(repository Repository, config Config) http.Handler {
	if config.DatabaseEngine == "" {
		config.DatabaseEngine = "postgresql"
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if config.OriginAuthToken != "" && request.Header.Get("X-BGP-API-Origin-Token") != config.OriginAuthToken {
			writeError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "origin authorization required")
			return
		}
		origin := request.Header.Get("Origin")
		_, allowedOrigin := config.AllowedOrigins[origin]
		if request.Method == http.MethodOptions {
			if allowedOrigin {
				writer.Header().Set("Access-Control-Allow-Origin", origin)
			}
			writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			writer.Header().Set("Access-Control-Max-Age", "86400")
			writer.Header().Set("Vary", "Origin")
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		setCORS(writer, origin, allowedOrigin)

		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/":
			writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "service": "bgp-api", "version": 1})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/health":
			writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "service": "bgp-api", "version": 1, "database": config.DatabaseEngine})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/me":
			lookupIP(writer, request, repository, clientIP(request, config.TrustedProxies))
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/ip/"):
			lookupIP(writer, request, repository, strings.TrimPrefix(request.URL.Path, "/v1/ip/"))
		default:
			writeError(writer, http.StatusNotFound, "NOT_FOUND", "route not found")
		}
	})
}

func lookupIP(writer http.ResponseWriter, request *http.Request, repository Repository, input string) {
	ip, ok := ipkey.Parse(input)
	if !ok {
		writeError(writer, http.StatusBadRequest, "INVALID_IP", "path parameter must be a valid IPv4 or IPv6 address")
		return
	}
	response, err := repository.Lookup(request.Context(), ip)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected lookup failure")
		return
	}
	if response == nil {
		writeError(writer, http.StatusNotFound, "IP_NOT_FOUND", "no RIR allocation, route, or geofeed record matched this IP")
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func clientIP(request *http.Request, trustedProxies []netip.Prefix) string {
	peer, ok := addressFromRemote(request.RemoteAddr)
	if !ok {
		return ""
	}
	if !isTrustedProxy(peer, trustedProxies) {
		return peer.String()
	}
	for _, header := range []string{"X-BGP-API-Cloudflare-IP", "X-BGP-API-Forwarded-IP"} {
		if candidate, ok := addressFromHeader(request.Header.Get(header)); ok {
			return candidate.String()
		}
	}
	return peer.String()
}

func addressFromRemote(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return addressFromHeader(host)
}

func addressFromHeader(value string) (netip.Addr, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func isTrustedProxy(address netip.Addr, trustedProxies []netip.Prefix) bool {
	for _, prefix := range trustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func setCORS(writer http.ResponseWriter, origin string, allowed bool) {
	writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	writer.Header().Set("Vary", "Origin")
	if allowed {
		writer.Header().Set("Access-Control-Allow-Origin", origin)
	}
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	response := errorResponse{}
	response.Error.Code = code
	response.Error.Message = message
	writeJSON(writer, status, response)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

type LookupCandidate struct {
	StartIPSort string
	EndIPSort   string
	IPVersion   int
	Registry    nullableString
	Country     nullableString
	Netname     nullableString
	CIDR        nullableString
	ASN         nullableString
	Region      nullableString
	City        nullableString
}

func BuildResponse(ip ipkey.Parsed, allocations, routes, geolocations []LookupCandidate) *LookupResponse {
	allocation := narrowest(allocations)
	bestRoutes := allNarrowest(routes)
	geolocation := narrowest(geolocations)
	if allocation == nil && len(bestRoutes) == 0 && geolocation == nil {
		return nil
	}

	response := &LookupResponse{IP: ip.Canonical, Version: ip.Version}
	response.Registry = lower(nullable(allocation, func(row LookupCandidate) nullableString { return row.Registry }))
	response.Network.ASNs = make([]string, 0)
	for _, route := range bestRoutes {
		if asn := upper(nullable(&route, func(row LookupCandidate) nullableString { return row.ASN })); asn != nil && !contains(response.Network.ASNs, *asn) {
			response.Network.ASNs = append(response.Network.ASNs, *asn)
		}
	}
	sortASNs(response.Network.ASNs)
	if len(response.Network.ASNs) == 1 {
		response.Network.ASN = &response.Network.ASNs[0]
		response.Network.ASNumber = asNumber(response.Network.ASN)
	}
	if len(bestRoutes) > 0 {
		route := bestRoutes[0]
		response.Network.CIDR = nullable(&route, func(row LookupCandidate) nullableString { return row.CIDR })
		start := ipkey.SortKeyToIP(route.StartIPSort, ip.Version)
		end := ipkey.SortKeyToIP(route.EndIPSort, ip.Version)
		response.Network.StartIP, response.Network.EndIP = &start, &end
	}
	response.Network.Name = nullable(allocation, func(row LookupCandidate) nullableString { return row.Netname })

	if allocation != nil {
		start := ipkey.SortKeyToIP(allocation.StartIPSort, ip.Version)
		end := ipkey.SortKeyToIP(allocation.EndIPSort, ip.Version)
		response.Allocation.StartIP, response.Allocation.EndIP = &start, &end
		response.Allocation.Registry = lower(nullable(allocation, func(row LookupCandidate) nullableString { return row.Registry }))
		response.Allocation.CountryRaw = upper(nullable(allocation, func(row LookupCandidate) nullableString { return row.Country }))
		response.Allocation.CountryCode = countryCode(response.Allocation.CountryRaw)
		response.Allocation.Name = nullable(allocation, func(row LookupCandidate) nullableString { return row.Netname })
	}
	if geolocation != nil {
		response.Location.CountryCode = countryCode(upper(nullable(geolocation, func(row LookupCandidate) nullableString { return row.Country })))
		response.Location.Region = nullable(geolocation, func(row LookupCandidate) nullableString { return row.Region })
		response.Location.City = nullable(geolocation, func(row LookupCandidate) nullableString { return row.City })
	}
	if response.Location.CountryCode == nil {
		response.Location.CountryCode = response.Allocation.CountryCode
	}
	response.Sources.Allocation = allocation != nil
	response.Sources.Route = len(bestRoutes) > 0
	response.Sources.Geofeed = geolocation != nil
	return response
}

func nullable(candidate *LookupCandidate, selectValue func(LookupCandidate) nullableString) nullableString {
	if candidate == nil {
		return nil
	}
	value := selectValue(*candidate)
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func lower(value nullableString) nullableString {
	if value == nil {
		return nil
	}
	lowercase := strings.ToLower(*value)
	return &lowercase
}

func upper(value nullableString) nullableString {
	if value == nil {
		return nil
	}
	uppercase := strings.ToUpper(*value)
	return &uppercase
}

func countryCode(value nullableString) nullableString {
	if value == nil || len(*value) != 2 {
		return nil
	}
	for _, character := range *value {
		if character < 'A' || character > 'Z' {
			return nil
		}
	}
	return value
}

func asNumber(asn nullableString) *int {
	if asn == nil || !strings.HasPrefix(*asn, "AS") {
		return nil
	}
	number, err := strconv.Atoi(strings.TrimPrefix(*asn, "AS"))
	if err != nil || number <= 0 {
		return nil
	}
	return &number
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func sortASNs(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && asnOrder(values[cursor]) < asnOrder(values[cursor-1]); cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

func asnOrder(value string) int {
	number, err := strconv.Atoi(strings.TrimPrefix(value, "AS"))
	if err != nil {
		return 0
	}
	return number
}

func narrowest(candidates []LookupCandidate) *LookupCandidate {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if rangeWidth(candidate).Cmp(rangeWidth(best)) < 0 {
			best = candidate
		}
	}
	return &best
}

func allNarrowest(candidates []LookupCandidate) []LookupCandidate {
	best := narrowest(candidates)
	if best == nil {
		return nil
	}
	width := rangeWidth(*best)
	result := make([]LookupCandidate, 0)
	for _, candidate := range candidates {
		if rangeWidth(candidate).Cmp(width) == 0 {
			result = append(result, candidate)
		}
	}
	return result
}

func rangeWidth(candidate LookupCandidate) *big.Int {
	start, _ := new(big.Int).SetString(candidate.StartIPSort, 10)
	end, _ := new(big.Int).SetString(candidate.EndIPSort, 10)
	if start == nil || end == nil {
		return big.NewInt(0)
	}
	return new(big.Int).Sub(end, start)
}
