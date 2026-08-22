package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// userPromptsSchemaSQL is migration 6: the Postgres half of the
// user_prompts table — see the SQLite backend's createUserPromptsSQL for
// the real claude-mem table this mirrors. Full-text search is a generated
// tsvector column under a GIN index, the same approach observations'
// search_vector takes, rather than a shadow table.
const userPromptsSchemaSQL = `
CREATE TABLE IF NOT EXISTS user_prompts (
	id                BIGSERIAL PRIMARY KEY,
	session_id        TEXT NOT NULL,
	project           TEXT NOT NULL,
	prompt_text       TEXT NOT NULL,
	prompt_number     INTEGER NOT NULL,
	created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
	created_at_epoch  BIGINT NOT NULL,
	search_vector     tsvector GENERATED ALWAYS AS (to_tsvector('english', prompt_text)) STORED,
	UNIQUE (session_id, prompt_number)
);
CREATE INDEX IF NOT EXISTS idx_user_prompts_project ON user_prompts(project);
CREATE INDEX IF NOT EXISTS idx_user_prompts_created ON user_prompts(created_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_user_prompts_search_vector ON user_prompts USING GIN(search_vector);
`

// InsertPrompt implements memory.Backend.InsertPrompt — see the SQLite
// backend for why prompt_number is max+1 inside one transaction.
func (s *Store) InsertPrompt(ctx context.Context, sessionID, project, promptText string) (int64, error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("insert prompt: begin: %w", err)
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO user_prompts (session_id, project, prompt_text, prompt_number, created_at, created_at_epoch)
		VALUES ($1, $2, $3,
			(SELECT COALESCE(MAX(prompt_number), 0) + 1 FROM user_prompts WHERE session_id = $1),
			$4, $5)
		RETURNING id`,
		sessionID, project, promptText, now, now.UnixMilli()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert prompt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("insert prompt: commit: %w", err)
	}
	return id, nil
}

// SearchPrompts implements memory.Backend.SearchPrompts with the same
// websearch_to_tsquery translation Search uses, so a boolean query means
// the same thing for prompts on both backends.
func (s *Store) SearchPrompts(ctx context.Context, project, query string, limit, offset int) ([]memory.PromptResult, error) {
	limit = clampNegativeLimit(limit)
	offset = clampNegativeLimit(offset)
	enumerate := strings.TrimSpace(query) == ""
	var args []any
	predicate, order := "TRUE", "ORDER BY created_at_epoch DESC, id DESC"
	if !enumerate {
		args = append(args, websearchQuery(query))
		predicate = "search_vector @@ websearch_to_tsquery('english', $1)"
		order = "ORDER BY ts_rank_cd(search_vector, websearch_to_tsquery('english', $1)) DESC, id"
	}
	scope := ""
	if project != "" {
		args = append(args, project)
		scope = fmt.Sprintf(" AND project = $%d", len(args))
	}
	args = append(args, limit)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))
	args = append(args, offset)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args))
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, prompt_number, prompt_text, created_at_epoch
		FROM user_prompts
		WHERE `+predicate+scope+`
		`+order+`
		LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder, args...)
	if err != nil {
		return nil, fmt.Errorf("search prompts %q: %w", query, err)
	}
	return scanPromptResults(rows)
}

// PromptsBySession implements memory.Backend.PromptsBySession.
func (s *Store) PromptsBySession(ctx context.Context, project, sessionID string, limit int) ([]memory.PromptResult, error) {
	limit = clampNegativeLimit(limit)
	args := []any{sessionID}
	scope := ""
	if project != "" {
		args = append(args, project)
		scope = fmt.Sprintf(" AND project = $%d", len(args))
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, prompt_number, prompt_text, created_at_epoch
		FROM user_prompts
		WHERE session_id = $1`+scope+`
		ORDER BY prompt_number ASC, id ASC
		LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("prompts for session %q: %w", sessionID, err)
	}
	return scanPromptResults(rows)
}

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

// ExportPrompts implements memory.Backend.ExportPrompts. CreatedAt
// travels as RFC3339 for the same cross-backend reason ExportAll's does.
func (s *Store) ExportPrompts(ctx context.Context, afterID int64, limit int) ([]memory.PromptRow, error) {
	limit = clampNegativeLimit(limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, project, prompt_number, prompt_text, created_at, created_at_epoch
		FROM user_prompts
		WHERE id > $1
		ORDER BY id ASC
		LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("export prompts after id %d: %w", afterID, err)
	}
	defer rows.Close()
	var out []memory.PromptRow
	for rows.Next() {
		r := memory.PromptRow{Kind: memory.PromptRowKind}
		var createdAt time.Time
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Project, &r.PromptNumber, &r.Text, &createdAt, &r.CreatedAtEpoch); err != nil {
			return nil, fmt.Errorf("scan export prompt row: %w", err)
		}
		r.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ImportPrompt implements memory.Backend.ImportPrompt, idempotent on
// (session_id, prompt_number).
func (s *Store) ImportPrompt(ctx context.Context, row memory.PromptRow) (bool, error) {
	createdAt, err := memory.ParseExportCreatedAt(row.CreatedAt)
	if err != nil {
		return false, err
	}
	if row.PromptNumber < 1 {
		return false, fmt.Errorf("import prompt: prompt_number %d must be >= 1", row.PromptNumber)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO user_prompts (session_id, project, prompt_text, prompt_number, created_at, created_at_epoch)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (session_id, prompt_number) DO NOTHING`,
		row.SessionID, row.Project, row.Text, row.PromptNumber, createdAt, row.CreatedAtEpoch)
	if err != nil {
		return false, fmt.Errorf("import prompt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("import prompt: rows affected: %w", err)
	}
	return n > 0, nil
}
