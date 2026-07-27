package main

import (
	"database/sql"
	"testing"
)

func TestCopyValueNormalizesPostgresText(t *testing.T) {
	input := string([]byte{'a', 0xa8, 0x00, '\t', '\n', '\r', '\\', 'b'})
	want := "a\uFFFD\uFFFD\\t\\n\\r\\\\b"
	if got := copyValue(input); got != want {
		t.Fatalf("copyValue() = %q, want %q", got, want)
	}
}

func TestRouteObjectRowNormalizesPrefixAndASN(t *testing.T) {
	row, err := routeObjectRow([]sql.NullString{
		{String: "192.0.2.42/24", Valid: true}, {String: "start", Valid: true}, {String: "end", Valid: true}, {String: "4", Valid: true}, {String: "AS64500", Valid: true},
		{String: "TEST", Valid: true}, {String: "SOURCE", Valid: true}, {String: "MNT", Valid: true}, {String: "ORG", Valid: true}, {String: "ABUSE", Valid: true}, {String: "description", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row[0].String != "192.0.2.0/24" || row[1].String != "24" || row[6].String != "64500" || row[11].String != "ABUSE" {
		t.Fatalf("row = %#v", row)
	}
}

func TestNullableASNRejectsInvalidValues(t *testing.T) {
	if value := nullableASN(sql.NullString{String: "AS0", Valid: true}); value.Valid {
		t.Fatalf("AS0 parsed as %#v", value)
	}
}
