package api

import (
	"context"
	"fmt"

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
			SELECT start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city
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
