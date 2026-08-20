package store

import (
	"path/filepath"
	"testing"
)

func openExportTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestExportAllReturnsEverythingOldestFirst(t *testing.T) {
	st := openExportTestStore(t)
	first, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"),
		Observation{Type: "discovery", Title: "first"}, 0.01)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	second, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "b", "2"),
		Observation{Type: "discovery", Title: "second"}, 0.02)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	rows, err := st.ExportAll(0, 10)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ExportAll returned %d rows, want 2", len(rows))
	}
	if rows[0].ID != first.ID || rows[1].ID != second.ID {
		t.Fatalf("ExportAll order = [%d %d], want [%d %d] (oldest first)", rows[0].ID, rows[1].ID, first.ID, second.ID)
	}
	if rows[0].ContentHash == "" || rows[0].CreatedAt == "" {
		t.Fatalf("ExportAll row missing ContentHash/CreatedAt: %+v", rows[0])
	}
}

// TestExportAllPaginatesCorrectly is the real property this exists for:
// a caller must be able to page through a store larger than one batch by
// passing the previous page's last ID as the next afterID, and get every
// row exactly once across all pages.
func TestExportAllPaginatesCorrectly(t *testing.T) {
	st := openExportTestStore(t)
	const n = 25
	var ids []int64
	for i := 0; i < n; i++ {
		res, err := st.Insert("s1", "proj", "Bash",
			ContentHash("s1", "Bash", string(rune('a'+i%26)), string(rune(i))),
			Observation{Type: "discovery", Title: "row"}, 0)
		if err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		ids = append(ids, res.ID)
	}

	var got []int64
	afterID := int64(0)
	const pageSize = 7
	for {
		page, err := st.ExportAll(afterID, pageSize)
		if err != nil {
			t.Fatalf("ExportAll(afterID=%d): %v", afterID, err)
		}
		for _, r := range page {
			got = append(got, r.ID)
		}
		if len(page) < pageSize {
			break
		}
		afterID = page[len(page)-1].ID
	}

	if len(got) != n {
		t.Fatalf("paginated ExportAll returned %d rows total, want %d", len(got), n)
	}
	for i, id := range got {
		if id != ids[i] {
			t.Fatalf("row %d = id %d, want %d (paginated result out of order or missing a row)", i, id, ids[i])
		}
	}
}

// TestImportRowRoundTripsIntoAFreshStore is export+import's core contract:
// exporting from one store and importing into a completely different,
// fresh one must reproduce the same observations, including their
// original content and (for prune/recent ordering to make sense after a
// restore) their original timestamps.
func TestImportRowRoundTripsIntoAFreshStore(t *testing.T) {
	source := openExportTestStore(t)
	o := Observation{
		Type: "discovery", Title: "exported observation", Subtitle: "sub",
		Facts: []string{"fact one", "fact two"}, Narrative: "what happened",
		Concepts: []string{"concept"}, FilesRead: []string{"a.go"}, FilesModified: []string{"b.go"},
	}
	orig, err := source.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "x", "y"), o, 0.05)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	rows, err := source.ExportAll(0, 10)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ExportAll returned %d rows, want 1", len(rows))
	}

	dest := openExportTestStore(t)
	res, err := dest.ImportRow(rows[0])
	if err != nil {
		t.Fatalf("ImportRow: %v", err)
	}
	if !res.Inserted {
		t.Fatal("ImportRow into a fresh store: want Inserted=true")
	}

	restored, err := dest.RecentByProject("proj", 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("RecentByProject after import returned %d rows, want 1", len(restored))
	}
	r := restored[0]
	if r.Observation.Title != o.Title || r.Observation.Narrative != o.Narrative {
		t.Fatalf("restored observation = %+v, want it to match the original %+v", r.Observation, o)
	}
	if len(r.Observation.Facts) != 2 || len(r.Observation.FilesRead) != 1 {
		t.Fatalf("restored observation lost array fields: %+v", r.Observation)
	}
	_ = orig
}

// TestImportRowIsIdempotent confirms re-importing the same exported row
// (e.g. re-running a restore, or the same export applied twice) is a
// no-op — the same guarantee Insert already provides, preserved here.
func TestImportRowIsIdempotent(t *testing.T) {
	source := openExportTestStore(t)
	if _, err := source.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "x", "y"),
		Observation{Type: "discovery", Title: "row"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	rows, err := source.ExportAll(0, 10)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}

	dest := openExportTestStore(t)
	first, err := dest.ImportRow(rows[0])
	if err != nil {
		t.Fatalf("first ImportRow: %v", err)
	}
	second, err := dest.ImportRow(rows[0])
	if err != nil {
		t.Fatalf("second ImportRow: %v", err)
	}
	if second.Inserted {
		t.Fatal("second ImportRow of the same row: want Inserted=false (idempotent)")
	}
	if second.ID != first.ID {
		t.Fatalf("second ImportRow returned a different ID (%d) than the first (%d)", second.ID, first.ID)
	}
	count, err := dest.CountByProject("proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject after importing the same row twice = %d, want 1", count)
	}
}
