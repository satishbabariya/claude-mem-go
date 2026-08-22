package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// createUserPromptsSQL is migration 8: the user_prompts table real
// claude-mem keeps (src/services/sqlite/SessionStore.ts — id,
// session_db_id/content_session_id, prompt_number, prompt_text,
// created_at, created_at_epoch, with a user_prompts_fts(prompt_text) FTS5
// table kept in sync by user_prompts_ai/_ad triggers) and the last
// substantive schema gap this port had. Two deliberate differences: this
// port keys sessions by the Claude Code session_id string rather than an
// sdk_sessions row, and project is stored on the row rather than joined
// from one (real claude-mem's prompt search JOINs sdk_sessions for it). This is the one
// table that holds the user's verbatim words rather than a model's
// summary of them, so the write path is opt-in (see
// cmd/claude-mem-go/prompt_context.go); the schema exists regardless so
// search/export/stats have something well-defined to read.
//
// The FTS5 shadow table is built exactly like observations_fts — a
// self-contained table (see createFTSSQL for the modernc.org/sqlite
// external-content problem that forces this) kept in sync by AFTER
// INSERT/AFTER DELETE triggers, and the delete trigger uses the plain
// DELETE form for the same reason migration 5 had to fix observations_ad.
//
// UNIQUE(session_id, prompt_number) is what makes ImportPrompt idempotent
// (ON CONFLICT DO NOTHING), the same role content_hash plays for
// observations.
const createUserPromptsSQL = `
CREATE TABLE IF NOT EXISTS user_prompts (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id        TEXT NOT NULL,
	project           TEXT NOT NULL,
	prompt_text       TEXT NOT NULL,
	prompt_number     INTEGER NOT NULL,
	created_at        TEXT NOT NULL,
	created_at_epoch  INTEGER NOT NULL,
	UNIQUE (session_id, prompt_number)
);
CREATE INDEX IF NOT EXISTS idx_user_prompts_project ON user_prompts(project);
CREATE INDEX IF NOT EXISTS idx_user_prompts_created ON user_prompts(created_at_epoch DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS user_prompts_fts USING fts5(prompt_text);
CREATE TRIGGER IF NOT EXISTS user_prompts_ai AFTER INSERT ON user_prompts BEGIN
	INSERT INTO user_prompts_fts(rowid, prompt_text) VALUES (new.id, new.prompt_text);
END;
CREATE TRIGGER IF NOT EXISTS user_prompts_ad AFTER DELETE ON user_prompts BEGIN
	DELETE FROM user_prompts_fts WHERE rowid = old.id;
END;
`

