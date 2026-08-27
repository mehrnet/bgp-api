package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/mehrnet/bgp-api/internal/ipkey"
)

type Repository interface {
	Lookup(context.Context, ipkey.RuntimeIP, LookupOptions) (*LookupResponse, error)
}

type MetadataRepository interface {
	DatasetMetadata(context.Context) (*DatasetMetadata, error)
}

type ResourceRepository interface {
	LookupPrefix(context.Context, ipkey.ParsedPrefix, Page) (*PrefixResponse, error)
	LookupRange(context.Context, ipkey.ParsedRange, RangeKind, RangePage) (*RangeResponse, error)
	LookupRangeSummary(context.Context, ipkey.ParsedRange, RangeKind) (*RangeResponse, error)
	LookupASN(context.Context, uint32, Page) (*ASNResponse, error)
}

type Page struct {
	Cursor   int64
	Limit    int
	Number   int
	Numbered bool
}

// RangePage keeps its cursor opaque so the immutable producer index can
// evolve independently of the API response schema.
type RangePage struct {
	Cursor string
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
	AllowedOrigins             map[string]struct{}
	OriginAuthToken            string
	Build                      BuildInfo
	CompactResponseCacheBytes  int
	ResourceResponseCacheBytes int
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
	compactCache := newCompactResponseCache(config.CompactResponseCacheBytes)
	resourceCache := newResourceResponseCache(config.ResourceResponseCacheBytes)
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
		case request.Method == http.MethodGet && request.URL.Path == "/v1/ip":
			lookupIP(writer, request, repository, compactCache, request.URL.Query().Get("query"))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/prefix":
			lookupPrefix(writer, request, repository, resourceCache)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/range":
			lookupRange(writer, request, repository, resourceCache)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/asn":
			lookupASN(writer, request, repository, resourceCache, request.URL.Query().Get("query"))
		default:
			writeError(writer, http.StatusNotFound, "NOT_FOUND", "route not found")
		}
	})
}

func health(writer http.ResponseWriter, request *http.Request, repository Repository, config Config) {
	response := HealthResponse{OK: true, Service: "bgp-api", Version: 1, Database: "bbolt"}
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

func lookupPrefix(writer http.ResponseWriter, request *http.Request, repository Repository, cache *responseCache) {
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
	cacheKey := responseCacheKey("prefix", prefix.Canonical, strconv.Itoa(page.Limit), strconv.FormatInt(page.Cursor, 10))
	if release, served := acquireCachedResponse(writer, cache, cacheKey); served {
		return
	} else {
		defer release()
	}
	response, err := resources.LookupPrefix(request.Context(), prefix, page)
	if err != nil {
		if errors.Is(err, errBboltQueryTooBroad) {
			writeError(writer, http.StatusUnprocessableEntity, "QUERY_TOO_BROAD", "prefix matches too many source records; request a narrower prefix")
			return
		}
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
	writeAndCacheJSON(writer, cache, cacheKey, response)
}

func requestRangePage(writer http.ResponseWriter, request *http.Request) (RangePage, bool) {
	page := RangePage{Limit: 50, Cursor: request.URL.Query().Get("cursor")}
	if raw := request.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			writeError(writer, http.StatusBadRequest, "INVALID_LIMIT", "limit must be between 1 and 100")
			return RangePage{}, false
		}
		page.Limit = limit
	}
	if len(page.Cursor) > 256 {
		writeError(writer, http.StatusBadRequest, "INVALID_CURSOR", "cursor is invalid")
		return RangePage{}, false
	}
	return page, true
}

