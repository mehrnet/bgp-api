package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mehrnet/bgp-api/internal/ipkey"
)

type fakeRepository struct{}

func (fakeRepository) Lookup(_ context.Context, ip ipkey.Parsed) (*LookupResponse, error) {
	response := &LookupResponse{IP: ip.Canonical, Version: ip.Version}
	response.Network.ASNs = []string{}
	return response, nil
}

func TestHandlerRejectsInvalidIP(t *testing.T) {
	handler := New(fakeRepository{}, Config{DatabaseEngine: "postgresql"})
	request := httptest.NewRequest(http.MethodGet, "/v1/ip/not-an-ip", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Body.String(); got != "{\"error\":{\"code\":\"INVALID_IP\",\"message\":\"path parameter must be a valid IPv4 or IPv6 address\"}}\n" {
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
