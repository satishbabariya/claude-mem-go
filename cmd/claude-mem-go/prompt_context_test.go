package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateForLogIsRuneSafe: the preview must never split a multi-byte
// character. 79 ASCII bytes followed by a 3-byte rune straddles the
// 80-byte cut exactly.
func TestTruncateForLogIsRuneSafe(t *testing.T) {
	s := strings.Repeat("a", 79) + "日本語"
	got := truncateForLog(s)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateForLog produced invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", 79) + "…"; got != want {
		t.Fatalf("truncateForLog = %q, want %q (cut back to the rune boundary)", got, want)
	}
	if short := "short prompt"; truncateForLog(short) != short {
		t.Fatalf("truncateForLog altered a prompt under the cap")
	}
}
