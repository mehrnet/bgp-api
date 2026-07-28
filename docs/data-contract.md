# Data Contract

`GET /v1/ip?query={address}` returns a single, stable shape. Values unavailable in the
source data are `null`; they are never guessed.

```json
{
  "meta": {
    "dataset": {
      "release_tag": "db-2026.07.27-0400-1",
      "built_at": "2026-07-27T04:00:00Z",
      "activated_at": "2026-07-27 05:00:00+00",
      "source_commit": "0123456789abcdef"
    }
  },
  "ip": "1.1.1.1",
  "version": 4,
  "registry": "apnic",
  "allocation_date": null,
  "allocation_status": null,
  "network": {
    "cidr": "1.1.1.0/24",
    "start_ip": "1.1.1.0",
    "end_ip": "1.1.1.255",
    "asn": "AS13335",
    "asns": ["AS13335"],
    "as_number": 13335,
    "name": "apnic-labs",
    "status": null,
    "abuse_contact": null
  },
  "allocation": {
    "start_ip": "1.1.1.0",
    "end_ip": "1.1.1.255",
    "registry": "apnic",
    "country_code": "AU",
    "country_raw": "AU",
    "name": "apnic-labs",
    "allocation_date": null,
    "status": null,
    "abuse_contact": null
  },
  "location": {
    "country_code": "AU",
    "region": null,
    "city": null
  },
  "sources": {
    "allocation": true,
    "route": true,
    "geofeed": false
  }
}
```

The producer retains both the point-lookup fields and normalized source
objects:

Every response is constructed from these imported datasets; the API performs
no runtime upstream lookups.

Successful lookup/resource responses include `meta.dataset`. `release_tag`
identifies the GitHub release, `built_at` is the producer timestamp,
`activated_at` is when the server sync made the dataset active, and
`source_commit` is the producer repository commit used for the build. These
fields may be `null` only for old local fixtures or pre-metadata datasets.

| Dataset | Fields retained |
| --- | --- |
| `allocations` | IP range, IP version, RIR registry, country, `netname`, `status`, delegated allocation date, RPSL `created`, `last-modified`, `source`, `mnt-by`, `org`, direct `abuse-c`/`abuse-mailbox`, and `descr` |
| `routes` | IP range, IP version, route CIDR, origin ASN, RIR registry, RPSL `source`, `mnt-by`, `org`, direct `abuse-c`/`abuse-mailbox`, and `descr` |
| `autnums` | RPSL `aut-num`, `as-name`, country, status, provenance, maintainer, organization, direct `abuse-c`/`abuse-mailbox`, and description |
| `geolocations` | IP range, IP version, country, region, city from geofeeds |

`GET /v1/prefix?prefix=:cidr` returns a normalized CIDR descriptor, its
covering allocation record, and cursor-paginated registered RPSL route
objects. `GET /v1/range?start=:ip&end=:ip` returns overlapping allocation
records by default; use `kind=routes` for overlapping route objects.
`GET /v1/asn/:asn` returns an `aut-num` object and the ASN's registered route
objects. These route objects are registry/IRR records, not a claim that the
prefix is visible in the global BGP table.

`registry` is lowercase for API consistency. `country_raw` preserves source
values such as the RIR's non-ISO "EU # ..." pseudo-country, while
`country_code` is only set when the source is exactly an ISO-style two-letter
code.

`network.asns` retains every origin ASN for a most-specific route. `network.asn`
and `network.as_number` are populated only when that route has one origin;
they are `null` for a multi-origin route rather than choosing an arbitrary ASN.
