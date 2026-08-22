package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// TestEveryReadPathPopulatesCreatedAtEpoch is the guard the field's own
// doc comment promises: a field only some queries fill is worse than no
// field, because a zero reads as "1970" rather than as "unknown".
func TestEveryReadPathPopulatesCreatedAtEpoch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	before := time.Now().Add(-time.Minute).UnixMilli()
	res, err := st.Insert("s1", "p", "Bash", memory.ContentHash("s1", "Bash", "t", "1"),
		memory.Observation{Type: "change", Title: "epoch check", FilesRead: []string{"/tmp/f.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	checks := map[string]func() ([]memory.SearchResult, error){
		"Search":              func() ([]memory.SearchResult, error) { return st.Search("p", "epoch", "", 10, 0, 0, 0, "") },
		"Search(enumerate)":   func() ([]memory.SearchResult, error) { return st.Search("p", "", "", 10, 0, 0, 0, "") },
		"RecentByProject":     func() ([]memory.SearchResult, error) { return st.RecentByProject("p", 10) },
		"BySessionID":         func() ([]memory.SearchResult, error) { return st.BySessionID("s1", 10) },
		"ObservationsForFile": func() ([]memory.SearchResult, error) { return st.ObservationsForFile("p", "/tmp/f.go", 10) },
		"ByIDs":               func() ([]memory.SearchResult, error) { return st.ByIDs([]int64{res.ID}) },
		"Timeline":            func() ([]memory.SearchResult, error) { return st.Timeline("p", res.ID, 2, 2) },
	}
	for name, fn := range checks {
		got, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) == 0 {
			t.Fatalf("%s returned no rows; cannot verify the field", name)
		}
		for _, r := range got {
			if r.CreatedAtEpoch < before {
				t.Errorf("%s: CreatedAtEpoch = %d, want a real timestamp (>= %d). "+
					"A zero here silently means 1970 and would make the file-context staleness check "+
					"treat every observation as ancient.", name, r.CreatedAtEpoch, before)
			}
		}
	}
}
