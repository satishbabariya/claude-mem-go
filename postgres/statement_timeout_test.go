package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestWithStatementTimeout covers withStatementTimeout's pure logic —
// no container needed for these cases, unlike TestPostgresOpenApplies
// StatementTimeout below, which confirms Postgres itself actually
// enforces the value this function adds.
func TestWithStatementTimeout(t *testing.T) {
	t.Run("appends the default when the DSN has none", func(t *testing.T) {
		got := withStatementTimeout("postgres://user:pass@host:5432/db?sslmode=disable")
		if !strings.Contains(got, "statement_timeout=30000") {
			t.Fatalf("withStatementTimeout(...) = %q, want it to contain statement_timeout=30000", got)
		}
	})

	t.Run("leaves an explicit statement_timeout untouched", func(t *testing.T) {
		in := "postgres://user:pass@host:5432/db?statement_timeout=5000"
		got := withStatementTimeout(in)
		if got != in {
			t.Fatalf("withStatementTimeout(%q) = %q, want it unchanged (operator's own explicit value must win)", in, got)
		}
	})

	t.Run("respects the env var override", func(t *testing.T) {
		t.Setenv(statementTimeoutEnvVar, "12345")
		got := withStatementTimeout("postgres://user:pass@host:5432/db")
		if !strings.Contains(got, "statement_timeout=12345") {
			t.Fatalf("withStatementTimeout(...) with %s=12345 = %q, want it to contain statement_timeout=12345", statementTimeoutEnvVar, got)
		}
	})

	t.Run("ignores a garbage env var and falls back to the default", func(t *testing.T) {
		t.Setenv(statementTimeoutEnvVar, "not-a-number")
		got := withStatementTimeout("postgres://user:pass@host:5432/db")
		if !strings.Contains(got, "statement_timeout=30000") {
			t.Fatalf("withStatementTimeout(...) with a garbage env var = %q, want it to fall back to the 30000 default", got)
		}
	})

	t.Run("returns an unparseable DSN unchanged rather than erroring", func(t *testing.T) {
		in := "postgres://user:pass@[invalid host/db"
		if got := withStatementTimeout(in); got != in {
			t.Fatalf("withStatementTimeout(%q) = %q, want it returned unchanged so the real connection failure surfaces on its own", in, got)
		}
	})
}

// TestPostgresOpenAppliesStatementTimeout is the real, live-container
// regression test for a real gap: this backend bounded MaxOpenConns (10,
// shared by every hook process and the worker daemon) but never bounded
// how long any single query on those 10 connections could run — a
// hung query (lock contention, a pathological plan, a network stall)
// held its connection forever, and enough of them exhausted the whole
// pool with no self-healing. Confirms a real SELECT pg_sleep(10) against
// a Store opened with a short statement_timeout actually gets canceled
// by Postgres itself near that timeout, not left running for the full
// 10 seconds — the exact failure mode this closes.
func TestPostgresOpenAppliesStatementTimeout(t *testing.T) {
	// Via the env var, not an explicit DSN param: this exercises Open's
	// OWN withStatementTimeout call directly, rather than a DSN that
	// already carries the setting regardless of whether Open ever wires
	// it in at all.
	t.Setenv(statementTimeoutEnvVar, "1500")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := Open(ctx, requireTestDSN(t), DefaultEmbedDims, 0)
	if err != nil {
		t.Skipf("postgres not reachable (start it with `docker compose up -d`): %v", err)
	}
	defer st.Close()

	start := time.Now()
	_, err = st.db.Exec("SELECT pg_sleep(10)")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("SELECT pg_sleep(10) succeeded, want a statement-timeout error from the 1.5s timeout")
	}
	if !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("query error = %v, want Postgres's own \"canceling statement due to statement timeout\"", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("query took %s to error, want well under the 10s sleep — the 1.5s statement_timeout should have canceled it quickly, not let it run to completion", elapsed)
	}

	// The real failure mode this closes isn't just "one query hangs" —
	// it's "a hung query's connection never comes back, and enough of
	// them exhaust the whole (10-connection) pool." Confirm the pool is
	// still healthy immediately after the timeout: a trivial query on
	// the same Store must succeed quickly, not queue behind a
	// connection that never got released.
	quickStart := time.Now()
	var one int
	if err := st.db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1 after the timed-out query failed: %v — the pool looks wedged, not recovered", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d, want 1", one)
	}
	if quickElapsed := time.Since(quickStart); quickElapsed > 2*time.Second {
		t.Fatalf("SELECT 1 after the timed-out query took %s, want near-instant — the pool should have a free connection available", quickElapsed)
	}
}
