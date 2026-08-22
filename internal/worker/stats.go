package worker

import (
	"encoding/json"
	"fmt"
	"github.com/satishbabariya/claude-mem-go/internal/hook"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/store"
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
	Processed      int64 `json:"processed"`       // turns that completed and persisted a new observation
	Duplicates     int64 `json:"duplicates"`      // turns that hit an already-persisted content_hash (not an error)
	ObserverErrors int64 `json:"observer_errors"` // the observer turn itself failed (including the one retry)
	InsertErrors   int64 `json:"insert_errors"`   // an observation was produced but the DB insert failed
	EmbedErrors    int64 `json:"embed_errors"`    // insert succeeded, embedding failed (keyword search still works)
	// RecallBySource describes the READ path, broken down by which hook
	// did the reading. Every other counter here describes writes, which
	// left this system's quietest failure — recall returning nothing —
	// with no observable signal at all.
	// Fixed fields rather than a map keyed by source: the source set is
	// closed (ParseRecallReport rejects anything else), and this keeps
	// Stats comparable with == — a property its own tests rely on — as
	// well as giving deterministic ordering in the metrics output for
	// free.
	RecallPrompt   RecallStat `json:"recall_prompt"`
	RecallSession  RecallStat `json:"recall_session"`
	RecallFile     RecallStat `json:"recall_file"`
	CachedSessions int        `json:"cached_sessions"`
	PoolInFlight   int        `json:"pool_in_flight"`
	PoolCapacity   int        `json:"pool_capacity"`
	LastActivityAt string     `json:"last_activity_at,omitempty"` // RFC3339; empty if nothing processed yet
	UpdatedAt      string     `json:"updated_at"`
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

// RecallStat is one read path's recall tally, as persisted and exposed.
type RecallStat struct {
	Searches int64 `json:"searches"`
	Empty    int64 `json:"empty"`
}

// RecallTotals sums every read path. Used for the compact one-line
// summaries (doctor's activity line); the per-source breakdown is what
// any actual diagnosis needs.
func (s Stats) RecallTotals() (searches, empty int64) {
	for _, v := range s.RecallAll() {
		searches += v.Stat.Searches
		empty += v.Stat.Empty
	}
	return searches, empty
}

// NamedRecall pairs a read path's label with its tally, so callers can
// iterate every source without repeating the list.
type NamedRecall struct {
	Source string
	Stat   RecallStat
}

// RecallAll returns every read path in a stable, meaningful order —
// session first because an empty there is the most consequential.
func (s Stats) RecallAll() []NamedRecall {
	return []NamedRecall{
		{string(hook.RecallSession), s.RecallSession},
		{string(hook.RecallPrompt), s.RecallPrompt},
		{string(hook.RecallFile), s.RecallFile},
	}
}

// RecallAlarming sums only the read paths where an empty result is
// genuinely suspicious. The file-context lookup is deliberately excluded:
// most files have no prior observations, so its empty rate is high on a
// perfectly healthy install and folding it in would make any threshold
// meaningless.
func (s Stats) RecallAlarming() (searches, empty int64) {
	for _, v := range []RecallStat{s.RecallPrompt, s.RecallSession} {
		searches += v.Searches
		empty += v.Empty
	}
	return searches, empty
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
	// recallSearches/recallEmpty instrument the READ path, which nothing
	// else here does. recallEmpty is the one that matters: a semantic
	// search returning zero results is this system's worst failure mode
	// precisely because it looks identical to "nothing was relevant".
	// One pair per read path rather than a single total. Empty results
	// mean opposite things depending on where they came from — a
	// file-context lookup finding nothing is ordinary, a session starting
	// with nothing is the worst failure this system has — so averaging
	// them together would destroy the signal instead of broadening it.
	recallPrompt   recallCounter
	recallSession  recallCounter
	recallFile     recallCounter
	lastActivityNS atomic.Int64 // UnixNano; 0 means never
}

// recordRecall notes one completed semantic recall and whether it came
// back empty. Deliberately does NOT touch() the activity timestamp: that
// field means "the capture pipeline is alive", and a read arriving while
// no writes happen would make a stalled capture path look healthy.
func (c *statsCounters) recordRecall(source hook.RecallSource, results int) {
	var t *recallCounter
	switch source {
	case hook.RecallPrompt:
		t = &c.recallPrompt
	case hook.RecallSession:
		t = &c.recallSession
	case hook.RecallFile:
		t = &c.recallFile
	default:
		// Unreachable: ParseRecallReport rejects unknown sources before
		// this is called. Dropping rather than guessing a bucket, because
		// a miscounted source is worse than an uncounted one.
		return
	}
	t.searches.Add(1)
	if results == 0 {
		t.empty.Add(1)
	}
}

// recallCounter is one read path's tally.
type recallCounter struct {
	searches atomic.Int64
	empty    atomic.Int64
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
		RecallPrompt:   RecallStat{Searches: c.recallPrompt.searches.Load(), Empty: c.recallPrompt.empty.Load()},
		RecallSession:  RecallStat{Searches: c.recallSession.searches.Load(), Empty: c.recallSession.empty.Load()},
		RecallFile:     RecallStat{Searches: c.recallFile.searches.Load(), Empty: c.recallFile.empty.Load()},
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
