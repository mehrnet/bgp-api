package api

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
	AbuseContact nullableString `json:"abuse_contact"`
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
	AbuseContact   nullableString `json:"abuse_contact"`
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
	AbuseContact nullableString `json:"abuse_contact"`
	Description  nullableString `json:"description"`
}

type RoutePage struct {
	Items      []RouteObject  `json:"items"`
	NextCursor nullableString `json:"next_cursor"`
}

type PrefixResponse struct {
	Meta       *ResponseMeta     `json:"meta,omitempty"`
	Prefix     PrefixDescriptor  `json:"prefix"`
	Allocation *AllocationObject `json:"allocation"`
	Routes     RoutePage         `json:"routes"`
}

type RangeResponse struct {
	Meta        *ResponseMeta      `json:"meta,omitempty"`
	Range       RangeDescriptor    `json:"range"`
	Kind        RangeKind          `json:"kind"`
	Allocations []AllocationObject `json:"allocations,omitempty"`
	Routes      []RouteObject      `json:"routes,omitempty"`
	NextCursor  nullableString     `json:"next_cursor"`
}

type ASNResponse struct {
	Meta     *ResponseMeta `json:"meta,omitempty"`
	ASN      string        `json:"asn"`
	ASNumber int           `json:"as_number"`
	Autnum   *AutnumObject `json:"autnum"`
	Routes   RoutePage     `json:"routes"`
}
