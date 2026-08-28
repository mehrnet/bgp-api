package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mehrnet/bgp-api/internal/ipkey"
)

type fakeRepository struct{}

func (fakeRepository) Lookup(_ context.Context, ip ipkey.RuntimeIP, options LookupOptions) (*LookupResponse, error) {
	response := &LookupResponse{IP: ip.Canonical, Version: ip.Version}
	response.Network.ASNs = []string{}
	if options.Details == LookupDetailsFull {
		response.Details = &LookupDetails{Allocations: []LookupDetailRecord{{StartIP: "1.1.1.0", EndIP: "1.1.1.255", Version: 4}}}
	}
	return response, nil
}

func (fakeRepository) DatasetMetadata(context.Context) (*DatasetMetadata, error) {
	releaseTag := "db-2026.07.27-0400-1"
	builtAt := "2026-07-27T04:00:00Z"
	activatedAt := "2026-07-27 05:00:00+00"
	sourceCommit := "0123456789abcdef"
	return &DatasetMetadata{ReleaseTag: &releaseTag, BuiltAt: &builtAt, ActivatedAt: &activatedAt, SourceCommit: &sourceCommit}, nil
}

type resourceFakeRepository struct{ fakeRepository }

type broadQueryFakeRepository struct{ resourceFakeRepository }

func (broadQueryFakeRepository) LookupPrefix(context.Context, ipkey.ParsedPrefix, Page) (*PrefixResponse, error) {
	return nil, errBboltQueryTooBroad
}

func (broadQueryFakeRepository) LookupRange(context.Context, ipkey.ParsedRange, RangeKind, RangePage) (*RangeResponse, error) {
	return nil, errBboltQueryTooBroad
}

func (resourceFakeRepository) LookupPrefix(_ context.Context, prefix ipkey.ParsedPrefix, _ Page) (*PrefixResponse, error) {
	return &PrefixResponse{
		Prefix: PrefixDescriptor{CIDR: prefix.Canonical, Version: prefix.Version, StartIP: prefix.Start.Canonical, EndIP: prefix.End.Canonical, AddressCount: prefix.AddressCount},
		Routes: RoutePage{Items: []RouteObject{}},
	}, nil
}

func (resourceFakeRepository) LookupRange(_ context.Context, rangeValue ipkey.ParsedRange, kind RangeKind, _ RangePage) (*RangeResponse, error) {
	return &RangeResponse{Range: RangeDescriptor{StartIP: rangeValue.Start.Canonical, EndIP: rangeValue.End.Canonical, Version: rangeValue.Version, AddressCount: rangeValue.AddressCount}, Kind: kind, Mode: "records", Allocations: []AllocationObject{}}, nil
}

func (resourceFakeRepository) LookupRangeSummary(_ context.Context, rangeValue ipkey.ParsedRange, kind RangeKind) (*RangeResponse, error) {
	return &RangeResponse{
		Range: rangeDescriptor(rangeValue),
		Kind:  kind,
		Mode:  "summary",
		Summary: &RangeSummary{
			Aggregation: "overlapping_source_records", BucketPrefixLength: 16, Buckets: 32,
			Countries: []RangeFacet{}, ASNs: []RangeFacet{},
		},
	}, nil
}

func (resourceFakeRepository) LookupASN(_ context.Context, asn uint32, page Page) (*ASNResponse, error) {
	routes := RoutePage{Items: []RouteObject{}}
	if page.Numbered {
		routes.Page, routes.TotalPages, routes.TotalItems = page.Number, 3, 101
	}
	return &ASNResponse{ASN: "AS13335", ASNumber: int(asn), Routes: routes}, nil
}

func TestHandlerRejectsInvalidIP(t *testing.T) {
	handler := New(fakeRepository{}, Config{})
	request := httptest.NewRequest(http.MethodGet, "/v1/ip?query=not-an-ip", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Body.String(); got != "{\"error\":{\"code\":\"INVALID_IP\",\"message\":\"query must be a valid IPv4 or IPv6 address\"}}\n" {
		t.Fatalf("body = %s", got)
	}
}

func TestHandlerRequiresOriginToken(t *testing.T) {
	handler := New(fakeRepository{}, Config{OriginAuthToken: "shared-token"})
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	request.Header.Set("X-BGP-API-Origin-Token", "shared-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized status = %d", response.Code)
	}
}

