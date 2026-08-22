package sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// TestNextStepsRoundTrip covers the whole field: written on insert, read
// back by the SessionStart query, and preserved by export. Export matters
// specifically because this project has already shipped a silent
// data-loss bug of exactly that shape — a field that was captured and
// then quietly dropped by the migration path.
func TestNextStepsRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	want := []string{"wire the retry budget into the client", "delete the dead flag"}
	if _, err := st.Insert("s1", "proj", "SessionSummary",
		memory.ContentHash("s1", "SessionSummary", "summary", ""),
		memory.Observation{Type: "summary", Title: "session summary", NextSteps: want}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	recent, err := st.RecentByProject("proj", 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("RecentByProject returned %d rows, want 1", len(recent))
	}
	if got := recent[0].Observation.NextSteps; !equalStrings(got, want) {
		t.Errorf("NextSteps from RecentByProject = %v, want %v", got, want)
	}

	rows, err := st.ExportAll(0, 10)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ExportAll returned %d rows, want 1", len(rows))
	}
	if got := rows[0].Observation.NextSteps; !equalStrings(got, want) {
		t.Errorf("NextSteps from ExportAll = %v, want %v — the field is captured but lost on migration", got, want)
	}
}

// TestNextStepsDefaultsEmpty pins that an ordinary per-tool-call
// observation, which never carries next steps, reads back as empty rather
// than as a spurious entry — the SessionStart injection keys off
// non-emptiness, so a stray value would put a phantom "unfinished" block
// at the top of every session.
func TestNextStepsDefaultsEmpty(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert("s1", "proj", "Bash",
		memory.ContentHash("s1", "Bash", "ordinary", ""),
		memory.Observation{Type: "discovery", Title: "ordinary observation"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	recent, err := st.RecentByProject("proj", 10)
	if err != nil {
		t.Fatalf("RecentByProject: %v", err)
	}
	if got := recent[0].Observation.NextSteps; len(got) != 0 {
		t.Errorf("NextSteps = %v for an observation that never had any, want empty", got)
	}
}

// TestParseXMLExtractsNextSteps covers the capture end: the
// summary prompt asks for <next_steps><step>, and nothing else in the
// pipeline would notice if the parser ignored it — the field would simply
// always be empty, looking exactly like "the session finished everything".
func TestParseXMLExtractsNextSteps(t *testing.T) {
	out := `<observation>
  <type>summary</type>
  <title>a session</title>
  <narrative>did things</narrative>
  <next_steps>
    <step>finish the migration</step>
    <step>re-run the soak</step>
  </next_steps>
</observation>`
	o, err := memory.ParseXML(out)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	want := []string{"finish the migration", "re-run the soak"}
	if !equalStrings(o.NextSteps, want) {
		t.Errorf("NextSteps = %v, want %v", o.NextSteps, want)
	}

	// An ordinary turn has no such block and must yield nothing.
	plain, err := memory.ParseXML(`<observation><type>change</type><title>x</title></observation>`)
	if err != nil {
		t.Fatalf("ParseXML (plain): %v", err)
	}
	if len(plain.NextSteps) != 0 {
		t.Errorf("NextSteps = %v for output with no <next_steps> block, want empty", plain.NextSteps)
	}
}

// TestNextStepsMigrationOnPreexistingStore is the one that matters for
// anyone who already has a store: the column is added by migration 7, and
// rows written before it must survive and read back as empty rather than
// failing to scan.
func TestNextStepsMigrationOnPreexistingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash",
		memory.ContentHash("s1", "Bash", "old", ""),
		memory.Observation{Type: "discovery", Title: "written before next_steps existed"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Simulate a store created by an older build: BOTH the column and its
	// migration record must go. Dropping only the column leaves a state no
	// real store ever reaches — schema_migrations claiming version 7 is
	// applied while the column it adds is absent — and every migration
	// framework skips already-recorded versions, so that would test a
	// scenario that cannot happen rather than the upgrade that can.
	if _, err := st.db.Exec(`ALTER TABLE observations DROP COLUMN next_steps`); err != nil {
		t.Fatalf("dropping the column to simulate an older store: %v", err)
	}
	if _, err := st.db.Exec(`DELETE FROM schema_migrations WHERE version = 7`); err != nil {
		t.Fatalf("removing the migration record to simulate an older store: %v", err)
	}
	st.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a store without next_steps must migrate it, got: %v", err)
	}
	defer reopened.Close()

	recent, err := reopened.RecentByProject("proj", 10)
	if err != nil {
		t.Fatalf("RecentByProject after migration: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("got %d rows after migration, want the pre-existing 1", len(recent))
	}
	if !strings.Contains(recent[0].Observation.Title, "before next_steps") {
		t.Errorf("the pre-existing row did not survive: %+v", recent[0].Observation)
	}
	if len(recent[0].Observation.NextSteps) != 0 {
		t.Errorf("a row predating the column has NextSteps = %v, want empty", recent[0].Observation.NextSteps)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
