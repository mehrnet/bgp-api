package api

import (
	"context"
	"encoding/json"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/mehrnet/bgp-api/internal/ipkey"
)

type Repository interface {
	Lookup(context.Context, ipkey.Parsed, LookupOptions) (*LookupResponse, error)
}

type MetadataRepository interface {
	DatasetMetadata(context.Context) (*DatasetMetadata, error)
}

type ResourceRepository interface {
	LookupPrefix(context.Context, ipkey.ParsedPrefix, Page) (*PrefixResponse, error)
	LookupRange(context.Context, ipkey.ParsedRange, RangeKind, Page) (*RangeResponse, error)
	LookupASN(context.Context, uint32, Page) (*ASNResponse, error)
}

type Page struct {
	Cursor int64
	Limit  int
}

type RangeKind string

const (
	RangeAllocations RangeKind = "allocations"
	RangeRoutes      RangeKind = "routes"
)

type LookupOptions struct {
	Details LookupDetailsMode
}

type LookupDetailsMode string

const (
	LookupDetailsNone LookupDetailsMode = ""
	LookupDetailsFull LookupDetailsMode = "full"
)

type Config struct {
	AllowedOrigins  map[string]struct{}
	OriginAuthToken string
	Build           BuildInfo
	DatabaseEngine  string
	TrustedProxies  []netip.Prefix
}

type nullableString = *string

type LookupResponse struct {
	Meta             *ResponseMeta  `json:"meta,omitempty"`
	IP               string         `json:"ip"`
	Version          int            `json:"version"`
	Registry         nullableString `json:"registry"`
	AllocationDate   nullableString `json:"allocation_date"`
	AllocationStatus nullableString `json:"allocation_status"`
	Network          struct {
		CIDR         nullableString `json:"cidr"`
		StartIP      nullableString `json:"start_ip"`
		EndIP        nullableString `json:"end_ip"`
		ASN          nullableString `json:"asn"`
		ASNs         []string       `json:"asns"`
		ASNumber     *int           `json:"as_number"`
		Name         nullableString `json:"name"`
		Status       nullableString `json:"status"`
		AbuseContact nullableString `json:"abuse_contact"`
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
		AbuseContact   nullableString `json:"abuse_contact"`
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
	Details *LookupDetails `json:"details,omitempty"`
}

type LookupDetails struct {
	Allocations []LookupDetailRecord `json:"allocations,omitempty"`
	Routes      []LookupDetailRecord `json:"routes,omitempty"`
	Geofeeds    []LookupDetailRecord `json:"geofeeds,omitempty"`
}

type LookupDetailRecord struct {
	StartIP        string         `json:"start_ip"`
	EndIP          string         `json:"end_ip"`
	Version        int            `json:"version"`
	Registry       nullableString `json:"registry,omitempty"`
	CountryCode    nullableString `json:"country_code,omitempty"`
	CountryRaw     nullableString `json:"country_raw,omitempty"`
	Name           nullableString `json:"name,omitempty"`
	CIDR           nullableString `json:"cidr,omitempty"`
	ASN            nullableString `json:"asn,omitempty"`
	ASNumber       *int           `json:"as_number,omitempty"`
	Region         nullableString `json:"region,omitempty"`
	City           nullableString `json:"city,omitempty"`
	Status         nullableString `json:"status,omitempty"`
	AllocationDate nullableString `json:"allocation_date,omitempty"`
	Created        nullableString `json:"created,omitempty"`
	LastModified   nullableString `json:"last_modified,omitempty"`
	Source         nullableString `json:"source,omitempty"`
	Maintainers    nullableString `json:"maintainers,omitempty"`
	Organization   nullableString `json:"organization,omitempty"`
	AbuseContact   nullableString `json:"abuse_contact,omitempty"`
	Description    nullableString `json:"description,omitempty"`
}

type ResponseMeta struct {
	Dataset *DatasetMetadata `json:"dataset,omitempty"`
}

type DatasetMetadata struct {
	ReleaseTag   nullableString `json:"release_tag"`
	BuiltAt      nullableString `json:"built_at"`
	ActivatedAt  nullableString `json:"activated_at"`
	SourceCommit nullableString `json:"source_commit"`
}

type BuildInfo struct {
	Version nullableString `json:"version"`
	Commit  nullableString `json:"commit"`
	BuiltAt nullableString `json:"built_at"`
}

type HealthResponse struct {
	OK       bool             `json:"ok"`
	Service  string           `json:"service"`
	Version  int              `json:"version"`
	Database string           `json:"database"`
	Build    *BuildInfo       `json:"build,omitempty"`
	Dataset  *DatasetMetadata `json:"dataset,omitempty"`
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
			health(writer, request, repository, config)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/me":
			lookupIP(writer, request, repository, clientIP(request, config.TrustedProxies))
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/ip/"):
			lookupIP(writer, request, repository, strings.TrimPrefix(request.URL.Path, "/v1/ip/"))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/prefix":
			lookupPrefix(writer, request, repository)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/range":
			lookupRange(writer, request, repository)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/asn/"):
			lookupASN(writer, request, repository, strings.TrimPrefix(request.URL.Path, "/v1/asn/"))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/search":
			search(writer, request)
		default:
			writeError(writer, http.StatusNotFound, "NOT_FOUND", "route not found")
		}
	})
}