func TestHealthIncludesDatasetMetadata(t *testing.T) {
	version := "db-2026.07.27-0719-9"
	commit := "62a5fd6b0c94e7f58aaeb4cf8e44394ab054c340"
	builtAt := "2026-07-27T07:27:00Z"
	handler := New(fakeRepository{}, Config{Build: BuildInfo{Version: &version, Commit: &commit, BuiltAt: &builtAt}})
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"release_tag":"db-2026.07.27-0400-1"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"build":{"version":"db-2026.07.27-0719-9","commit":"62a5fd6b0c94e7f58aaeb4cf8e44394ab054c340","built_at":"2026-07-27T07:27:00Z"}`) {
		t.Fatalf("missing build metadata: %s", response.Body.String())
	}
}

func TestHealthReportsEnabledRuntimeCacheControl(t *testing.T) {
	handler := New(fakeRepository{}, Config{RuntimeCacheControl: true})
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"runtime":{"cache_control":true}`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestHandlerIgnoresLegacyEnrichmentParameter(t *testing.T) {
	handler := New(fakeRepository{}, Config{})
	request := httptest.NewRequest(http.MethodGet, "/v1/ip?query=1.1.1.1&enrich=1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"enrichment"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestHandlerReturnsFullLookupDetails(t *testing.T) {
	handler := New(fakeRepository{}, Config{})
	request := httptest.NewRequest(http.MethodGet, "/v1/ip?query=1.1.1.1&details=full", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"details":{"allocations":[`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestHandlerRejectsInvalidDetailsMode(t *testing.T) {
	handler := New(fakeRepository{}, Config{})
	request := httptest.NewRequest(http.MethodGet, "/v1/ip?query=1.1.1.1&details=raw", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_DETAILS"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestHandlerDoesNotServeLegacyPathOrSearchRoutes(t *testing.T) {
	handler := New(fakeRepository{}, Config{})
	for _, path := range []string{"/v1/me", "/v1/ip/1.1.1.1", "/v1/asn/AS13335", "/v1/search?q=1.1.1.1"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}

type countingRepository struct {
	fakeRepository
	lookups int
}

type countingResourceRepository struct {
	resourceFakeRepository
	prefixCalls       int
	rangeCalls        int
	rangeSummaryCalls int
	asnCalls          int
}

func (repository *countingResourceRepository) LookupPrefix(ctx context.Context, prefix ipkey.ParsedPrefix, page Page) (*PrefixResponse, error) {
	repository.prefixCalls++
	return repository.resourceFakeRepository.LookupPrefix(ctx, prefix, page)
}

func (repository *countingResourceRepository) LookupRange(ctx context.Context, value ipkey.ParsedRange, kind RangeKind, page RangePage) (*RangeResponse, error) {
	repository.rangeCalls++
	return repository.resourceFakeRepository.LookupRange(ctx, value, kind, page)
}

func (repository *countingResourceRepository) LookupRangeSummary(ctx context.Context, value ipkey.ParsedRange, kind RangeKind) (*RangeResponse, error) {
	repository.rangeSummaryCalls++
	return repository.resourceFakeRepository.LookupRangeSummary(ctx, value, kind)
}

func (repository *countingResourceRepository) LookupASN(ctx context.Context, asn uint32, page Page) (*ASNResponse, error) {
	repository.asnCalls++
	return repository.resourceFakeRepository.LookupASN(ctx, asn, page)
}

func (repository *countingRepository) Lookup(ctx context.Context, ip ipkey.RuntimeIP, options LookupOptions) (*LookupResponse, error) {
	repository.lookups++
	return repository.fakeRepository.Lookup(ctx, ip, options)
}

func TestHandlerCachesOnlyDefaultCompactIPResponses(t *testing.T) {
	repository := &countingRepository{}
	handler := New(repository, Config{CompactResponseCacheBytes: 1 << 20})
	request := httptest.NewRequest(http.MethodGet, "/v1/ip?query=1.1.1.1", nil)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, request)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, request)
	if repository.lookups != 1 || first.Body.String() != second.Body.String() {
		t.Fatalf("lookups = %d, first = %s, second = %s", repository.lookups, first.Body.String(), second.Body.String())
	}
	full := httptest.NewRecorder()
	handler.ServeHTTP(full, httptest.NewRequest(http.MethodGet, "/v1/ip?query=1.1.1.1&details=full", nil))
	if repository.lookups != 2 {
		t.Fatalf("details=full lookup count = %d", repository.lookups)
	}
}

func TestRuntimeCacheControllerDropsAndRestoresResponseCaching(t *testing.T) {
	repository := &countingRepository{}
	handler, controller := NewWithRuntime(repository, Config{CompactResponseCacheBytes: 1 << 20})
	request := httptest.NewRequest(http.MethodGet, "/v1/ip?query=1.1.1.1", nil)

	handler.ServeHTTP(httptest.NewRecorder(), request)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if repository.lookups != 1 {
		t.Fatalf("warm cache lookup count = %d", repository.lookups)
	}

	release := controller.DisableAndClear()
	if release.CompactEntries != 1 || release.CompactBytes < 1 {
		t.Fatalf("cache release = %#v", release)
	}
	handler.ServeHTTP(httptest.NewRecorder(), request)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if repository.lookups != 3 {
		t.Fatalf("disabled cache lookup count = %d", repository.lookups)
	}

	controller.Enable()
	handler.ServeHTTP(httptest.NewRecorder(), request)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if repository.lookups != 4 {
		t.Fatalf("restored cache lookup count = %d", repository.lookups)
	}
}

func TestHandlerCachesCanonicalResourceResponses(t *testing.T) {
	repository := &countingResourceRepository{}
	handler := New(repository, Config{ResourceResponseCacheBytes: 1 << 20})
	for _, path := range []string{
		"/v1/prefix?prefix=1.1.1.42/24&limit=10",
		"/v1/range?start=1.1.1.1&end=1.1.1.2&limit=10",
		"/v1/range?start=80.0.0.0&end=80.255.255.255",
		"/v1/asn?query=AS13335&limit=10&page=1",
	} {
		for range 2 {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
			}
		}
	}
	if repository.prefixCalls != 1 || repository.rangeCalls != 1 || repository.rangeSummaryCalls != 1 || repository.asnCalls != 1 {
		t.Fatalf("resource calls = prefix:%d range:%d summary:%d asn:%d", repository.prefixCalls, repository.rangeCalls, repository.rangeSummaryCalls, repository.asnCalls)
	}
}

func TestResponseCacheCollapsesConcurrentMisses(t *testing.T) {
	cache := newResponseCache(1<<20, 1<<20, 1<<10)
	start := make(chan struct{})
	leaderStarted := make(chan struct{})
	allowLeader := make(chan struct{})
	var producers atomic.Int32
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			value, cached, release := cache.Acquire("query")
			if cached {
				if string(value) != "body" {
					t.Errorf("cached value = %q", value)
				}
				return
			}
			if producers.Add(1) == 1 {
				close(leaderStarted)
			}
			<-allowLeader
			cache.Add("query", []byte("body"))
			release()
		}()
	}
	close(start)
	<-leaderStarted
	close(allowLeader)
	group.Wait()
	if producers.Load() != 1 {
		t.Fatalf("producers = %d", producers.Load())
	}
}

func TestHandlerLooksUpPrefixRangeAndASN(t *testing.T) {
	handler := New(resourceFakeRepository{}, Config{})
	for _, test := range []struct {
		path string
		want string
	}{
		{"/v1/prefix?prefix=1.1.1.42/24", `"cidr":"1.1.1.0/24"`},
		{"/v1/range?start=1.1.1.1&end=1.1.1.2", `"address_count":"2"`},
		{"/v1/range?start=80.0.0.0&end=80.255.255.255", `"mode":"summary"`},
		{"/v1/asn?query=13335", `"asn":"AS13335"`},
		{"/v1/asn?query=13335&page=2", `"total_pages":3`},
	} {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) {
			t.Fatalf("%s: status = %d, body = %s", test.path, response.Code, response.Body.String())
		}
	}
}

func TestHandlerRejectsInvalidResourceQueries(t *testing.T) {
	handler := New(resourceFakeRepository{}, Config{})
	for _, path := range []string{
		"/v1/prefix?prefix=invalid",
		"/v1/range?start=1.1.1.2&end=1.1.1.1",
		"/v1/range?start=1.1.1.1&end=1.1.1.2&kind=unknown",
		"/v1/asn?query=AS0",
		"/v1/asn?query=AS13335&limit=101",
		"/v1/asn?query=AS13335&page=0",
		"/v1/asn?query=AS13335&page=1&cursor=10",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}

func TestHandlerBoundsRecordOverlapQueries(t *testing.T) {
	handler := New(broadQueryFakeRepository{}, Config{})
	for _, path := range []string{
		"/v1/prefix?prefix=1.1.1.0%2F24",
		"/v1/range?start=1.1.1.1&end=1.1.1.2",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"code":"QUERY_TOO_BROAD"`) {
			t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}
