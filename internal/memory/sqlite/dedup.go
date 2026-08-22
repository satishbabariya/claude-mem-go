// Content-hash dedup — the first of three gaps flagged as "known
// limitations" toward enterprise readiness. Real claude-mem added a
// content_hash column (src/services/sqlite/SessionStore.ts) precisely
// because re-syncing or re-processing the same event must not create a
// second observation. This project had the identical exposure: re-running
// `ingest` on a transcript already ingested, or a PostToolUse hook firing
// twice for the same event (both real possibilities, not hypothetical —
// hooks are documented as at-least-once, not exactly-once), silently
// duplicated rows.
//
// memory.ContentHash is computed from the SOURCE material (session + tool call),
// not the model's output — the model's XML reply can vary slightly between
// retries even for byte-identical input, so hashing the output would make
// two genuinely-the-same-event retries look like different observations.
// Hashing the input is the correct idempotency key: "have we already
// observed THIS tool call in THIS session."
package sqlite

import (
	"database/sql"
	"fmt"
)

// ensureContentHashColumn adds content_hash to a database created before
// this column existed (this project's own earlier schema, or any database
// still on it), backfills existing rows with a synthetic-but-unique value
// (there's no way to recover the original tool_input/tool_output to hash
// properly — those were never stored — so "legacy-<id>" just satisfies the
// uniqueness constraint without colliding), then ensures the column and its
// unique index exist for every database going forward.
func ensureContentHashColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(observations)`)
	if err != nil {
		return fmt.Errorf("read observations schema: %w", err)
	}
	hasColumn := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema row: %w", err)
		}
		if name == "content_hash" {
			hasColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if !hasColumn {
		if _, err := db.Exec(`ALTER TABLE observations ADD COLUMN content_hash TEXT`); err != nil {
			return fmt.Errorf("add content_hash column: %w", err)
		}
		if _, err := db.Exec(`UPDATE observations SET content_hash = 'legacy-' || id WHERE content_hash IS NULL`); err != nil {
			return fmt.Errorf("backfill content_hash for pre-existing rows: %w", err)
		}
	}

	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_observations_content_hash ON observations(content_hash)`); err != nil {
		return fmt.Errorf("create content_hash unique index: %w", err)
	}
	return nil
}
