package store

import (
	"path/filepath"
	"testing"
)

func TestContentHashIsDeterministicAndDistinguishesInputs(t *testing.T) {
	h1 := ContentHash("session-1", "Bash", "ls -la", "file1\nfile2")
	h2 := ContentHash("session-1", "Bash", "ls -la", "file1\nfile2")
	if h1 != h2 {
		t.Fatal("ContentHash is not deterministic for identical inputs")
	}

	cases := []struct {
		name                                       string
		sessionID, toolName, toolInput, toolOutput string
	}{
		{"different session", "session-2", "Bash", "ls -la", "file1\nfile2"},
		{"different tool", "session-1", "Read", "ls -la", "file1\nfile2"},
		{"different input", "session-1", "Bash", "ls -l", "file1\nfile2"},
		{"different output", "session-1", "Bash", "ls -la", "file1\nfile2\nfile3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ContentHash(c.sessionID, c.toolName, c.toolInput, c.toolOutput); got == h1 {
				t.Fatalf("ContentHash collided with the baseline for a %s — hash isn't sensitive to that field", c.name)
			}
		})
	}
}

func TestInsertIsIdempotentOnContentHash(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	hash := ContentHash("session-1", "Bash", "npm test", "42 passed")
	o := Observation{Type: "discovery", Title: "Tests passed"}

	first, err := st.Insert("session-1", "proj", "Bash", hash, o, 0.01)
	if err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if !first.Inserted {
		t.Fatal("first Insert of a fresh content_hash: want Inserted=true")
	}

	// Simulate re-ingesting the same transcript, or a hook firing twice for
	// the same event — the real scenarios this exists to prevent.
	second, err := st.Insert("session-1", "proj", "Bash", hash, o, 0.01)
	if err != nil {
		t.Fatalf("second Insert (duplicate): %v", err)
	}
	if second.Inserted {
		t.Fatal("second Insert with the same content_hash: want Inserted=false (duplicate)")
	}
	if second.ID != first.ID {
		t.Fatalf("second Insert returned ID %d, want the original row's ID %d", second.ID, first.ID)
	}

	count, err := st.CountByProject("proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject = %d, want 1 — the duplicate must not have created a second row", count)
	}
}

func TestInsertAllowsSameToolCallInDifferentSessions(t *testing.T) {
	// Two different sessions running the identical command with identical
	// output (e.g. both ran "ls -la" in a fresh checkout) are legitimately
	// distinct observations — content_hash is scoped per-session, not global.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	o := Observation{Type: "discovery", Title: "Listed files"}
	r1, err := st.Insert("session-a", "proj", "Bash", ContentHash("session-a", "Bash", "ls -la", "same output"), o, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	r2, err := st.Insert("session-b", "proj", "Bash", ContentHash("session-b", "Bash", "ls -la", "same output"), o, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !r1.Inserted || !r2.Inserted {
		t.Fatal("both sessions' observations should be inserted as new rows, not deduped against each other")
	}
	if r1.ID == r2.ID {
		t.Fatal("different sessions got the same row id")
	}
}

func TestEnsureContentHashColumnMigratesPreExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Simulate a database created before content_hash existed: open once,
	// then drop the column and its index the same migration adds, and
	// insert a row directly via the pre-migration schema shape.
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.db.Exec(`DROP INDEX IF EXISTS idx_observations_content_hash`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := st.db.Exec(`
		CREATE TABLE observations_legacy (
			id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, project TEXT NOT NULL,
			tool_name TEXT NOT NULL, type TEXT NOT NULL, title TEXT, subtitle TEXT,
			facts TEXT NOT NULL DEFAULT '[]', narrative TEXT, concepts TEXT NOT NULL DEFAULT '[]',
			files_read TEXT NOT NULL DEFAULT '[]', files_modified TEXT NOT NULL DEFAULT '[]',
			cost_usd REAL NOT NULL DEFAULT 0, created_at TEXT NOT NULL, created_at_epoch INTEGER NOT NULL
		)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := st.db.Exec(`DROP TABLE observations`); err != nil {
		t.Fatalf("drop current table: %v", err)
	}
	if _, err := st.db.Exec(`ALTER TABLE observations_legacy RENAME TO observations`); err != nil {
		t.Fatalf("rename legacy table into place: %v", err)
	}
	if _, err := st.db.Exec(`
		INSERT INTO observations (session_id, project, tool_name, type, title, created_at, created_at_epoch)
		VALUES ('old-session', 'old-proj', 'Bash', 'discovery', 'A pre-migration row', '2020-01-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	st.Close()

	// Re-Open should detect the missing column, migrate it in, backfill the
	// pre-existing row with a synthetic-but-unique hash, and leave the
	// database usable — this is the exact path a real upgrade takes.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-Open after simulated pre-migration state: %v", err)
	}
	defer st2.Close()

	count, err := st2.CountByProject("old-proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject after migration = %d, want 1 (pre-existing row preserved)", count)
	}

	// The migrated database must still enforce uniqueness going forward.
	dupHash := ContentHash("new-session", "Bash", "new command", "new output")
	first, err := st2.Insert("new-session", "old-proj", "Bash", dupHash, Observation{Type: "discovery", Title: "x"}, 0)
	if err != nil {
		t.Fatalf("Insert after migration: %v", err)
	}
	if !first.Inserted {
		t.Fatal("Insert after migration: want Inserted=true for a fresh hash")
	}
	second, err := st2.Insert("new-session", "old-proj", "Bash", dupHash, Observation{Type: "discovery", Title: "x"}, 0)
	if err != nil {
		t.Fatalf("Insert after migration (duplicate): %v", err)
	}
	if second.Inserted {
		t.Fatal("Insert after migration: dedup must still work for new rows")
	}
}
