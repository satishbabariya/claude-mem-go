package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"
)

func TestExportThenImportRoundTripsThroughACLIFile(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	src, err := sqlite.Open(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if _, err := src.Insert(context.Background(), "s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"),
		memory.Observation{Type: "discovery", Title: "cli round trip"}, 0.03); err != nil {
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

	dest, err := sqlite.Open(context.Background(), destPath)
	if err != nil {
		t.Fatalf("Open dest: %v", err)
	}
	defer dest.Close()
	count, err := dest.CountByProject(context.Background(), "proj")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject(dest) after export+import = %d, want 1", count)
	}
}

func TestImportIsIdempotentAcrossTwoRuns(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	src, err := sqlite.Open(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if _, err := src.Insert(context.Background(), "s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"),
		memory.Observation{Type: "discovery", Title: "idempotent import"}, 0); err != nil {
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

	dest, err := sqlite.Open(context.Background(), destPath)
	if err != nil {
		t.Fatalf("Open dest: %v", err)
	}
	defer dest.Close()
	count, err := dest.CountByProject(context.Background(), "proj")
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

// TestExportThenImportCarriesPromptsAndStaysIdempotent: a backup that
// silently dropped stored prompts would be a data-loss bug of exactly the
// kind the embedding omission was. Prompt rows travel tagged
// "kind":"prompt" after the observation rows; a second import of the
// same file is a no-op for both kinds.
func TestExportThenImportCarriesPromptsAndStaysIdempotent(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	src, err := sqlite.Open(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if _, err := src.Insert(context.Background(), "s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "a", "1"),
		memory.Observation{Type: "discovery", Title: "with prompts"}, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	for _, text := range []string{"first question", "second question"} {
		if _, err := src.InsertPrompt(context.Background(), "s1", "proj", text); err != nil {
			t.Fatalf("InsertPrompt: %v", err)
		}
	}
	src.Close()

	exportPath := filepath.Join(t.TempDir(), "export.jsonl")
	if rc := cmdExport([]string{"-db", sourcePath, "-out", exportPath}); rc != 0 {
		t.Fatalf("cmdExport exit code = %d, want 0", rc)
	}
	raw, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("export has %d lines, want 3 (1 observation + 2 prompts):\n%s", len(lines), raw)
	}
	if strings.Contains(lines[0], `"kind"`) {
		t.Fatalf("observation row carries a kind (must stay backward compatible): %s", lines[0])
	}
	if !strings.Contains(lines[1], `"kind":"prompt"`) || !strings.Contains(lines[2], `"kind":"prompt"`) {
		t.Fatalf("prompt rows not tagged: %s / %s", lines[1], lines[2])
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	for i := 0; i < 2; i++ {
		if rc := cmdImport([]string{"-db", destPath, "-in", exportPath}); rc != 0 {
			t.Fatalf("cmdImport #%d exit code = %d, want 0", i+1, rc)
		}
	}
	dest, err := sqlite.Open(context.Background(), destPath)
	if err != nil {
		t.Fatalf("Open dest: %v", err)
	}
	defer dest.Close()
	prompts, err := dest.PromptsBySession(context.Background(), "proj", "s1", 10)
	if err != nil {
		t.Fatalf("PromptsBySession: %v", err)
	}
	if len(prompts) != 2 || prompts[0].Text != "first question" || prompts[1].Text != "second question" {
		t.Fatalf("imported prompts = %+v, want the two originals in order, once", prompts)
	}
	count, err := dest.CountByProject(context.Background(), "proj")
	if err != nil || count != 1 {
		t.Fatalf("CountByProject = %d, %v; want 1", count, err)
	}
}
