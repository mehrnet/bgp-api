package main

import "testing"

func TestCopyValueNormalizesPostgresText(t *testing.T) {
	input := string([]byte{'a', 0xa8, 0x00, '\t', '\n', '\r', '\\', 'b'})
	want := "a\uFFFD\uFFFD\\t\\n\\r\\\\b"
	if got := copyValue(input); got != want {
		t.Fatalf("copyValue() = %q, want %q", got, want)
	}
}