// InsertPrompt implements memory.Backend.InsertPrompt. prompt_number is
// assigned inside one transaction as max(prompt_number)+1 for the
// session — equivalent to "count+1" while a session's prompts are
// intact, but still correct after a prune or a partial import has left a
// gap, where count+1 would collide with the UNIQUE constraint.
func (s *Store) InsertPrompt(ctx context.Context, sessionID, project, promptText string) (int64, error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("insert prompt: begin: %w", err)
	}
	defer tx.Rollback()
	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(prompt_number), 0) + 1 FROM user_prompts WHERE session_id = ?`, sessionID).Scan(&next); err != nil {
		return 0, fmt.Errorf("insert prompt: next prompt_number: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO user_prompts (session_id, project, prompt_text, prompt_number, created_at, created_at_epoch)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, project, promptText, next, now.Format(time.RFC3339), now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("insert prompt: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert prompt: last insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("insert prompt: commit: %w", err)
	}
	return id, nil
}

// SearchPrompts implements memory.Backend.SearchPrompts — the same two
// shapes Search has: an FTS5 MATCH ranked by bm25 (tied on id so paging
// stays deterministic), or, for an empty query, a plain newest-first
// enumeration that never touches the FTS table.
func (s *Store) SearchPrompts(ctx context.Context, project, query string, limit, offset int) ([]memory.PromptResult, error) {
	limit = clampNegativeLimit(limit)
	offset = clampNegativeLimit(offset)
	enumerate := strings.TrimSpace(query) == ""
	var args []any
	if !enumerate {
		args = append(args, sanitizeFTSQuery(query))
	}
	scope := ""
	if project != "" {
		scope = " AND p.project = ?"
		args = append(args, project)
	}
	args = append(args, limit, offset)

	from, where, order := "user_prompts_fts f JOIN user_prompts p ON p.id = f.rowid", "user_prompts_fts MATCH ?", "ORDER BY rank, p.id"
	if enumerate {
		from, where, order = "user_prompts p", "1=1", "ORDER BY p.created_at_epoch DESC, p.id DESC"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.session_id, p.project, p.prompt_number, p.prompt_text, p.created_at_epoch
		FROM `+from+`
		WHERE `+where+scope+`
		`+order+`
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("search prompts %q: %w", query, err)
	}
	return scanPromptResults(rows)
}

// PromptsBySession implements memory.Backend.PromptsBySession: one
// session's prompts in prompt_number order, scoped like BySessionID.
func (s *Store) PromptsBySession(ctx context.Context, project, sessionID string, limit int) ([]memory.PromptResult, error) {
	limit = clampNegativeLimit(limit)
	args := []any{sessionID}
	scope := ""
	if project != "" {
		scope = " AND project = ?"
		args = append(args, project)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, prompt_number, prompt_text, created_at_epoch
		FROM user_prompts
		WHERE session_id = ?`+scope+`
		ORDER BY prompt_number ASC, id ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("prompts for session %q: %w", sessionID, err)
	}
	return scanPromptResults(rows)
}

// scanPromptResults drains rows selected as
//
//	id, session_id, project, prompt_number, prompt_text, created_at_epoch
func scanPromptResults(rows *sql.Rows) ([]memory.PromptResult, error) {
	defer rows.Close()
	var out []memory.PromptResult
	for rows.Next() {
		var r memory.PromptResult
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.PromptNumber, &r.Text, &r.CreatedAtEpoch); err != nil {
			return nil, fmt.Errorf("scan prompt row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExportPrompts implements memory.Backend.ExportPrompts, paginated
// exactly like ExportAll.
func (s *Store) ExportPrompts(ctx context.Context, afterID int64, limit int) ([]memory.PromptRow, error) {
	limit = clampNegativeLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, prompt_number, prompt_text, created_at, created_at_epoch
		FROM user_prompts
		WHERE id > ?
		ORDER BY id ASC
		LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("export prompts after id %d: %w", afterID, err)
	}
	defer rows.Close()
	var out []memory.PromptRow
	for rows.Next() {
		r := memory.PromptRow{Kind: memory.PromptRowKind}
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.PromptNumber, &r.Text, &r.CreatedAt, &r.CreatedAtEpoch); err != nil {
			return nil, fmt.Errorf("scan export prompt row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ImportPrompt implements memory.Backend.ImportPrompt. The original
// created_at string is stored as-is (the column is TEXT) after being
// validated, for the same byte-identical round-trip reason ImportRow
// gives; (session_id, prompt_number) is the idempotency key.
func (s *Store) ImportPrompt(ctx context.Context, row memory.PromptRow) (bool, error) {
	if _, err := memory.ParseExportCreatedAt(row.CreatedAt); err != nil {
		return false, err
	}
	if row.PromptNumber < 1 {
		return false, fmt.Errorf("import prompt: prompt_number %d must be >= 1", row.PromptNumber)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO user_prompts (session_id, project, prompt_text, prompt_number, created_at, created_at_epoch)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, prompt_number) DO NOTHING`,
		row.SessionID, row.Project, row.Text, row.PromptNumber, row.CreatedAt, row.CreatedAtEpoch)
	if err != nil {
		return false, fmt.Errorf("import prompt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("import prompt: rows affected: %w", err)
	}
	return n > 0, nil
}
