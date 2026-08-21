package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"claude-mem-go/store"
)

// DefaultStatsPath is ~/.claude-mem-go/worker-stats.json — the daemon's
// only observability surface beyond raw per-hook log files (context.log,
// hook.log, stop.log, mcp.log, file-context.log). Those logs answer "what
// happened on this one event"; this file answers "is the daemon actually
// healthy right now" without grepping text — the gap `doctor` (and any
// future dashboard) actually wants closed.
func DefaultStatsPath() string { return filepath.Join(store.DefaultHome(), "worker-stats.json") }

// Stats is a point-in-time snapshot of the daemon's own activity.
type Stats struct {
	Processed      int64  `json:"processed"`       // turns that completed and persisted a new observation
	Duplicates     int64  `json:"duplicates"`      // turns that hit an already-persisted content_hash (not an error)
	ObserverErrors int64  `json:"observer_errors"` // the observer turn itself failed (including the one retry)
	InsertErrors   int64  `json:"insert_errors"`   // an observation was produced but the DB insert failed
	EmbedErrors    int64  `json:"embed_errors"`    // insert succeeded, embedding failed (keyword search still works)
	CachedSessions int    `json:"cached_sessions"`
	PoolInFlight   int    `json:"pool_in_flight"`
	PoolCapacity   int    `json:"pool_capacity"`
	LastActivityAt string `json:"last_activity_at,omitempty"` // RFC3339; empty if nothing processed yet
	UpdatedAt      string `json:"updated_at"`
	// Store is the database this daemon is actually writing to, already
	// redacted (see store.RedactDSN) because a Postgres DSN carries a
	// password and this file is world-readable in the user's home.
	//
	// The daemon is a long-lived process that opened its store once, at
	// start. Nothing re-reads $CLAUDE_MEM_DB afterwards, and nothing
	// should — a daemon silently switching databases underneath in-flight
	// work would be worse. But that means a worker started before the
	// variable changed keeps writing to the OLD store while every hook,
	// every CLI command and `doctor` itself resolve the new one, and
	// until this field existed nothing anywhere reported which store the
	// daemon had.
	//
	// Reproduced end to end: a worker started on store A, CLAUDE_MEM_DB
	// then pointed at B, one PostToolUse event — the observation landed
	// in A, while `doctor -db B` reported "worker daemon reachable" and
	// "database reachable (B)" and "the store is empty — nothing recorded
	// yet". Two green checks and a reassuring message, with the memory in
	// a different file.
	Store string `json:"store,omitempty"`
	// Version is the daemon's own build string, and PID is its process
	// id — together, enough to notice that the daemon is running code
	// older than the binary everything else uses, and to do something
	// about it.
	//
	// This is the same shape of gap as Store above, and was found the
	// same way: by running the whole loop end to end rather than trusting
	// unit tests. A daemon that had been up for ~28 hours across sixteen
	// commits was still applying the OLD project-naming rule, so a real
	// session in a git subdirectory had its observations written under
	// project "auth" (basename) while the freshly-built SessionStart hook
	// looked them up under "repo" (git root). Writes and reads disagreed,
	// silently, and the project-naming fix was defeated by a process that
	// simply never restarted.
	//
	// `start` only ever asked whether a daemon was running, never which
	// one, so nothing anywhere could notice.
	Version string `json:"version,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

// statsCounters is the daemon's live counters — atomic because process()
// runs concurrently (one goroutine per accepted connection, serialized only
// per-session, not globally).
type statsCounters struct {
	processed      atomic.Int64
	duplicates     atomic.Int64
	observerErrors atomic.Int64
	insertErrors   atomic.Int64
	embedErrors    atomic.Int64
	lastActivityNS atomic.Int64 // UnixNano; 0 means never
}

func (c *statsCounters) touch() { c.lastActivityNS.Store(time.Now().UnixNano()) }

// snapshot builds a Stats from the current counters plus whatever the
// caller passes for the parts statsCounters doesn't own (pool/session-cache
// state lives in sessionCache, not here, to avoid an import cycle and
// because those are already point-in-time queries, not counters).
func (c *statsCounters) snapshot(cachedSessions, poolInFlight, poolCapacity int) Stats {
	s := Stats{
		Processed:      c.processed.Load(),
		Duplicates:     c.duplicates.Load(),
		ObserverErrors: c.observerErrors.Load(),
		InsertErrors:   c.insertErrors.Load(),
		EmbedErrors:    c.embedErrors.Load(),
		CachedSessions: cachedSessions,
		PoolInFlight:   poolInFlight,
		PoolCapacity:   poolCapacity,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if ns := c.lastActivityNS.Load(); ns != 0 {
		s.LastActivityAt = time.Unix(0, ns).UTC().Format(time.RFC3339)
	}
	return s
}

// writeStatsFile writes s to path atomically (temp file + rename) so a
// concurrent reader (doctor, or anything else polling this file) never
// observes a partially-written JSON document.
func writeStatsFile(path string, s Stats) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal worker stats: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write worker stats temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename worker stats file into place: %w", err)
	}
	return nil
}

// ReadStatsFile reads and parses a stats file written by writeStatsFile —
// used by `doctor` (and available to anything else that wants the
// daemon's last-known activity without talking to it directly).
func ReadStatsFile(path string) (Stats, error) {
	var s Stats
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parse worker stats file %s: %w", path, err)
	}
	return s, nil
}
