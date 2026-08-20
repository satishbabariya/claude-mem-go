// Package store persists observations to SQLite and parses the XML an
// observer session replies with into a structured Observation.
//
// The schema is a deliberately narrower cousin of claude-mem's real
// observations table (src/services/sqlite/SessionStore.ts), which has
// accumulated 40+ migrations' worth of columns (sync/origin-device
// bookkeeping, an FTS5 shadow table, content hashes, etc.). This keeps only
// the columns that describe an observation's actual content — the same
// ones ResponseProcessor.ts populates into its broadcast record — encoded
// the same way: facts/concepts/files_read/files_modified as
// JSON.stringify(x||[]) TEXT columns, not raw XML.
//
// IMPORTANT: this is its own database, in ~/.claude-mem-go/. It must never
// write to the real ~/.claude-mem/claude-mem.db — that file has a live
// foreign key (memory_session_id -> sdk_sessions) and schema this package
// doesn't replicate; sharing storage would risk corrupting a real claude-mem
// installation for no benefit.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo — registers as "sqlite"
)

// DefaultHome is ~/.claude-mem-go, created if missing.
func DefaultHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(home, ".claude-mem-go")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// DefaultDBPath is ~/.claude-mem-go/observations.db.
func DefaultDBPath() string { return filepath.Join(DefaultHome(), "observations.db") }

// Observation is what an observer session's XML reply decodes into — the
// same field set buildObservationPrompt (observer package) asks for, and
// the same one this package's schema persists.
type Observation struct {
	Type          string
	Title         string
	Subtitle      string
	Facts         []string
	Narrative     string
	Concepts      []string
	FilesRead     []string
	FilesModified []string
}

var xmlFenceRe = regexp.MustCompile("(?s)```(?:xml)?\\s*(.*?)\\s*```")

func tagRe(tag string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`)
}

func extractTag(s, tag string) string {
	m := tagRe(tag).FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// extractItems reads a wrapper block (e.g. <facts>...</facts>) and returns
// every <item>...</item> found inside it, in order.
func extractItems(s, block, item string) []string {
	blockMatch := tagRe(block).FindStringSubmatch(s)
	if blockMatch == nil {
		return nil
	}
	matches := tagRe(item).FindAllStringSubmatch(blockMatch[1], -1)
	var out []string
	for _, m := range matches {
		v := strings.TrimSpace(m[1])
		if v != "" && v != "..." {
			out = append(out, v)
		}
	}
	return out
}

// ParseXML pulls an Observation out of an observer session's raw text
// reply. Models routinely wrap XML in a ```xml code fence despite being
// told not to — strip that first, tolerantly: a shape we don't expect is
// worked around, not fatal.
func ParseXML(raw string) (Observation, error) {
	body := raw
	if m := xmlFenceRe.FindStringSubmatch(raw); m != nil {
		body = m[1]
	}
	if !strings.Contains(body, "<observation>") {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return Observation{}, fmt.Errorf("no <observation> block in model output: %q", snippet)
	}

	return Observation{
		Type:          extractTag(body, "type"),
		Title:         extractTag(body, "title"),
		Subtitle:      extractTag(body, "subtitle"),
		Facts:         extractItems(body, "facts", "fact"),
		Narrative:     extractTag(body, "narrative"),
		Concepts:      extractItems(body, "concepts", "concept"),
		FilesRead:     extractItems(body, "files_read", "file"),
		FilesModified: extractItems(body, "files_modified", "file"),
	}, nil
}

// Store is a thin wrapper around the sqlite handle.
type Store struct {
	db *sql.DB
}

const createTableSQL = `
CREATE TABLE IF NOT EXISTS observations (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id        TEXT NOT NULL,
	project           TEXT NOT NULL,
	tool_name         TEXT NOT NULL,
	type              TEXT NOT NULL,
	title             TEXT,
	subtitle          TEXT,
	facts             TEXT NOT NULL DEFAULT '[]',
	narrative         TEXT,
	concepts          TEXT NOT NULL DEFAULT '[]',
	files_read        TEXT NOT NULL DEFAULT '[]',
	files_modified    TEXT NOT NULL DEFAULT '[]',
	cost_usd          REAL NOT NULL DEFAULT 0,
	created_at        TEXT NOT NULL,
	created_at_epoch  INTEGER NOT NULL
	-- content_hash is added by ensureContentHashColumn (dedup.go), not here:
	-- a brand-new database gets it as part of the same call, but a database
	-- created before this column existed needs ALTER TABLE, not CREATE TABLE
	-- IF NOT EXISTS (which is a no-op once the table already exists).
);
CREATE INDEX IF NOT EXISTS idx_observations_session ON observations(session_id);
CREATE INDEX IF NOT EXISTS idx_observations_project ON observations(project);
CREATE INDEX IF NOT EXISTS idx_observations_type ON observations(type);
CREATE INDEX IF NOT EXISTS idx_observations_created ON observations(created_at_epoch DESC);
`

// Open opens (creating if needed) the sqlite file at path and ensures the
// schema exists.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return &Store{db: db}, nil
}

func jsonArray(items []string) string {
	if items == nil {
		items = []string{}
	}
	b, _ := json.Marshal(items) // []string always marshals cleanly
	return string(b)
}

// InsertResult reports whether Insert actually created a new row or found
// one already there with the same ContentHash — the caller (worker/ingest)
// needs to know which happened: a duplicate is not an error, but it also
// shouldn't be double-counted or re-embedded.
type InsertResult struct {
	ID       int64
	Inserted bool // false: a row with this ContentHash already existed; ID is that row's.
}

// Insert persists one observation, JSON-encoding facts/concepts/files_*
// exactly like ResponseProcessor.ts's broadcast record does. contentHash
// (see ContentHash) is the idempotency key: inserting the same hash twice
// is a no-op that returns the original row, not a duplicate.
func (s *Store) Insert(sessionID, project, toolName, contentHash string, o Observation, costUSD float64) (InsertResult, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO observations
			(session_id, project, tool_name, type, title, subtitle, facts, narrative,
			 concepts, files_read, files_modified, cost_usd, created_at, created_at_epoch, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(content_hash) DO NOTHING`,
		sessionID, project, toolName, o.Type, o.Title, o.Subtitle,
		jsonArray(o.Facts), o.Narrative, jsonArray(o.Concepts),
		jsonArray(o.FilesRead), jsonArray(o.FilesModified),
		costUSD, now.Format(time.RFC3339), now.UnixMilli(), contentHash,
	)
	if err != nil {
		return InsertResult{}, fmt.Errorf("insert observation: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return InsertResult{}, fmt.Errorf("insert observation, checking rows affected: %w", err)
	}
	if affected == 0 {
		var id int64
		if err := s.db.QueryRow(`SELECT id FROM observations WHERE content_hash = ?`, contentHash).Scan(&id); err != nil {
			return InsertResult{}, fmt.Errorf("look up existing observation for duplicate content_hash: %w", err)
		}
		return InsertResult{ID: id, Inserted: false}, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return InsertResult{}, err
	}
	return InsertResult{ID: id, Inserted: true}, nil
}

// CountByProject is a small read-path check useful for verifying a round
// trip, mirroring the kind of query src/services/sqlite/SessionSearch.ts
// runs (filter by project).
func (s *Store) CountByProject(project string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM observations WHERE project = ?`, project).Scan(&n)
	return n, err
}

func (s *Store) Close() error { return s.db.Close() }
