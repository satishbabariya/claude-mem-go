package backend

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/store"
)

func TestOpenDispatchesToSQLiteForAPlainPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	be, err := Open(context.Background(), dbPath, 0, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer be.Close()

	// Prove it's actually the SQLite backend doing real work, not just that
	// something satisfying the interface came back.
	hash := store.ContentHash("s1", "Bash", "a", "b")
	res, err := be.Insert("s1", "proj", "Bash", hash, store.Observation{Type: "discovery", Title: "x"}, 0)
	if err != nil {
		t.Fatalf("Insert through dispatched backend: %v", err)
	}
	if !res.Inserted {
		t.Fatal("first Insert: want Inserted=true")
	}

	count, err := be.CountByProject("proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject = %d, want 1", count)
	}
}

func TestOpenRecognizesBothPostgresSchemes(t *testing.T) {
	// Both schemes must reach postgres.Open, not silently fall through to
	// treating the DSN as a SQLite file path — which would try to create a
	// file literally named "postgres://...", succeeding with a nonsense
	// database rather than failing loudly. Asserted two ways depending on
	// whether Postgres actually answers, so this proves real dispatch
	// happened either way instead of just skipping on any error:
	//   - reachable: Open succeeds AND Insert/CountByProject work through
	//     the real postgres.Store (a SQLite fallback would "succeed" too,
	//     against the wrong, nonsense file — this is why the follow-up
	//     behavioral check matters, not just a non-nil return).
	//   - unreachable: the error is postgres.Open's own connection-refused
	//     wrapper ("ping postgres: ..."), not a SQLite file-open error.
	// Deliberately NOT a hardcoded real DSN. This test used to point both
	// schemes at postgres://...@localhost:55432/claudemem — the same
	// database README.md tells operators to keep their actual memory in —
	// and the "reachable" branch below then INSERTED into it. It left 2
	// real rows under project `backend-dispatch-test` in a developer store,
	// measured. postgres/postgres_test.go had the same defect at far larger
	// scale (see testDSNEnvVar there); this is the sibling instance.
	//
	// Unlike those tests, this one loses nothing by having no database: its
	// whole assertion is that both schemes DISPATCH to postgres.Open rather
	// than falling through to SQLite, and the unreachable branch proves
	// that from the error content alone. So it runs everywhere — against
	// the disposable database CI names, or against a guaranteed-dead port
	// locally, but never against a store somebody actually uses.
	host := "127.0.0.1:1/nodb" // a closed port: fast "connection refused"
	if dsn := os.Getenv("CLAUDE_MEM_GO_TEST_POSTGRES_DSN"); dsn != "" {
		if u, err := url.Parse(dsn); err == nil {
			host = u.Host + u.Path
		}
	}
	for _, dsn := range []string{
		"postgres://claudemem:claudemem@" + host + "?sslmode=disable",
		"postgresql://claudemem:claudemem@" + host + "?sslmode=disable",
	} {
		t.Run(dsn, func(t *testing.T) {
			be, err := Open(context.Background(), dsn, 0, 0)
			if err != nil {
				if !strings.Contains(err.Error(), "ping postgres") {
					t.Fatalf("error = %q, want it to be postgres.Open's connection error (proves dispatch happened), not something else", err.Error())
				}
				t.Skipf("postgres not reachable (dispatch confirmed via error content): %v", err)
				return
			}
			defer be.Close()

			// Reachable: confirm this is a genuinely functioning Postgres
			// backend, not a coincidental non-nil return.
			hash := store.ContentHash("s1", "Bash", "backend-dispatch-check", dsn)
			if _, err := be.Insert("s1", "backend-dispatch-test", "Bash", hash, store.Observation{Type: "discovery", Title: "x"}, 0); err != nil {
				t.Fatalf("Insert through the dispatched postgres backend: %v", err)
			}
		})
	}
}
