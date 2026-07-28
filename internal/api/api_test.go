package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/mehrnet/bgp-api/internal/ipkey"
)

type fakeRepository struct{}

func (fakeRepository) Lookup(_ context.Context, ip ipkey.Parsed, options LookupOptions) (*LookupResponse, error) {
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

func (resourceFakeRepository) LookupPrefix(_ context.Context, prefix ipkey.ParsedPrefix, _ Page) (*PrefixResponse, error) {
	return &PrefixResponse{
		Prefix: PrefixDescriptor{CIDR: prefix.Canonical, Version: prefix.Version, StartIP: prefix.Start.Canonical, EndIP: prefix.End.Canonical, AddressCount: prefix.AddressCount},
		Routes: RoutePage{Items: []RouteObject{}},
	}, nil
}

func (resourceFakeRepository) LookupRange(_ context.Context, rangeValue ipkey.ParsedRange, kind RangeKind, _ Page) (*RangeResponse, error) {
	return &RangeResponse{Range: RangeDescriptor{StartIP: rangeValue.Start.Canonical, EndIP: rangeValue.End.Canonical, Version: rangeValue.Version, AddressCount: rangeValue.AddressCount}, Kind: kind, Allocations: []AllocationObject{}}, nil
}

func (resourceFakeRepository) LookupASN(_ context.Context, asn uint32, _ Page) (*ASNResponse, error) {
	return &ASNResponse{ASN: "AS13335", ASNumber: int(asn), Routes: RoutePage{Items: []RouteObject{}}}, nil
}

func TestHandlerRejectsInvalidIP(t *testing.T) {
	handler := New(fakeRepository{}, Config{DatabaseEngine: "postgresql"})
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
	handler := New(fakeRepository{}, Config{DatabaseEngine: "postgresql", OriginAuthToken: "shared-token"})
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
	handler := New(fakeRepository{}, Config{DatabaseEngine: "postgresql", Build: BuildInfo{Version: &version, Commit: &commit, BuiltAt: &builtAt}})
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

func TestHandlerLooksUpTrustedCloudflareClientIP(t *testing.T) {
	handler := New(fakeRepository{}, Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.RemoteAddr = "127.0.0.1:3102"
	request.Header.Set("X-BGP-API-Cloudflare-IP", "185.227.108.163")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ip":"185.227.108.163"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"meta":{"dataset":`) {
		t.Fatalf("missing dataset meta: %s", response.Body.String())
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

func TestHandlerDoesNotServeLegacyIPPathOrSearchRoute(t *testing.T) {
	handler := New(fakeRepository{}, Config{})
	for _, path := range []string{"/v1/ip/1.1.1.1", "/v1/search?q=1.1.1.1"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}

func TestHandlerLooksUpTrustedCloudflareIPv6(t *testing.T) {
	handler := New(fakeRepository{}, Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("::1/128")}})
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.RemoteAddr = "[::1]:3102"
	request.Header.Set("X-BGP-API-Cloudflare-IP", "240.16.0.1")
	request.Header.Set("X-BGP-API-Cloudflare-IPv6", "2606:4700:4700::1111")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ip":"2606:4700:4700::1111"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestHandlerIgnoresForwardedIPFromUntrustedPeer(t *testing.T) {
	handler := New(fakeRepository{}, Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.RemoteAddr = "94.141.98.12:443"
	request.Header.Set("X-BGP-API-Cloudflare-IP", "185.227.108.163")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ip":"94.141.98.12"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
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
		{"/v1/asn/13335", `"asn":"AS13335"`},
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
		"/v1/asn/AS0",
		"/v1/asn/AS13335?limit=101",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}
