package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mehrnet/bgp-api/internal/ipkey"
)

const maxCandidates = 64

type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (repository *PostgresRepository) Lookup(ctx context.Context, ip ipkey.Parsed, options LookupOptions) (*LookupResponse, error) {
	keys := ipkey.PrefixKeysForIP(ip)
	batch := &pgx.Batch{}
	for _, source := range []string{"allocation", "route", "geofeed"} {
		batch.Queue(`
			SELECT start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city,
			       status, allocation_date, created, last_modified, record_source, mnt_by, org,
			       to_jsonb(lookup_prefixes)->>'abuse_contact', description
			FROM lookup_prefixes
			WHERE source = $1 AND prefix_key = ANY($2::text[])
			ORDER BY prefix_length DESC
			LIMIT $3
		`, source, keys, maxCandidates)
	}
	results := repository.pool.SendBatch(ctx, batch)
	defer results.Close()

	allocations, err := readCandidates(results)
	if err != nil {
		return nil, err
	}
	routes, err := readCandidates(results)
	if err != nil {
		return nil, err
	}
	geolocations, err := readCandidates(results)
	if err != nil {
		return nil, err
	}
	return BuildResponse(ip, allocations, routes, geolocations, options), nil
}

func readCandidates(results pgx.BatchResults) ([]LookupCandidate, error) {
	rows, err := results.Query()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]LookupCandidate, 0)
	for rows.Next() {
		candidate := LookupCandidate{}
		if err := rows.Scan(
			&candidate.StartIPSort,
			&candidate.EndIPSort,
			&candidate.IPVersion,
			&candidate.Registry,
			&candidate.Country,
			&candidate.Netname,
			&candidate.CIDR,
			&candidate.ASN,
			&candidate.Region,
			&candidate.City,
			&candidate.Status,
			&candidate.AllocationDate,
			&candidate.Created,
			&candidate.LastModified,
			&candidate.RecordSource,
			&candidate.MntBy,
			&candidate.Org,
			&candidate.AbuseContact,
			&candidate.Description,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read lookup candidates: %w", err)
	}
	return candidates, nil
}

