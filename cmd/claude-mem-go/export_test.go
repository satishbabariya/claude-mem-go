package main

import (
	"path/filepath"
	"testing"

	"claude-mem-go/store"
)

func TestExportThenImportRoundTripsThroughACLIFile(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	src, err := store.Open(sourcePath)
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if _, err := src.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "a", "1"),
		store.Observation{Type: "discovery", Title: "cli round trip"}, 0.03); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	src.Close()

	exportPath := filepath.Join(t.TempDir(), "export.jsonl")
	if rc := cmdExport([]string{"-db", sourcePath, "-out", exportPath}); rc != 0 {
		t.Fatalf("cmdExport exit code = %d, want 0", rc)
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	if rc := cmdImport([]string{"-db", destPath, "-in", exportPath}); rc != 0 {
		t.Fatalf("cmdImport exit code = %d, want 0", rc)
	}

	dest, err := store.Open(destPath)
	if err != nil {
		t.Fatalf("Open dest: %v", err)
	}
	defer dest.Close()
	count, err := dest.CountByProject("proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject(dest) after export+import = %d, want 1", count)
	}
}

func TestImportIsIdempotentAcrossTwoRuns(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	src, err := store.Open(sourcePath)
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if _, err := src.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "a", "1"),
		store.Observation{Type: "discovery", Title: "idempotent import"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	src.Close()

	exportPath := filepath.Join(t.TempDir(), "export.jsonl")
	if rc := cmdExport([]string{"-db", sourcePath, "-out", exportPath}); rc != 0 {
		t.Fatalf("cmdExport exit code = %d, want 0", rc)
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	if rc := cmdImport([]string{"-db", destPath, "-in", exportPath}); rc != 0 {
		t.Fatalf("first cmdImport exit code = %d, want 0", rc)
	}
	if rc := cmdImport([]string{"-db", destPath, "-in", exportPath}); rc != 0 {
		t.Fatalf("second cmdImport exit code = %d, want 0", rc)
	}

	dest, err := store.Open(destPath)
	if err != nil {
		t.Fatalf("Open dest: %v", err)
	}
	defer dest.Close()
	count, err := dest.CountByProject("proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject(dest) after importing the same file twice = %d, want 1 (idempotent)", count)
	}
}

func TestImportRequiresInFlag(t *testing.T) {
	if rc := cmdImport(nil); rc != 2 {
		t.Fatalf("cmdImport with no -in exit code = %d, want 2 (usage error)", rc)
	}
}