func lookupRange(writer http.ResponseWriter, request *http.Request, repository Repository, cache *responseCache) {
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
	page, valid := requestRangePage(writer, request)
	if !valid {
		return
	}
	cacheKey := responseCacheKey("range", rangeValue.Start.Canonical, rangeValue.End.Canonical, string(kind))
	_, broad := ipkey.SummaryPrefixKeys(rangeValue)
	if !broad {
		cacheKey = responseCacheKey(cacheKey, strconv.Itoa(page.Limit), page.Cursor)
	}
	if release, served := acquireCachedResponse(writer, cache, cacheKey); served {
		return
	} else {
		defer release()
	}
	var response *RangeResponse
	var err error
	if broad {
		response, err = resources.LookupRangeSummary(request.Context(), rangeValue, kind)
	} else {
		response, err = resources.LookupRange(request.Context(), rangeValue, kind, page)
	}
	if err != nil {
		if errors.Is(err, errInvalidRangeCursor) {
			writeError(writer, http.StatusBadRequest, "INVALID_CURSOR", "cursor is invalid for this range lookup")
			return
		}
		if errors.Is(err, errBboltQueryTooBroad) {
			writeError(writer, http.StatusUnprocessableEntity, "QUERY_TOO_BROAD", "range matches too many source records; use a canonical IPv4 CIDR from /0 through /16 for a generated summary, or request a narrower range")
			return
		}
		log.Printf("range lookup failed for %s-%s: %v", rangeValue.Start.Canonical, rangeValue.End.Canonical, err)
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected range lookup failure")
		return
	}
	if !attachMeta(writer, request, repository, response) {
		return
	}
	writeAndCacheJSON(writer, cache, cacheKey, response)
}

func lookupASN(writer http.ResponseWriter, request *http.Request, repository Repository, cache *responseCache, input string) {
	resources, ok := resourceRepository(writer, repository)
	if !ok {
		return
	}
	asn, valid := parseASN(input)
	if !valid {
		writeError(writer, http.StatusBadRequest, "INVALID_ASN", "ASN must be a positive AS number, with or without the AS prefix")
		return
	}
	page, valid := requestASNPage(writer, request)
	if !valid {
		return
	}
	pageKey := strconv.FormatInt(page.Cursor, 10)
	if page.Numbered {
		pageKey = "page=" + strconv.Itoa(page.Number)
	}
	cacheKey := responseCacheKey("asn", strconv.FormatUint(uint64(asn), 10), strconv.Itoa(page.Limit), pageKey)
	if release, served := acquireCachedResponse(writer, cache, cacheKey); served {
		return
	} else {
		defer release()
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
	writeAndCacheJSON(writer, cache, cacheKey, response)
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

func requestASNPage(writer http.ResponseWriter, request *http.Request) (Page, bool) {
	page, valid := requestPage(writer, request)
	if !valid {
		return Page{}, false
	}
	raw := request.URL.Query().Get("page")
	if raw == "" {
		return page, true
	}
	if request.URL.Query().Get("cursor") != "" {
		writeError(writer, http.StatusBadRequest, "INVALID_PAGINATION", "page and cursor cannot be used together")
		return Page{}, false
	}
	number, err := strconv.Atoi(raw)
	if err != nil || number < 1 || number > 100000 {
		writeError(writer, http.StatusBadRequest, "INVALID_PAGE", "page must be between 1 and 100000")
		return Page{}, false
	}
	page.Number = number
	page.Numbered = true
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

func lookupIP(writer http.ResponseWriter, request *http.Request, repository Repository, cache *responseCache, input string) {
	ip, ok := ipkey.ParseRuntime(input)
	if !ok {
		writeError(writer, http.StatusBadRequest, "INVALID_IP", "query must be a valid IPv4 or IPv6 address")
		return
	}
	options, ok := lookupOptions(writer, request)
	if !ok {
		return
	}
	if options.Details == LookupDetailsNone {
		if release, served := acquireCachedResponse(writer, cache, ip.Canonical); served {
			return
		} else {
			defer release()
		}
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
	if options.Details == LookupDetailsNone {
		writeAndCacheJSON(writer, cache, ip.Canonical, response)
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

func writeJSONBytes(writer http.ResponseWriter, status int, value []byte) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write(value)
}

func acquireCachedResponse(writer http.ResponseWriter, cache *responseCache, key string) (release func(), served bool) {
	value, cached, release := cache.Acquire(key)
	if cached {
		writeJSONBytes(writer, http.StatusOK, value)
		return nil, true
	}
	return release, false
}

func writeAndCacheJSON(writer http.ResponseWriter, cache *responseCache, key string, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		log.Printf("cached response encoding failed for %s: %v", key, err)
		writeError(writer, http.StatusInternalServerError, "INTERNAL_ERROR", "unexpected response serialization failure")
		return
	}
	body = append(body, '\n')
	cache.Add(key, body)
	writeJSONBytes(writer, http.StatusOK, body)
}

func responseCacheKey(parts ...string) string { return strings.Join(parts, "\x00") }

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