func health(writer http.ResponseWriter, request *http.Request, repository Repository, config Config) {
	response := HealthResponse{OK: true, Service: "bgp-api", Version: 1, Database: config.DatabaseEngine}
	response.Build = buildInfo(config.Build)
	metadata, err := datasetMetadata(request.Context(), repository)
	if err != nil {
		log.Printf("dataset metadata lookup failed: %v", err)
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected metadata lookup failure")
		return
	}
	response.Dataset = metadata
	writeJSON(writer, http.StatusOK, response)
}

func buildInfo(info BuildInfo) *BuildInfo {
	info.Version = present(info.Version)
	info.Commit = present(info.Commit)
	info.BuiltAt = present(info.BuiltAt)
	if info.Version == nil && info.Commit == nil && info.BuiltAt == nil {
		return nil
	}
	return &info
}

func resourceRepository(writer http.ResponseWriter, repository Repository) (ResourceRepository, bool) {
	resources, ok := repository.(ResourceRepository)
	if !ok {
		writeError(writer, http.StatusServiceUnavailable, "DATASET_FEATURE_UNAVAILABLE", "this dataset does not include normalized route and ASN records")
	}
	return resources, ok
}

func lookupPrefix(writer http.ResponseWriter, request *http.Request, repository Repository) {
	resources, ok := resourceRepository(writer, repository)
	if !ok {
		return
	}
	prefix, valid := ipkey.ParsePrefix(request.URL.Query().Get("prefix"))
	if !valid {
		writeError(writer, http.StatusBadRequest, "INVALID_PREFIX", "prefix must be a valid IPv4 or IPv6 CIDR")
		return
	}
	page, valid := requestPage(writer, request)
	if !valid {
		return
	}
	response, err := resources.LookupPrefix(request.Context(), prefix, page)
	if err != nil {
		log.Printf("prefix lookup failed for %s: %v", prefix.Canonical, err)
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected prefix lookup failure")
		return
	}
	if response == nil {
		writeError(writer, http.StatusNotFound, "PREFIX_NOT_FOUND", "no allocation or route object matched this prefix")
		return
	}
	if !attachMeta(writer, request, repository, response) {
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func lookupRange(writer http.ResponseWriter, request *http.Request, repository Repository) {
	resources, ok := resourceRepository(writer, repository)
	if !ok {
		return
	}
	query := request.URL.Query()
	rangeValue, valid := ipkey.ParseRange(query.Get("start"), query.Get("end"))
	if !valid {
		writeError(writer, http.StatusBadRequest, "INVALID_RANGE", "start and end must be valid addresses in the same IP version, with start not greater than end")
		return
	}
	kind := RangeKind(query.Get("kind"))
	if kind == "" {
		kind = RangeAllocations
	}
	if kind != RangeAllocations && kind != RangeRoutes {
		writeError(writer, http.StatusBadRequest, "INVALID_RANGE_KIND", "kind must be allocations or routes")
		return
	}
	page, valid := requestPage(writer, request)
	if !valid {
		return
	}
	response, err := resources.LookupRange(request.Context(), rangeValue, kind, page)
	if err != nil {
		log.Printf("range lookup failed for %s-%s: %v", rangeValue.Start.Canonical, rangeValue.End.Canonical, err)
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected range lookup failure")
		return
	}
	if !attachMeta(writer, request, repository, response) {
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func lookupASN(writer http.ResponseWriter, request *http.Request, repository Repository, input string) {
	resources, ok := resourceRepository(writer, repository)
	if !ok {
		return
	}
	asn, valid := parseASN(input)
	if !valid {
		writeError(writer, http.StatusBadRequest, "INVALID_ASN", "ASN must be a positive AS number, with or without the AS prefix")
		return
	}
	page, valid := requestPage(writer, request)
	if !valid {
		return
	}
	response, err := resources.LookupASN(request.Context(), asn, page)
	if err != nil {
		log.Printf("ASN lookup failed for AS%d: %v", asn, err)
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected ASN lookup failure")
		return
	}
	if response == nil {
		writeError(writer, http.StatusNotFound, "ASN_NOT_FOUND", "no route or aut-num object matched this ASN")
		return
	}
	if !attachMeta(writer, request, repository, response) {
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func search(writer http.ResponseWriter, request *http.Request) {
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	if query == "" {
		writeError(writer, http.StatusBadRequest, "INVALID_QUERY", "q is required")
		return
	}
	if ip, ok := ipkey.Parse(query); ok {
		writeJSON(writer, http.StatusOK, SearchResponse{Query: query, Type: "ip", Normalized: ip.Canonical, Endpoint: "/v1/ip/" + ip.Canonical})
		return
	}
	if prefix, ok := ipkey.ParsePrefix(query); ok {
		writeJSON(writer, http.StatusOK, SearchResponse{Query: query, Type: "prefix", Normalized: prefix.Canonical, Endpoint: "/v1/prefix?prefix=" + prefix.Canonical})
		return
	}
	if asn, ok := parseASN(query); ok {
		writeJSON(writer, http.StatusOK, SearchResponse{Query: query, Type: "asn", Normalized: "AS" + strconv.FormatUint(uint64(asn), 10), Endpoint: "/v1/asn/AS" + strconv.FormatUint(uint64(asn), 10)})
		return
	}
	writeError(writer, http.StatusBadRequest, "INVALID_QUERY", "q must be an IP address, CIDR, or ASN")
}

func requestPage(writer http.ResponseWriter, request *http.Request) (Page, bool) {
	page := Page{Limit: 50}
	query := request.URL.Query()
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(writer, http.StatusBadRequest, "INVALID_LIMIT", "limit must be between 1 and 100")
			return Page{}, false
		}
		page.Limit = limit
	}
	if raw := query.Get("cursor"); raw != "" {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			writeError(writer, http.StatusBadRequest, "INVALID_CURSOR", "cursor must be a non-negative record cursor")
			return Page{}, false
		}
		page.Cursor = cursor
	}
	return page, true
}

func parseASN(input string) (uint32, bool) {
	value := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(input)), "AS")
	number, err := strconv.ParseUint(value, 10, 32)
	if err != nil || number == 0 {
		return 0, false
	}
	return uint32(number), true
}

func lookupIP(writer http.ResponseWriter, request *http.Request, repository Repository, input string) {
	ip, ok := ipkey.Parse(input)
	if !ok {
		writeError(writer, http.StatusBadRequest, "INVALID_IP", "path parameter must be a valid IPv4 or IPv6 address")
		return
	}
	options, ok := lookupOptions(writer, request)
	if !ok {
		return
	}
	response, err := repository.Lookup(request.Context(), ip, options)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected lookup failure")
		return
	}
	if response == nil {
		writeError(writer, http.StatusNotFound, "IP_NOT_FOUND", "no RIR allocation, route, or geofeed record matched this IP")
		return
	}
	if !attachMeta(writer, request, repository, response) {
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func lookupOptions(writer http.ResponseWriter, request *http.Request) (LookupOptions, bool) {
	rawDetails := strings.TrimSpace(request.URL.Query().Get("details"))
	if rawDetails == "" {
		return LookupOptions{}, true
	}
	if rawDetails != string(LookupDetailsFull) {
		writeError(writer, http.StatusBadRequest, "INVALID_DETAILS", "details must be full when present")
		return LookupOptions{}, false
	}
	return LookupOptions{Details: LookupDetailsFull}, true
}

func attachMeta(writer http.ResponseWriter, request *http.Request, repository Repository, response any) bool {
	metadata, err := datasetMetadata(request.Context(), repository)
	if err != nil {
		log.Printf("dataset metadata lookup failed: %v", err)
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected metadata lookup failure")
		return false
	}
	if metadata == nil {
		return true
	}
	meta := &ResponseMeta{Dataset: metadata}
	switch value := response.(type) {
	case *LookupResponse:
		value.Meta = meta
	case *PrefixResponse:
		value.Meta = meta
	case *RangeResponse:
		value.Meta = meta
	case *ASNResponse:
		value.Meta = meta
	}
	return true
}

func datasetMetadata(ctx context.Context, repository Repository) (*DatasetMetadata, error) {
	metadataRepository, ok := repository.(MetadataRepository)
	if !ok {
		return nil, nil
	}
	return metadataRepository.DatasetMetadata(ctx)
}

func clientIP(request *http.Request, trustedProxies []netip.Prefix) string {
	peer, ok := addressFromRemote(request.RemoteAddr)
	if !ok {
		return ""
	}
	if !isTrustedProxy(peer, trustedProxies) {
		return peer.String()
	}
	for _, header := range []string{"X-BGP-API-Cloudflare-IPv6", "X-BGP-API-Cloudflare-IP", "X-BGP-API-Forwarded-IP"} {
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
	StartIPSort    string
	EndIPSort      string
	IPVersion      int
	Registry       nullableString
	Country        nullableString
	Netname        nullableString
	CIDR           nullableString
	ASN            nullableString
	Region         nullableString
	City           nullableString
	Status         nullableString
	AllocationDate nullableString
	Created        nullableString
	LastModified   nullableString
	RecordSource   nullableString
	MntBy          nullableString
	Org            nullableString
	AbuseContact   nullableString
	Description    nullableString
}

func BuildResponse(ip ipkey.Parsed, allocations, routes, geolocations []LookupCandidate, options LookupOptions) *LookupResponse {
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
	if len(bestRoutes) > 0 {
		response.Network.Status = nullable(&bestRoutes[0], func(row LookupCandidate) nullableString { return row.Status })
		response.Network.AbuseContact = nullable(&bestRoutes[0], func(row LookupCandidate) nullableString { return row.AbuseContact })
	}

	if allocation != nil {
		start := ipkey.SortKeyToIP(allocation.StartIPSort, ip.Version)
		end := ipkey.SortKeyToIP(allocation.EndIPSort, ip.Version)
		response.Allocation.StartIP, response.Allocation.EndIP = &start, &end
		response.Allocation.Registry = lower(nullable(allocation, func(row LookupCandidate) nullableString { return row.Registry }))
		response.Allocation.CountryRaw = upper(nullable(allocation, func(row LookupCandidate) nullableString { return row.Country }))
		response.Allocation.CountryCode = countryCode(response.Allocation.CountryRaw)
		response.Allocation.Name = nullable(allocation, func(row LookupCandidate) nullableString { return row.Netname })
		response.Allocation.AllocationDate = nullable(allocation, func(row LookupCandidate) nullableString { return row.AllocationDate })
		response.Allocation.Status = nullable(allocation, func(row LookupCandidate) nullableString { return row.Status })
		response.Allocation.AbuseContact = nullable(allocation, func(row LookupCandidate) nullableString { return row.AbuseContact })
		response.AllocationDate = response.Allocation.AllocationDate
		response.AllocationStatus = response.Allocation.Status
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
	if options.Details == LookupDetailsFull {
		response.Details = &LookupDetails{
			Allocations: detailRecords(allocations, ip.Version),
			Routes:      detailRecords(routes, ip.Version),
			Geofeeds:    detailRecords(geolocations, ip.Version),
		}
	}
	return response
}

func detailRecords(candidates []LookupCandidate, version int) []LookupDetailRecord {
	if len(candidates) == 0 {
		return nil
	}
	records := make([]LookupDetailRecord, 0, len(candidates))
	for _, candidate := range candidates {
		record := LookupDetailRecord{
			StartIP:        ipkey.SortKeyToIP(candidate.StartIPSort, version),
			EndIP:          ipkey.SortKeyToIP(candidate.EndIPSort, version),
			Version:        candidate.IPVersion,
			Registry:       lower(candidate.Registry),
			CountryRaw:     upper(candidate.Country),
			Name:           present(candidate.Netname),
			CIDR:           present(candidate.CIDR),
			ASN:            upper(candidate.ASN),
			Region:         present(candidate.Region),
			City:           present(candidate.City),
			Status:         present(candidate.Status),
			AllocationDate: present(candidate.AllocationDate),
			Created:        present(candidate.Created),
			LastModified:   present(candidate.LastModified),
			Source:         present(candidate.RecordSource),
			Maintainers:    present(candidate.MntBy),
			Organization:   present(candidate.Org),
			AbuseContact:   present(candidate.AbuseContact),
			Description:    present(candidate.Description),
		}
		record.CountryCode = countryCode(record.CountryRaw)
		record.ASNumber = asNumber(record.ASN)
		records = append(records, record)
	}
	return records
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
	value = present(value)
	if value == nil {
		return nil
	}
	lowercase := strings.ToLower(*value)
	return &lowercase
}

func upper(value nullableString) nullableString {
	value = present(value)
	if value == nil {
		return nil
	}
	uppercase := strings.ToUpper(*value)
	return &uppercase
}

func present(value nullableString) nullableString {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
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
