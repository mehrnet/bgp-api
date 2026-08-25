package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

type indexDefinition struct {
	name      string
	statement string
}

func main() {
	databaseURL := flag.String("database-url", "", "PostgreSQL connection URL")
	schema := flag.String("schema", "", "dataset schema")
	output := flag.String("output", "", "optional psql index script output path")
	workers := flag.Int("workers", 2, "maximum concurrent index builds")
	flag.Parse()
	if !identifier.MatchString(*schema) || (*databaseURL == "" && *output == "") || *workers < 1 {
		log.Fatal("usage: build-postgres-indexes -schema bgp_... [-database-url URL] [-output indexes.sql] [-workers 2]")
	}

	definitions := indexDefinitions(*schema)
	if *output != "" {
		if err := os.WriteFile(*output, []byte(renderIndexSQL(definitions, *schema)), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	if *databaseURL != "" {
		if err := buildIndexes(context.Background(), *databaseURL, definitions, *schema, *workers); err != nil {
			log.Fatal(err)
		}
	}
}

func indexDefinitions(schema string) []indexDefinition {
	quoted := quoteIdentifier(schema)
	return []indexDefinition{
		{"idx_allocation_objects_overlap", fmt.Sprintf("CREATE INDEX idx_allocation_objects_overlap ON %s.allocation_objects USING GIST (numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]'))", quoted)},
		{"idx_route_objects_overlap", fmt.Sprintf("CREATE INDEX idx_route_objects_overlap ON %s.route_objects USING GIST (numrange(start_ip_sort::numeric, end_ip_sort::numeric, '[]'))", quoted)},
		{"idx_lookup_prefix", fmt.Sprintf("CREATE INDEX idx_lookup_prefix ON %s.lookup_prefixes (source, prefix_key)", quoted)},
		{"idx_route_objects_prefix", fmt.Sprintf("CREATE INDEX idx_route_objects_prefix ON %s.route_objects USING SPGIST (prefix inet_ops)", quoted)},
		{"idx_route_objects_asn_id", fmt.Sprintf("CREATE INDEX idx_route_objects_asn_id ON %s.route_objects (asn_number, id) WHERE asn_number IS NOT NULL", quoted)},
		{"idx_autnums_asn_number", fmt.Sprintf("CREATE INDEX idx_autnums_asn_number ON %s.autnums (asn_number) WHERE asn_number IS NOT NULL", quoted)},
	}
}

func buildIndexes(ctx context.Context, databaseURL string, definitions []indexDefinition, schema string, workerCount int) error {
	if workerCount > len(definitions) {
		workerCount = len(definitions)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan indexDefinition, len(definitions))
	for _, definition := range definitions {
		jobs <- definition
	}
	close(jobs)

	var wg sync.WaitGroup
	var firstErr error
	var errorMu sync.Mutex
	recordError := func(err error) {
		errorMu.Lock()
		defer errorMu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}

	for worker := 1; worker <= workerCount; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			conn, err := pgx.Connect(ctx, databaseURL)
			if err != nil {
				recordError(fmt.Errorf("index worker %d connect: %w", worker, err))
				return
			}
			defer conn.Close(context.Background())
			if _, err := conn.Exec(ctx, "SET synchronous_commit = off; SET maintenance_work_mem = '768MB'"); err != nil {
				recordError(fmt.Errorf("index worker %d configure: %w", worker, err))
				return
			}
			for definition := range jobs {
				if ctx.Err() != nil {
					return
				}
				started := time.Now()
				log.Printf("index worker %d building %s", worker, definition.name)
				if _, err := conn.Exec(ctx, definition.statement); err != nil {
					recordError(fmt.Errorf("build %s: %w", definition.name, err))
					return
				}
				log.Printf("index worker %d built %s in %s", worker, definition.name, time.Since(started).Round(time.Second))
			}
		}(worker)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect for analyze: %w", err)
	}
	defer conn.Close(context.Background())
	for _, table := range analyzedTables() {
		if _, err := conn.Exec(ctx, fmt.Sprintf("ANALYZE %s.%s", quoteIdentifier(schema), table)); err != nil {
			return fmt.Errorf("analyze %s: %w", table, err)
		}
	}
	return nil
}

func renderIndexSQL(definitions []indexDefinition, schema string) string {
	var output strings.Builder
	output.WriteString("\\if :{?skip_indexes}\n")
	output.WriteString("\\echo 'Skipping PostgreSQL indexes because skip_indexes is set'\n")
	output.WriteString("\\else\n")
	for _, definition := range definitions {
		fmt.Fprintf(&output, "%s;\n", definition.statement)
	}
	for _, table := range analyzedTables() {
		fmt.Fprintf(&output, "ANALYZE %s.%s;\n", quoteIdentifier(schema), table)
	}
	output.WriteString("\\endif\n")
	return output.String()
}

func analyzedTables() []string {
	return []string{"lookup_prefixes", "allocation_objects", "route_objects", "autnums", "range_summaries"}
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
