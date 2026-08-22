package postgres

import (
	"context"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestPostgresRepairFilePathsStripsRelativeEntries(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	proj := uniqueProject(t)
	mixed := memory.Observation{Type: "discovery", Title: "mixed", FilesRead: []string{"src/a.go", "/abs/keep.go"}, FilesModified: []string{"rel.go"}}
	m, err := st.Insert(ctx, "s", proj, "Read", proj+"-h1", mixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "s", proj, "Read", proj+"-h2", memory.Observation{Type: "discovery", Title: "clean", FilesRead: []string{"/abs/x.go"}}, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RepairFilePaths(ctx, proj, true); err != nil || n != 1 {
		t.Fatalf("dry run: n=%d err=%v", n, err)
	}
	if n, err := st.RepairFilePaths(ctx, proj, false); err != nil || n != 1 {
		t.Fatalf("apply: n=%d err=%v", n, err)
	}
	got, err := st.ByIDs(ctx, []int64{m.ID})
	if err != nil || len(got) != 1 {
		t.Fatalf("observation missing after repair: %v", err)
	}
	if fr := got[0].Observation.FilesRead; len(fr) != 1 || fr[0] != "/abs/keep.go" {
		t.Fatalf("files_read = %v", fr)
	}
	if n, _ := st.RepairFilePaths(ctx, proj, true); n != 0 {
		t.Fatalf("not idempotent: %d left", n)
	}
}
