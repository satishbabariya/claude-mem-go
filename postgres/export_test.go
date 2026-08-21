package postgres

import (
	"testing"

	"claude-mem-go/store"
)

func TestPostgresExportAllReturnsEverythingOldestFirst(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	first, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "a", project),
		store.Observation{Type: "discovery", Title: "first"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	second, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "b", project),
		store.Observation{Type: "discovery", Title: "second"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	rows, err := st.ExportAll(first.ID-1, 100)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	var gotFirst, gotSecond bool
	for i, r := range rows {
		if r.ID == first.ID {
			gotFirst = true
			if i > 0 {
				if rows[i-1].ID > r.ID {
					t.Fatalf("ExportAll not ordered by id ascending near row %d", i)
				}
			}
		}
		if r.ID == second.ID {
			gotSecond = true
		}
	}
	if !gotFirst || !gotSecond {
		t.Fatalf("ExportAll(afterID=%d) missing seeded rows: gotFirst=%v gotSecond=%v", first.ID-1, gotFirst, gotSecond)
	}
}

// TestPostgresImportRowCreatesANewRowWithPreservedFields constructs an
// ExportRow by hand (matching what a real export produces) rather than
// exporting a row this test itself just inserted — Postgres here is one
// persistent, shared instance across every test run (see uniqueProject's
// own doc comment), not a fresh database per test the way SQLite's temp
// file is, so "insert via Insert, then ImportRow the export of that same
// row into the SAME database" would just find its own already-existing
// row via content_hash and correctly (but unhelpfully, for this test)
// report Inserted=false. Building the row by hand isolates ImportRow's
// actual write path — restoring a row into a database that has never
// seen it — from that shared-instance artifact.
func TestPostgresImportRowCreatesANewRowWithPreservedFields(t *testing.T) {
	pg := openTestStore(t)
	project := uniqueProject(t)
	row := store.ExportRow{
		SessionID:   "s1",
		Project:     project,
		ToolName:    "Bash",
		ContentHash: store.ContentHash("s1", "Bash", "x", project),
		Observation: store.Observation{
			Type: "discovery", Title: "exported observation", Subtitle: "sub",
			Facts: []string{"fact one", "fact two"}, Narrative: "what happened",
			Concepts: []string{"concept"}, FilesRead: []string{"a.go"}, FilesModified: []string{"b.go"},
		},
		CostUSD:        0.05,
		CreatedAt:      "2020-01-01T00:00:00Z",
		CreatedAtEpoch: 1577836800000,
	}

	res, err := pg.ImportRow(row)
	if err != nil {
		t.Fatalf("ImportRow: %v", err)
	}
	if !res.Inserted {
		t.Fatal("ImportRow of a genuinely new row: want Inserted=true")
	}

	restored, err := pg.RecentByProject(project, 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("RecentByProject after import returned %d rows, want 1", len(restored))
	}
	if restored[0].Observation.Title != row.Observation.Title || len(restored[0].Observation.Facts) != 2 {
		t.Fatalf("restored observation = %+v, want it to match %+v", restored[0].Observation, row.Observation)
	}
}

// TestExportSQLiteImportPostgres is the real cross-backend property this
// feature exists for, not just same-backend backup: export from a fresh
// SQLite store and import into the live Postgres container, confirming
// export/import doubles as the SQLite<->Postgres migration path.
func TestExportSQLiteImportPostgres(t *testing.T) {
	sqliteDB, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("sqlite Open: %v", err)
	}
	defer sqliteDB.Close()

	project := uniqueProject(t)
	o := store.Observation{Type: "discovery", Title: "migrated from sqlite", FilesRead: []string{"main.go"}}
	if _, err := sqliteDB.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "migrate", project), o, 0.02); err != nil {
		t.Fatalf("sqlite Insert: %v", err)
	}

	rows, err := sqliteDB.ExportAll(0, 100)
	if err != nil {
		t.Fatalf("sqlite ExportAll: %v", err)
	}
	var toMigrate *store.ExportRow
	for i := range rows {
		if rows[i].Project == project {
			toMigrate = &rows[i]
		}
	}
	if toMigrate == nil {
		t.Fatalf("sqlite ExportAll didn't include the seeded row for project %s", project)
	}

	pg := openTestStore(t)
	res, err := pg.ImportRow(*toMigrate)
	if err != nil {
		t.Fatalf("postgres ImportRow: %v", err)
	}
	if !res.Inserted {
		t.Fatal("postgres ImportRow: want Inserted=true")
	}

	found, err := pg.ObservationsForFile(project, "main.go", 10)
	if err != nil {
		t.Fatalf("postgres ObservationsForFile: %v", err)
	}
	if len(found) != 1 || found[0].Observation.Title != o.Title {
		t.Fatalf("row migrated from SQLite not found correctly in Postgres: %v", found)
	}
}