func (repository *PostgresRepository) LookupPrefix(ctx context.Context, prefix ipkey.ParsedPrefix, page Page) (*PrefixResponse, error) {
	allocation, err := repository.prefixAllocation(ctx, prefix)
	if err != nil {
		return nil, err
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT id, prefix::text, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org,
		       to_jsonb(route_objects)->>'abuse_contact', description,
		       CASE WHEN prefix = $1::cidr THEN 'exact' WHEN prefix >>= $1::cidr THEN 'covering' ELSE 'more_specific' END
		FROM route_objects
		WHERE prefix && $1::cidr AND id > $2
		ORDER BY id
		LIMIT $3
	`, prefix.Canonical, page.Cursor, page.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("query route objects by prefix: %w", err)
	}
	routes, err := readRouteObjects(rows)
	if err != nil {
		return nil, err
	}
	routes, next := routePage(routes, page.Limit)
	return &PrefixResponse{
		Prefix:     prefixDescriptor(prefix),
		Allocation: allocation,
		Routes:     RoutePage{Items: routes, NextCursor: next},
	}, nil
}

func (repository *PostgresRepository) LookupRange(ctx context.Context, rangeValue ipkey.ParsedRange, kind RangeKind, page Page) (*RangeResponse, error) {
	response := &RangeResponse{Range: rangeDescriptor(rangeValue), Kind: kind, Mode: "records"}
	if kind == RangeAllocations {
		rows, err := repository.pool.Query(ctx, `
			WITH overlap AS MATERIALIZED (
				SELECT id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date,
				       created, last_modified, record_source, mnt_by, org, to_jsonb(allocation_objects)->>'abuse_contact' AS abuse_contact, description
				FROM allocation_objects
				WHERE ip_version = $1
				  AND numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]')
				      && numrange($2::numeric, $3::numeric, '[]')
			)
			SELECT id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date,
			       created, last_modified, record_source, mnt_by, org, abuse_contact, description
			FROM overlap
			WHERE id > $4
			ORDER BY id
			LIMIT $5
		`, rangeValue.Version, rangeValue.Start.SortKey, rangeValue.End.SortKey, page.Cursor, page.Limit+1)
		if err != nil {
			return nil, fmt.Errorf("query allocation objects by range: %w", err)
		}
		allocations, err := readAllocationObjects(rows)
		if err != nil {
			return nil, err
		}
		response.Allocations, response.NextCursor = allocationPage(allocations, page.Limit)
		return response, nil
	}

	rows, err := repository.pool.Query(ctx, `
		WITH overlap AS MATERIALIZED (
			SELECT id, prefix::text AS prefix, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org,
			       to_jsonb(route_objects)->>'abuse_contact' AS abuse_contact, description
			FROM route_objects
			WHERE ip_version = $1
			  AND numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]')
			      && numrange($2::numeric, $3::numeric, '[]')
		)
		SELECT id, prefix, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org,
		       abuse_contact, description, ''
		FROM overlap
		WHERE id > $4
		ORDER BY id
		LIMIT $5
	`, rangeValue.Version, rangeValue.Start.SortKey, rangeValue.End.SortKey, page.Cursor, page.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("query route objects by range: %w", err)
	}
	routes, err := readRouteObjects(rows)
	if err != nil {
		return nil, err
	}
	response.Routes, response.NextCursor = routePage(routes, page.Limit)
	return response, nil
}

func (repository *PostgresRepository) LookupRangeSummary(ctx context.Context, rangeValue ipkey.ParsedRange, kind RangeKind) (*RangeResponse, error) {
	keys, ok := ipkey.SummaryPrefixKeys(rangeValue)
	if !ok {
		return nil, fmt.Errorf("range is not eligible for a generated summary")
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT prefix_length, allocation_records, route_records, countries::text, asns::text
		FROM range_summaries
		WHERE cidr = ANY($1::cidr[])
	`, keys)
	if err != nil {
		return nil, fmt.Errorf("query generated range summaries: %w", err)
	}
	defer rows.Close()

	response := &RangeResponse{Range: rangeDescriptor(rangeValue), Kind: kind, Mode: "summary"}
	summary := &RangeSummary{Aggregation: "overlapping_source_records", BucketPrefixLength: summaryBucketLength(keys[0]), Buckets: len(keys), Countries: []RangeFacet{}, ASNs: []RangeFacet{}}
	countries := make(map[string]int64)
	asns := make(map[string]int64)
	for rows.Next() {
		var bucketLength int
		var allocationRecords, routeRecords int64
		var rawCountries, rawASNs string
		if err := rows.Scan(&bucketLength, &allocationRecords, &routeRecords, &rawCountries, &rawASNs); err != nil {
			return nil, fmt.Errorf("read generated range summary: %w", err)
		}
		if summary.BucketPrefixLength == 0 {
			summary.BucketPrefixLength = bucketLength
		}
		summary.AllocationRecords += allocationRecords
		summary.RouteRecords += routeRecords
		if err := mergeFacets(countries, rawCountries); err != nil {
			return nil, err
		}
		if err := mergeFacets(asns, rawASNs); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate generated range summaries: %w", err)
	}
	summary.Countries = rankFacets(countries)
	summary.ASNs = rankFacets(asns)
	response.Summary = summary
	return response, nil
}

func mergeFacets(target map[string]int64, raw string) error {
	items := []RangeFacet{}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return fmt.Errorf("decode generated range facets: %w", err)
	}
	for _, item := range items {
		target[item.Value] += item.RecordCount
	}
	return nil
}

func rankFacets(values map[string]int64) []RangeFacet {
	items := make([]RangeFacet, 0, len(values))
	for value, count := range values {
		items = append(items, RangeFacet{Value: value, RecordCount: count})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].RecordCount != items[right].RecordCount {
			return items[left].RecordCount > items[right].RecordCount
		}
		return items[left].Value < items[right].Value
	})
	if len(items) > 10 {
		return items[:10]
	}
	return items
}

func summaryBucketLength(key string) int {
	index := strings.LastIndexByte(key, '/')
	if index == -1 {
		return 0
	}
	length, _ := strconv.Atoi(key[index+1:])
	return length
}

func (repository *PostgresRepository) LookupASN(ctx context.Context, asn uint32, page Page) (*ASNResponse, error) {
	autnum, err := repository.autnum(ctx, asn)
	if err != nil {
		return nil, err
	}
	if page.Numbered {
		return repository.lookupASNPage(ctx, asn, autnum, page)
	}
	rows, err := repository.pool.Query(ctx, `
		WITH page AS MATERIALIZED (
			SELECT id
			FROM route_objects
			WHERE asn_number = $1 AND id > $2
			ORDER BY asn_number, id
			LIMIT $3
		)
		SELECT route_objects.id, prefix::text, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org,
		       to_jsonb(route_objects)->>'abuse_contact', description, ''
		FROM route_objects
		JOIN page USING (id)
		ORDER BY route_objects.id
	`, asn, page.Cursor, page.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("query route objects by ASN: %w", err)
	}
	routes, err := readRouteObjects(rows)
	if err != nil {
		return nil, err
	}
	if autnum == nil && len(routes) == 0 {
		return nil, nil
	}
	routes, next := routePage(routes, page.Limit)
	return &ASNResponse{
		ASN:      "AS" + strconv.FormatUint(uint64(asn), 10),
		ASNumber: int(asn),
		Autnum:   autnum,
		Routes:   RoutePage{Items: routes, NextCursor: next},
	}, nil
}

