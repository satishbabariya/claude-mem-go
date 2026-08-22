package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestPruneCutoffUnitsMatchInsertsRealTimestamp is the regression test for
// a real bug: cmd/claude-mem-go/main.go's cmdPrune originally computed its
// cutoff with time.Now().AddDate(...).Unix() (seconds), but Insert stamps
// created_at_epoch with now.UnixMilli() (milliseconds) — a ~1000x unit
// mismatch that made a seconds-based cutoff always smaller than any real
// row's timestamp, so prune silently deleted nothing, ever, for any
// reasonable -older-than-days value.
//
// This went unnoticed by every other Prune test in this package because
// they all backdate created_at_epoch by hand to small, unit-agnostic
// values (setCreatedAtEpoch) — correct for testing Prune's own SQL logic,
// but blind to a caller computing the cutoff in the wrong unit. This test
// uses Insert's REAL timestamp instead, the same way the CLI actually
// does, so a unit mismatch between Insert and a cutoff computation shows
// up here.
func TestPruneCutoffUnitsMatchInsertsRealTimestamp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "1"),
		Observation{Type: "discovery", Title: "inserted with a real timestamp"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// A cutoff of "1 day from now," in the SAME unit Insert actually
	// stamps (milliseconds) — the exact computation cmdPrune performs
	// (time.Now().AddDate(0, 0, N).UnixMilli()). If Prune's caller instead
	// used seconds here (the original bug), this cutoff would be ~1000x
	// too small and the row below would never be found.
	cutoff := time.Now().AddDate(0, 0, 1).UnixMilli()

	n, err := st.Prune("", cutoff, true)
	if err != nil {
		t.Fatalf("Prune (dry run): %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune found %d rows older than tomorrow (in milliseconds), want 1 — "+
			"a real Insert-stamped row not being found points at a unit mismatch between "+
			"Insert's created_at_epoch and the cutoff passed to Prune", n)
	}
}
