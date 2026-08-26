package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/mehrnet/bgp-api/internal/boltstore"
)

func main() {
	input := flag.String("input", "", "producer input directory")
	output := flag.String("output", "", "output bbolt database path")
	release := flag.String("release-tag", "", "dataset release tag")
	builtAt := flag.String("built-at", "", "dataset build timestamp")
	commit := flag.String("source-commit", "", "producer source commit")
	compact := flag.Bool("compact", true, "compact the immutable database after building")
	flag.Parse()

	stats, err := boltstore.Build(context.Background(), boltstore.BuildOptions{
		InputDir: *input, OutputPath: *output, ReleaseTag: *release,
		BuiltAt: *builtAt, SourceCommit: *commit, Compact: *compact,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("built %s: allocations=%d routes=%d geofeeds=%d autnums=%d allocation_index=%d route_index=%d geofeed_index=%d allocation_range_index=%d route_range_index=%d materialized_summaries=%d\n",
		*output, stats.Allocations, stats.Routes, stats.Geofeeds, stats.Autnums,
		stats.AllocationIndex, stats.RouteIndex, stats.GeofeedIndex,
		stats.AllocationRangeIndex, stats.RouteRangeIndex, stats.MaterializedSummary)
}
