# Data Contract

`GET /v1/ip/:ip` returns a single, stable shape. Values unavailable in the
source data are `null`; they are never guessed.

```json
{
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
    "as_number": 13335,
    "name": "apnic-labs",
    "status": null
  },
  "allocation": {
    "start_ip": "1.1.1.0",
    "end_ip": "1.1.1.255",
    "registry": "apnic",
    "country_code": "AU",
    "country_raw": "AU",
    "name": "apnic-labs",
    "allocation_date": null,
    "status": null
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

The current producer database supplies only these fields:

| Dataset | Fields retained |
| --- | --- |
| `allocations` | IP range, IP version, RIR registry, country, RPSL `netname` |
| `routes` | IP range, IP version, route CIDR, origin ASN |
| `geolocations` | IP range, IP version, country, region, city from geofeeds |

Consequently `allocation_date` and both status fields are `null`. To populate
them, change the producer before its CSV export to retain RPSL `status`,
`created`, `last-modified`, `source`, `mnt-by`, `org`, and `descr` attributes;
for delegated statistics retain the date and allocation status columns. Add
those columns to the SQLite tables and then extend this API schema in a
backward-compatible migration.

`registry` is lowercase for API consistency. `country_raw` preserves source
values such as the RIR's non-ISO "EU # ..." pseudo-country, while
`country_code` is only set when the source is exactly an ISO-style two-letter
code.
