package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestObservationsForFileMatchesReadAndModified(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	readMatch, err := st.Insert(context.Background(), "s1", "proj", "Read", memory.ContentHash("s1", "Read", "1", "1"),
		memory.Observation{Type: "discovery", Title: "read match", FilesRead: []string{"main.go", "other.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	modifiedMatch, err := st.Insert(context.Background(), "s1", "proj", "Edit", memory.ContentHash("s1", "Edit", "2", "2"),
		memory.Observation{Type: "change", Title: "modified match", FilesModified: []string{"main.go"}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert(context.Background(), "s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "3", "3"),
		memory.Observation{Type: "discovery", Title: "unrelated file", FilesRead: []string{"unrelated.go"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.ObservationsForFile(context.Background(), "proj", "main.go", 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("ObservationsForFile(\"main.go\") returned %d results, want 2", len(results))
	}
	ids := map[int64]bool{results[0].ID: true, results[1].ID: true}
	if !ids[readMatch.ID] || !ids[modifiedMatch.ID] {
		t.Fatalf("results = %v, want both the files_read match (%d) and files_modified match (%d)", ids, readMatch.ID, modifiedMatch.ID)
	}
}

func TestObservationsForFileScopesToProjectAndExactPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert(context.Background(), "s1", "proj-a", "Read", memory.ContentHash("s1", "Read", "1", "1"),
		memory.Observation{Type: "discovery", Title: "in proj-a", FilesRead: []string{"shared.go"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := st.Insert(context.Background(), "s1", "proj-b", "Read", memory.ContentHash("s1", "Read", "2", "2"),
		memory.Observation{Type: "discovery", Title: "in proj-b", FilesRead: []string{"shared.go"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.ObservationsForFile(context.Background(), "proj-a", "shared.go", 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want exactly 1 (scoped to proj-a, not proj-b's same-named file)", len(results))
	}

	// A substring of a real filename must not match — "main.go" must not
	// match an observation that only mentions "not-main.go-really".
	if _, err := st.Insert(context.Background(), "s1", "proj-c", "Read", memory.ContentHash("s1", "Read", "3", "3"),
		memory.Observation{Type: "discovery", Title: "similar but not equal", FilesRead: []string{"not-main.go-really"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	noResults, err := st.ObservationsForFile(context.Background(), "proj-c", "main.go", 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(noResults) != 0 {
		t.Fatalf("ObservationsForFile(\"main.go\") matched a substring-only filename — want exact match only, got %d results", len(noResults))
	}
}

func TestObservationsForFileNoMatches(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if _, err := st.Insert(context.Background(), "s1", "proj", "Read", memory.ContentHash("s1", "Read", "1", "1"),
		memory.Observation{Type: "discovery", Title: "x", FilesRead: []string{"a.go"}}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.ObservationsForFile(context.Background(), "proj", "never-read.go", 10)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results for a file never mentioned, want 0", len(results))
	}
}
