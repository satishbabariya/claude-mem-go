package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// TestValidateObservationTypeAcceptsTheRealVocabulary confirms every type
// value this project's own code actually produces (the observer prompt's
// discovery/change/decision, the Stop hook's summary, and
// add_observation's manual literal) is accepted — a regression here would
// make every real ingestion path start failing, not just reject garbage.
func TestValidateObservationTypeAcceptsTheRealVocabulary(t *testing.T) {
	for _, valid := range []string{"discovery", "change", "decision", "summary", "manual"} {
		if err := memory.ValidateObservationType(valid); err != nil {
			t.Errorf("ValidateObservationType(%q): %v, want nil — this is real vocabulary this project's own code produces", valid, err)
		}
	}
}

// TestValidateObservationTypeRejectsUnknownValues is the regression test
// for a real schema-completeness gap found by hand: nothing anywhere
// validated memory.Observation.Type before this, in either backend's schema or
// Go code — an LLM's <type> tag (or a corrupted/hand-edited import file)
// producing an unrecognized value would have silently persisted,
// invisible to any -type/type filter with no error anywhere.
func TestValidateObservationTypeRejectsUnknownValues(t *testing.T) {
	for _, invalid := range []string{"", "Discovery", "DISCOVERY", "bugfix", "note", " discovery"} {
		if err := memory.ValidateObservationType(invalid); err == nil {
			t.Errorf("ValidateObservationType(%q): want an error, got nil", invalid)
		}
	}
}

// TestInsertRejectsAnUnrecognizedObservationType confirms the validation
// is actually wired into the real ingestion boundary (Insert), not just
// correct in isolation — a real regression would be validating
// correctly but forgetting to call it from insertRow.
func TestInsertRejectsAnUnrecognizedObservationType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	_, err = st.Insert("s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "b"),
		memory.Observation{Type: "bugfix", Title: "x"}, 0)
	if err == nil {
		t.Fatal("Insert with an unrecognized type: want an error, got nil")
	}

	count, cerr := st.CountByProject("proj")
	if cerr != nil {
		t.Fatalf("CountByProject: %v", cerr)
	}
	if count != 0 {
		t.Fatalf("CountByProject = %d, want 0 — the rejected row must not have been persisted", count)
	}
}

// TestImportRowRejectsAnUnrecognizedObservationType confirms the same
// validation applies to ImportRow, the OTHER real ingestion boundary —
// insertRow is shared between Insert and ImportRow, but that sharing is
// exactly the kind of thing a future refactor could accidentally break
// for just one of the two callers.
func TestImportRowRejectsAnUnrecognizedObservationType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	_, err = st.ImportRow(memory.ExportRow{
		SessionID:   "s1",
		Project:     "proj",
		ToolName:    "Bash",
		ContentHash: memory.ContentHash("s1", "Bash", "a", "b"),
		Observation: memory.Observation{Type: "bugfix", Title: "x"},
		CreatedAt:   "2026-01-01T00:00:00Z",
	})
	if err == nil {
		t.Fatal("ImportRow with an unrecognized type: want an error, got nil")
	}
}
