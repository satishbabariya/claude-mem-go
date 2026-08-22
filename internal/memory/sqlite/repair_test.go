package sqlite

import (
	"context"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestRepairFilePathsStripsRelativeEntriesAndKeepsObservations(t *testing.T) {
	st, _ := openTempStore(t)
	ctx := context.Background()
	mixed := memory.Observation{Type: "discovery", Title: "mixed", FilesRead: []string{"src/auth/token.go", "/abs/keep.go"}, FilesModified: []string{"relative.go"}}
	clean := memory.Observation{Type: "discovery", Title: "clean", FilesRead: []string{"/abs/only.go"}}
	other := memory.Observation{Type: "discovery", Title: "other project", FilesRead: []string{"rel.go"}}
	m, err := st.Insert(ctx, "s", "proj", "Read", "h1", mixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "s", "proj", "Read", "h2", clean, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "s", "elsewhere", "Read", "h3", other, 0); err != nil {
		t.Fatal(err)
	}

	n, err := st.RepairFilePaths(ctx, "proj", true)
	if err != nil || n != 1 {
		t.Fatalf("dry run in proj: n=%d err=%v, want 1", n, err)
	}
	if got, _ := st.ByIDs(ctx, []int64{m.ID}); len(got[0].Observation.FilesRead) != 2 {
		t.Fatal("dry run modified the row")
	}
	n, err = st.RepairFilePaths(ctx, "", false)
	if err != nil || n != 2 {
		t.Fatalf("apply everywhere: n=%d err=%v, want 2", n, err)
	}
	got, _ := st.ByIDs(ctx, []int64{m.ID})
	if len(got) != 1 {
		t.Fatal("the observation was deleted; only its relative paths should go")
	}
	if fr := got[0].Observation.FilesRead; len(fr) != 1 || fr[0] != "/abs/keep.go" {
		t.Fatalf("files_read = %v, want only the absolute entry", fr)
	}
	if fm := got[0].Observation.FilesModified; len(fm) != 0 {
		t.Fatalf("files_modified = %v, want empty", fm)
	}
	var stale int
	if err := st.db.QueryRow(`SELECT count(*) FROM observation_files WHERE path NOT LIKE '/%'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d relative entries left in the path index — the trigger only fires on insert, so repair must clean it", stale)
	}
	if n, _ := st.RepairFilePaths(ctx, "", true); n != 0 {
		t.Fatalf("second pass still finds %d rows; repair is not idempotent", n)
	}
}