func (repository *PostgresRepository) lookupASNPage(ctx context.Context, asn uint32, autnum *AutnumObject, page Page) (*ASNResponse, error) {
	var total int64
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM route_objects WHERE asn_number = $1`, asn).Scan(&total); err != nil {
		return nil, fmt.Errorf("count route objects by ASN: %w", err)
	}
	if autnum == nil && total == 0 {
		return nil, nil
	}
	offset := int64(page.Number-1) * int64(page.Limit)
	rows, err := repository.pool.Query(ctx, `
		SELECT id, prefix::text, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org,
		       to_jsonb(route_objects)->>'abuse_contact', description, ''
		FROM route_objects
		WHERE asn_number = $1
		ORDER BY id
		OFFSET $2
		LIMIT $3
	`, asn, offset, page.Limit)
	if err != nil {
		return nil, fmt.Errorf("query numbered route objects by ASN: %w", err)
	}
	routes, err := readRouteObjects(rows)
	if err != nil {
		return nil, err
	}
	totalPages := int((total + int64(page.Limit) - 1) / int64(page.Limit))
	return &ASNResponse{
		ASN:      "AS" + strconv.FormatUint(uint64(asn), 10),
		ASNumber: int(asn),
		Autnum:   autnum,
		Routes: RoutePage{
			Items: routes, Page: page.Number, TotalPages: totalPages, TotalItems: total,
		},
	}, nil
}

func (repository *PostgresRepository) prefixAllocation(ctx context.Context, prefix ipkey.ParsedPrefix) (*AllocationObject, error) {
	row := repository.pool.QueryRow(ctx, `
		SELECT id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date,
		       created, last_modified, record_source, mnt_by, org, to_jsonb(allocation_objects)->>'abuse_contact', description
		FROM allocation_objects
		WHERE ip_version = $1
		  AND numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]')
		      @> numrange($2::numeric, $3::numeric, '[]')
		ORDER BY start_ip_sort DESC, end_ip_sort ASC, id
		LIMIT 1
	`, prefix.Version, prefix.Start.SortKey, prefix.End.SortKey)
	allocation, err := scanAllocationObject(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query allocation object by prefix: %w", err)
	}
	return &allocation, nil
}

func (repository *PostgresRepository) autnum(ctx context.Context, asn uint32) (*AutnumObject, error) {
	row := repository.pool.QueryRow(ctx, `
		SELECT asn, asn_number, registry, country, as_name, org, status, created, last_modified, record_source, mnt_by,
		       to_jsonb(autnums)->>'abuse_contact', description
		FROM autnums
		WHERE asn_number = $1
		ORDER BY id
		LIMIT 1
	`, asn)
	object := AutnumObject{}
	var asnNumber int64
	if err := row.Scan(
		&object.ASN, &asnNumber, &object.Registry, &object.CountryRaw, &object.Name, &object.Organization,
		&object.Status, &object.Created, &object.LastModified, &object.Source, &object.Maintainers, &object.AbuseContact, &object.Description,
	); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("query autnum: %w", err)
	}
	object.ASNumber = int(asnNumber)
	object.CountryRaw = upper(object.CountryRaw)
	object.CountryCode = countryCode(object.CountryRaw)
	object.Registry = lower(object.Registry)
	object.Name = present(object.Name)
	object.Organization = present(object.Organization)
	object.Status = present(object.Status)
	object.Created = present(object.Created)
	object.LastModified = present(object.LastModified)
	object.Source = present(object.Source)
	object.Maintainers = present(object.Maintainers)
	object.AbuseContact = present(object.AbuseContact)
	object.Description = present(object.Description)
	return &object, nil
}

type rowScanner interface {
	Scan(...any) error
}

func readRouteObjects(rows pgx.Rows) ([]RouteObject, error) {
	defer rows.Close()
	objects := make([]RouteObject, 0)
	for rows.Next() {
		object := RouteObject{}
		var asnNumber *int64
		if err := rows.Scan(
			&object.ID, &object.Prefix, &object.Version, &object.OriginASN, &asnNumber, &object.Registry,
			&object.Source, &object.Maintainers, &object.Organization, &object.AbuseContact, &object.Description, &object.Relation,
		); err != nil {
			return nil, fmt.Errorf("read route object: %w", err)
		}
		if asnNumber != nil {
			value := int(*asnNumber)
			object.ASNumber = &value
		}
		object.Registry = lower(object.Registry)
		object.Source = present(object.Source)
		object.Maintainers = present(object.Maintainers)
		object.Organization = present(object.Organization)
		object.AbuseContact = present(object.AbuseContact)
		object.Description = present(object.Description)
		object.Relation = strings.TrimSpace(object.Relation)
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read route objects: %w", err)
	}
	return objects, nil
}

func readAllocationObjects(rows pgx.Rows) ([]AllocationObject, error) {
	defer rows.Close()
	objects := make([]AllocationObject, 0)
	for rows.Next() {
		object, err := scanAllocationObject(rows)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read allocation objects: %w", err)
	}
	return objects, nil
}

func scanAllocationObject(row rowScanner) (AllocationObject, error) {
	object := AllocationObject{}
	if err := row.Scan(
		&object.ID, &object.StartIP, &object.EndIP, &object.Version, &object.Registry, &object.CountryRaw, &object.Name,
		&object.Status, &object.AllocationDate, &object.Created, &object.LastModified, &object.Source,
		&object.Maintainers, &object.Organization, &object.AbuseContact, &object.Description,
	); err != nil {
		return AllocationObject{}, fmt.Errorf("read allocation object: %w", err)
	}
	object.StartIP = ipkey.SortKeyToIP(object.StartIP, object.Version)
	object.EndIP = ipkey.SortKeyToIP(object.EndIP, object.Version)
	object.Registry = lower(object.Registry)
	object.CountryRaw = upper(object.CountryRaw)
	object.CountryCode = countryCode(object.CountryRaw)
	object.Name = present(object.Name)
	object.Status = present(object.Status)
	object.AllocationDate = present(object.AllocationDate)
	object.Created = present(object.Created)
	object.LastModified = present(object.LastModified)
	object.Source = present(object.Source)
	object.Maintainers = present(object.Maintainers)
	object.Organization = present(object.Organization)
	object.AbuseContact = present(object.AbuseContact)
	object.Description = present(object.Description)
	return object, nil
}

func (repository *PostgresRepository) DatasetMetadata(ctx context.Context) (*DatasetMetadata, error) {
	row := repository.pool.QueryRow(ctx, `
		SELECT
			release_tag,
			to_jsonb(dataset)->>'built_at',
			activated_at::text,
			to_jsonb(dataset)->>'source_commit'
		FROM public.bgp_api_dataset AS dataset
		WHERE singleton
	`)
	metadata := &DatasetMetadata{}
	if err := row.Scan(&metadata.ReleaseTag, &metadata.BuiltAt, &metadata.ActivatedAt, &metadata.SourceCommit); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("query dataset metadata: %w", err)
	}
	metadata.ReleaseTag = present(metadata.ReleaseTag)
	metadata.BuiltAt = present(metadata.BuiltAt)
	metadata.ActivatedAt = present(metadata.ActivatedAt)
	metadata.SourceCommit = present(metadata.SourceCommit)
	return metadata, nil
}

func routePage(items []RouteObject, limit int) ([]RouteObject, nullableString) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	cursor := strconv.FormatInt(items[len(items)-1].ID, 10)
	return items, &cursor
}

func allocationPage(items []AllocationObject, limit int) ([]AllocationObject, nullableString) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	cursor := strconv.FormatInt(items[len(items)-1].ID, 10)
	return items, &cursor
}

func prefixDescriptor(prefix ipkey.ParsedPrefix) PrefixDescriptor {
	return PrefixDescriptor{
		CIDR: prefix.Canonical, Version: prefix.Version, PrefixLength: prefix.PrefixLength,
		StartIP: prefix.Start.Canonical, EndIP: prefix.End.Canonical, AddressCount: prefix.AddressCount,
	}
}

func rangeDescriptor(rangeValue ipkey.ParsedRange) RangeDescriptor {
	return RangeDescriptor{
		StartIP: rangeValue.Start.Canonical, EndIP: rangeValue.End.Canonical,
		Version: rangeValue.Version, AddressCount: rangeValue.AddressCount,
	}
}
