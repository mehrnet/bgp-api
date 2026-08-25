package main

import (
	"strings"
	"testing"
)

func TestRenderIndexSQLSupportsIndexedAndUnindexedRestores(t *testing.T) {
	schema := "bgp_20260825_1345"
	definitions := indexDefinitions(schema)
	output := renderIndexSQL(definitions, schema)

	for _, expected := range []string{
		`\if :{?skip_indexes}`,
		`\else`,
		`CREATE INDEX idx_lookup_prefix ON "bgp_20260825_1345".lookup_prefixes`,
		`CREATE INDEX idx_allocation_objects_overlap ON "bgp_20260825_1345".allocation_objects`,
		`ANALYZE "bgp_20260825_1345".range_summaries;`,
		`\endif`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("index script does not contain %q:\n%s", expected, output)
		}
	}
	if count := strings.Count(output, "CREATE INDEX "); count != len(definitions) {
		t.Fatalf("index script has %d CREATE INDEX statements, want %d", count, len(definitions))
	}
}
