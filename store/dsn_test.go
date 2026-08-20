package store

import (
	"strings"
	"testing"
)

func TestRedactDSNMasksPostgresPassword(t *testing.T) {
	got := RedactDSN("postgres://claudemem:supersecretpw@localhost:55432/claudemem?sslmode=disable")
	if strings.Contains(got, "supersecretpw") {
		t.Fatalf("RedactDSN leaked the password: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("RedactDSN = %q, want a REDACTED marker in place of the password", got)
	}
	if !strings.Contains(got, "claudemem") || !strings.Contains(got, "localhost:55432") {
		t.Fatalf("RedactDSN = %q, want the non-secret parts (user, host, db) preserved for debuggability", got)
	}
}

func TestRedactDSNLeavesSQLiteFilePathUnchanged(t *testing.T) {
	path := "/Users/x/.claude-mem-go/observations.db"
	if got := RedactDSN(path); got != path {
		t.Fatalf("RedactDSN(%q) = %q, want it unchanged — a plain file path has no credentials", path, got)
	}
}

func TestRedactDSNHandlesNoPasswordWithoutPanicking(t *testing.T) {
	dsn := "postgres://claudemem@localhost:55432/claudemem"
	got := RedactDSN(dsn)
	if strings.Contains(got, "REDACTED") {
		t.Fatalf("RedactDSN(%q) = %q, want it unchanged — there's no password to redact", dsn, got)
	}
}

func TestRedactDSNHandlesMalformedInputWithoutPanicking(t *testing.T) {
	// The point of this test is that it doesn't panic — RedactDSN sits on
	// every path that logs a -db flag value, so it must never be the thing
	// that crashes a log call.
	for _, bad := range []string{"", "not a url at all", "postgres://", "://x"} {
		_ = RedactDSN(bad)
	}
}

// TestRedactDSNRedactsEvenWhenTheRestOfTheDSNIsMalformed is the real
// regression: an earlier net/url-based implementation fell back to
// returning the ORIGINAL, unredacted string whenever url.Parse failed —
// exactly backwards for a redaction function, and a malformed host after
// a well-formed user:password@ prefix triggered it immediately.
func TestRedactDSNRedactsEvenWhenTheRestOfTheDSNIsMalformed(t *testing.T) {
	dsn := "postgres://claudemem:mysupersecretpassword@[invalid host/claudemem"
	got := RedactDSN(dsn)
	if strings.Contains(got, "mysupersecretpassword") {
		t.Fatalf("RedactDSN leaked the password from a DSN with a malformed host: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("RedactDSN(%q) = %q, want a REDACTED marker", dsn, got)
	}
}
