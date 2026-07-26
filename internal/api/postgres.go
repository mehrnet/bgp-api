package api

import (
	"context"
	"fmt"
	"strconv"

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

func (repository *PostgresRepository) Lookup(ctx context.Context, ip ipkey.Parsed) (*LookupResponse, error) {
	keys := ipkey.PrefixKeysForIP(ip)
	batch := &pgx.Batch{}
	for _, source := range []string{"allocation", "route", "geofeed"} {
		batch.Queue(`
			SELECT start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city,
			       status, allocation_date, created, last_modified, record_source, mnt_by, org, description
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
	return BuildResponse(ip, allocations, routes, geolocations), nil
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
		SELECT id, prefix::text, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org, description,
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
	response := &RangeResponse{Range: rangeDescriptor(rangeValue), Kind: kind}
	if kind == RangeAllocations {
		rows, err := repository.pool.Query(ctx, `
			SELECT id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date,
			       created, last_modified, record_source, mnt_by, org, description
			FROM allocation_objects
			WHERE ip_version = $1 AND start_ip_sort <= $2 AND end_ip_sort >= $3 AND id > $4
			ORDER BY id
			LIMIT $5
		`, rangeValue.Version, rangeValue.End.SortKey, rangeValue.Start.SortKey, page.Cursor, page.Limit+1)
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
		SELECT id, prefix::text, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org, description, ''
		FROM route_objects
		WHERE ip_version = $1 AND start_ip_sort <= $2 AND end_ip_sort >= $3 AND id > $4
		ORDER BY id
		LIMIT $5
	`, rangeValue.Version, rangeValue.End.SortKey, rangeValue.Start.SortKey, page.Cursor, page.Limit+1)
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

func (repository *PostgresRepository) LookupASN(ctx context.Context, asn uint32, page Page) (*ASNResponse, error) {
	autnum, err := repository.autnum(ctx, asn)
	if err != nil {
		return nil, err
	}
	rows, err := repository.pool.Query(ctx, `
		SELECT id, prefix::text, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org, description, ''
		FROM route_objects
		WHERE asn_number = $1 AND id > $2
		ORDER BY id
		LIMIT $3
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

func (repository *PostgresRepository) prefixAllocation(ctx context.Context, prefix ipkey.ParsedPrefix) (*AllocationObject, error) {
	row := repository.pool.QueryRow(ctx, `
		SELECT id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date,
		       created, last_modified, record_source, mnt_by, org, description
		FROM allocation_objects
		WHERE ip_version = $1 AND start_ip_sort <= $2 AND end_ip_sort >= $3
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
		SELECT asn, asn_number, registry, country, as_name, org, status, created, last_modified, record_source, mnt_by, description
		FROM autnums
		WHERE asn_number = $1
		ORDER BY id
		LIMIT 1
	`, asn)
	object := AutnumObject{}
	var asnNumber int64
	if err := row.Scan(
		&object.ASN, &asnNumber, &object.Registry, &object.CountryRaw, &object.Name, &object.Organization,
		&object.Status, &object.Created, &object.LastModified, &object.Source, &object.Maintainers, &object.Description,
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
			&object.Source, &object.Maintainers, &object.Organization, &object.Description, &object.Relation,
		); err != nil {
			return nil, fmt.Errorf("read route object: %w", err)
		}
		if asnNumber != nil {
			value := int(*asnNumber)
			object.ASNumber = &value
		}
		object.Registry = lower(object.Registry)
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
		&object.Maintainers, &object.Organization, &object.Description,
	); err != nil {
		return AllocationObject{}, fmt.Errorf("read allocation object: %w", err)
	}
	object.StartIP = ipkey.SortKeyToIP(object.StartIP, object.Version)
	object.EndIP = ipkey.SortKeyToIP(object.EndIP, object.Version)
	object.Registry = lower(object.Registry)
	object.CountryRaw = upper(object.CountryRaw)
	object.CountryCode = countryCode(object.CountryRaw)
	return object, nil
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
