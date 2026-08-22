package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultDBPathHonorsEnvVar is the regression test for a real,
// measured gap: the Postgres backend was unreachable from an installed
// plugin. Nothing a plugin runs can pass `-db` (see DBPathEnvVar), so the
// env var is the whole mechanism — if this stops working, Postgres
// silently stops being reachable again and every hook quietly writes to
// SQLite instead.
func TestDefaultDBPathHonorsEnvVar(t *testing.T) {
	t.Run("a postgres DSN is returned verbatim", func(t *testing.T) {
		const dsn = "postgres://u:p@localhost:55432/claudemem?sslmode=disable"
		t.Setenv(DBPathEnvVar, dsn)
		if got := DefaultDBPath(); got != dsn {
			t.Fatalf("DefaultDBPath() = %q, want the DSN %q returned unchanged — "+
				"any rewriting here would break the one path a plugin has to Postgres", got, dsn)
		}
	})

	t.Run("a plain file path also works", func(t *testing.T) {
		// The variable is not Postgres-only: pointing several projects at
		// separate SQLite files is a legitimate use, and DefaultDBPath has
		// no business deciding which backend a value names — backend.Open
		// dispatches on the scheme.
		p := filepath.Join(t.TempDir(), "custom.db")
		t.Setenv(DBPathEnvVar, p)
		if got := DefaultDBPath(); got != p {
			t.Fatalf("DefaultDBPath() = %q, want %q", got, p)
		}
	})

	t.Run("unset falls back to the home-directory default", func(t *testing.T) {
		t.Setenv(DBPathEnvVar, "")
		got := DefaultDBPath()
		if !strings.HasSuffix(got, filepath.Join(".claude-mem-go", "observations.db")) {
			t.Fatalf("DefaultDBPath() with the var unset = %q, want the ~/.claude-mem-go default", got)
		}
	})

	t.Run("whitespace-only is treated as unset", func(t *testing.T) {
		// `CLAUDE_MEM_DB=$UNSET_VAR` in a shell profile produces exactly
		// this. Honoring it would point the store at a file named " " and
		// look, from the outside, like the memory had been wiped.
		t.Setenv(DBPathEnvVar, "   ")
		got := DefaultDBPath()
		if !strings.HasSuffix(got, filepath.Join(".claude-mem-go", "observations.db")) {
			t.Fatalf("DefaultDBPath() with a whitespace-only value = %q, want it ignored "+
				"in favour of the default rather than used as a literal filename", got)
		}
	})

	t.Run("surrounding whitespace is trimmed, not preserved", func(t *testing.T) {
		const dsn = "postgres://u:p@localhost:55432/db"
		t.Setenv(DBPathEnvVar, "  "+dsn+"\n")
		if got := DefaultDBPath(); got != dsn {
			t.Fatalf("DefaultDBPath() = %q, want %q — a trailing newline is what "+
				"`export CLAUDE_MEM_DB=$(cat dsn.txt)` produces, and pgx rejects it", got, dsn)
		}
	})
}
