package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestPingWithRetryBoundsAHangingConnection is the real regression test
// for the gap this closes: pingWithRetry used to call db.PingContext(ctx)
// with the caller's own context directly, and every real caller in this
// project passes one with no deadline of its own (see connectRetryBackoff's
// doc comment). A connection that completes the TCP handshake but never
// answers Postgres's startup packet — the realistic shape of a
// firewalled/black-holed host, simulated here with a real TCP listener
// that accepts and then goes silent — used to hang each ping attempt
// indefinitely, bounded by nothing at all. Confirms every attempt is now
// bounded by connectionTimeout(), not the caller's own (often absent)
// deadline.
func TestPingWithRetryBoundsAHangingConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and hold the connection open, never writing anything
			// back — Postgres's real startup handshake never completes,
			// so any read waiting on it blocks until something external
			// (a context deadline) cuts it off.
			t.Cleanup(func() { conn.Close() })
		}
	}()

	t.Setenv(connectionTimeoutEnvVar, "200")
	dsn := fmt.Sprintf("postgres://user:pass@%s/db?sslmode=disable", ln.Addr())
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	shortBackoff := []time.Duration{0, 10 * time.Millisecond}
	start := time.Now()
	err = pingWithRetry(context.Background(), db, shortBackoff)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("pingWithRetry against a connection that never answers the startup packet: want an error, got nil")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("pingWithRetry against a hanging connection took %s — want it bounded near 2 attempts x 200ms (connectionTimeoutEnvVar) plus backoff spacing, not left to hang on the caller's own undeadlined context", elapsed)
	}
}

func TestConnectionTimeoutDefaultsAndEnvOverride(t *testing.T) {
	if got, want := connectionTimeout(), time.Duration(defaultConnectionTimeoutMS)*time.Millisecond; got != want {
		t.Fatalf("connectionTimeout() with no env override = %s, want %s (defaultConnectionTimeoutMS)", got, want)
	}

	t.Setenv(connectionTimeoutEnvVar, "1234")
	if got, want := connectionTimeout(), 1234*time.Millisecond; got != want {
		t.Fatalf("connectionTimeout() with %s=1234 = %s, want %s", connectionTimeoutEnvVar, got, want)
	}

	t.Setenv(connectionTimeoutEnvVar, "not-a-number")
	if got, want := connectionTimeout(), time.Duration(defaultConnectionTimeoutMS)*time.Millisecond; got != want {
		t.Fatalf("connectionTimeout() with a garbage env var = %s, want it to fall back to the %s default", got, want)
	}
}
