package api

import "context"

// Enricher adds best-effort, cached data from public RDAP and RIPEstat APIs.
// A lookup remains complete when either upstream is unavailable.
type Enricher interface {
	Prefix(context.Context, string) *PrefixEnrichment
	ASN(context.Context, uint32) *ASNEnrichment
}

type PrefixDescriptor struct {
	CIDR         string `json:"cidr"`
	Version      int    `json:"version"`
	PrefixLength int    `json:"prefix_length"`
	StartIP      string `json:"start_ip"`
	EndIP        string `json:"end_ip"`
	AddressCount string `json:"address_count"`
}

type RangeDescriptor struct {
	StartIP      string `json:"start_ip"`
	EndIP        string `json:"end_ip"`
	Version      int    `json:"version"`
	AddressCount string `json:"address_count"`
}

type RouteObject struct {
	ID           int64          `json:"-"`
	Prefix       string         `json:"prefix"`
	Version      int            `json:"version"`
	OriginASN    string         `json:"origin_asn"`
	ASNumber     *int           `json:"as_number"`
	Relation     string         `json:"relation,omitempty"`
	Registry     nullableString `json:"registry"`
	Source       nullableString `json:"source"`
	Maintainers  nullableString `json:"maintainers"`
	Organization nullableString `json:"organization"`
	Description  nullableString `json:"description"`
}

type AllocationObject struct {
	ID             int64          `json:"-"`
	StartIP        string         `json:"start_ip"`
	EndIP          string         `json:"end_ip"`
	Version        int            `json:"version"`
	Registry       nullableString `json:"registry"`
	CountryCode    nullableString `json:"country_code"`
	CountryRaw     nullableString `json:"country_raw"`
	Name           nullableString `json:"name"`
	Status         nullableString `json:"status"`
	AllocationDate nullableString `json:"allocation_date"`
	Created        nullableString `json:"created"`
	LastModified   nullableString `json:"last_modified"`
	Source         nullableString `json:"source"`
	Maintainers    nullableString `json:"maintainers"`
	Organization   nullableString `json:"organization"`
	Description    nullableString `json:"description"`
}

type AutnumObject struct {
	ASN          string         `json:"asn"`
	ASNumber     int            `json:"as_number"`
	Registry     nullableString `json:"registry"`
	CountryCode  nullableString `json:"country_code"`
	CountryRaw   nullableString `json:"country_raw"`
	Name         nullableString `json:"name"`
	Organization nullableString `json:"organization"`
	Status       nullableString `json:"status"`
	Created      nullableString `json:"created"`
	LastModified nullableString `json:"last_modified"`
	Source       nullableString `json:"source"`
	Maintainers  nullableString `json:"maintainers"`
	Description  nullableString `json:"description"`
}

type RoutePage struct {
	Items      []RouteObject  `json:"items"`
	NextCursor nullableString `json:"next_cursor"`
}

type PrefixResponse struct {
	Prefix     PrefixDescriptor  `json:"prefix"`
	Allocation *AllocationObject `json:"allocation"`
	Routes     RoutePage         `json:"routes"`
	Enrichment *PrefixEnrichment `json:"enrichment,omitempty"`
}

type RangeResponse struct {
	Range       RangeDescriptor    `json:"range"`
	Kind        RangeKind          `json:"kind"`
	Allocations []AllocationObject `json:"allocations,omitempty"`
	Routes      []RouteObject      `json:"routes,omitempty"`
	NextCursor  nullableString     `json:"next_cursor"`
}

type ASNResponse struct {
	ASN        string         `json:"asn"`
	ASNumber   int            `json:"as_number"`
	Autnum     *AutnumObject  `json:"autnum"`
	Routes     RoutePage      `json:"routes"`
	Enrichment *ASNEnrichment `json:"enrichment,omitempty"`
}

type SearchResponse struct {
	Query      string `json:"query"`
	Type       string `json:"type"`
	Normalized string `json:"normalized"`
	Endpoint   string `json:"endpoint"`
}

type PrefixEnrichment struct {
	RDAP          *RDAPNetwork   `json:"rdap,omitempty"`
	RoutingStatus *RoutingStatus `json:"routing_status,omitempty"`
}

type ASNEnrichment struct {
	RoutingStatus *ASNRoutingStatus `json:"routing_status,omitempty"`
}

type RDAPNetwork struct {
	Handle      nullableString `json:"handle"`
	Name        nullableString `json:"name"`
	Type        nullableString `json:"type"`
	StartIP     nullableString `json:"start_ip"`
	EndIP       nullableString `json:"end_ip"`
	CountryCode nullableString `json:"country_code"`
	Status      []string       `json:"status"`
	Created     nullableString `json:"created"`
	LastChanged nullableString `json:"last_changed"`
}

type RoutingStatus struct {
	FirstSeen nullableString `json:"first_seen"`
	LastSeen  nullableString `json:"last_seen"`
	Origins   []string       `json:"origins"`
}

type ASNRoutingStatus struct {
	Holder    nullableString `json:"holder"`
	Announced *bool          `json:"announced"`
}
