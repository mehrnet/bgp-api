package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func main() {
	databaseURL := flag.String("database-url", "", "PostgreSQL connection URL")
	schema := flag.String("schema", "", "dataset schema")
	flag.Parse()
	if *databaseURL == "" || !identifier.MatchString(*schema) {
		log.Fatal("usage: build-postgres-indexes -database-url URL -schema bgp_...")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET synchronous_commit = off; SET maintenance_work_mem = '1GB'"); err != nil {
		log.Fatal(err)
	}
	quoted := `"` + strings.ReplaceAll(*schema, `"`, `""`) + `"`
	for _, statement := range []string{
		fmt.Sprintf("CREATE INDEX idx_lookup_prefix ON %s.lookup_prefixes (source, prefix_key)", quoted),
		fmt.Sprintf("CREATE INDEX idx_route_objects_prefix ON %s.route_objects USING SPGIST (prefix inet_ops)", quoted),
		fmt.Sprintf("CREATE INDEX idx_allocation_objects_overlap ON %s.allocation_objects USING GIST (numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]'))", quoted),
		fmt.Sprintf("CREATE INDEX idx_route_objects_overlap ON %s.route_objects USING GIST (numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]'))", quoted),
		fmt.Sprintf("CREATE INDEX idx_route_objects_asn_id ON %s.route_objects (asn_number, id) WHERE asn_number IS NOT NULL", quoted),
		fmt.Sprintf("CREATE INDEX idx_autnums_asn_number ON %s.autnums (asn_number) WHERE asn_number IS NOT NULL", quoted),
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			log.Fatal(err)
		}
	}
	for _, table := range []string{"lookup_prefixes", "allocation_objects", "route_objects", "autnums", "range_summaries"} {
		if _, err := conn.Exec(ctx, fmt.Sprintf("ANALYZE %s.%s", quoted, table)); err != nil {
			log.Fatal(err)
		}
	}
}