// TestExportSQLiteImportPostgresPreservesEmbedding is the regression test
// for a real gap in export/import's first version: ExportRow carried no
// embedding field, so migrating from SQLite to Postgres — supposedly the
// path to "real ANN search at scale" — silently arrived with nothing left
// to search. Uses a real embedding value round-tripped through both
// backends, not a mocked one.
func TestExportSQLiteImportPostgresPreservesEmbedding(t *testing.T) {
	sqliteDB, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("sqlite Open: %v", err)
	}
	defer sqliteDB.Close()

	project := uniqueProject(t)
	res, err := sqliteDB.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "embed-migrate", project),
		store.Observation{Type: "discovery", Title: "row with an embedding to migrate"}, 0)
	if err != nil {
		t.Fatalf("sqlite Insert: %v", err)
	}
	vec := make([]float32, DefaultEmbedDims)
	vec[0] = 1
	if err := sqliteDB.SaveEmbedding(res.ID, vec); err != nil {
		t.Fatalf("sqlite SaveEmbedding: %v", err)
	}

	rows, err := sqliteDB.ExportAll(0, 100)
	if err != nil {
		t.Fatalf("sqlite ExportAll: %v", err)
	}
	var toMigrate *store.ExportRow
	for i := range rows {
		if rows[i].Project == project {
			toMigrate = &rows[i]
		}
	}
	if toMigrate == nil {
		t.Fatalf("sqlite ExportAll didn't include the seeded row for project %s", project)
	}
	if len(toMigrate.Embedding) != DefaultEmbedDims {
		t.Fatalf("exported row's Embedding has %d dims, want %d — the embedding wasn't exported at all", len(toMigrate.Embedding), DefaultEmbedDims)
	}

	pg := openTestStore(t)
	importRes, err := pg.ImportRow(*toMigrate)
	if err != nil {
		t.Fatalf("postgres ImportRow: %v", err)
	}

	query := make([]float32, DefaultEmbedDims)
	query[0] = 1
	matches, err := pg.SemanticSearch(project, query, 10)
	if err != nil {
		t.Fatalf("postgres SemanticSearch: %v", err)
	}
	found := false
	for _, m := range matches {
		if m.ID == importRes.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("SemanticSearch in Postgres after migrating from SQLite didn't find the row — the embedding didn't survive the migration: %+v", matches)
	}
}

// TestPostgresImportRowRejectsAnUnrecognizedObservationType is this
// backend's half of the regression test for a real schema-completeness
// gap: nothing anywhere validated Observation.Type before this. insertRow
// is shared between Insert and ImportRow here too — exactly the kind of
// sharing a future refactor could accidentally break for just one of the
// two callers, so both need their own coverage.
func TestPostgresImportRowRejectsAnUnrecognizedObservationType(t *testing.T) {
	pg := openTestStore(t)
	project := uniqueProject(t)

	_, err := pg.ImportRow(store.ExportRow{
		SessionID:      "s1",
		Project:        project,
		ToolName:       "Bash",
		ContentHash:    store.ContentHash("s1", "Bash", "bad-type", project),
		Observation:    store.Observation{Type: "bugfix", Title: "x"},
		CreatedAt:      "2020-01-01T00:00:00Z",
		CreatedAtEpoch: 1577836800000,
	})
	if err == nil {
		t.Fatal("ImportRow with an unrecognized type: want an error, got nil")
	}
}
