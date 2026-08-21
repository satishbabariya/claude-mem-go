package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-mem-go/store"
)

// runFileContext drives cmdFileContext end to end and returns (stdout,
// log). Unlike the Stop hook, this one's stdout IS its product — Claude
// Code reads it — so both matter: the log says why, stdout says what was
// injected.
func runFileContext(t *testing.T, dbPath, payload string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if _, err := inW.WriteString(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	inW.Close()
	origIn := os.Stdin
	os.Stdin = inR
	defer func() { os.Stdin = origIn }()

	out := captureStdout(t, func() { cmdFileContext([]string{"-db", dbPath}) })

	logBytes, _ := os.ReadFile(filepath.Join(home, ".claude-mem-go", "file-context.log"))
	return out, string(logBytes)
}

// seedFileObservation writes one observation about filePath, recorded
// now. The comparison is then steered from the FILE side with
// os.Chtimes rather than by backdating the row: the store has no
// test-only setter for created_at_epoch, and adding one to its public API
// to serve a test would be a worse trade than moving the file's mtime,
// which is the exact signal under test anyway.
func seedFileObservation(t *testing.T, project, filePath string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fc.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	res, err := st.Insert("s1", project, "Read", store.ContentHash("s1", "Read", filePath, "1"),
		store.Observation{Type: "discovery", Title: "the parser used a regex here", FilesRead: []string{filePath}}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if res.ID == 0 {
		t.Fatal("Insert returned id 0")
	}
	return dbPath
}

func payloadFor(cwd, filePath string) string {
	return `{"session_id":"s1","cwd":"` + cwd + `","transcript_path":"/tmp/t.jsonl",` +
		`"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"` + filePath + `"}}`
}

// TestFileContextSkipsWhenTheFileIsNewerThanTheMemory is the regression
// test for a real correctness problem, not a cost one. This hook fires
// immediately before Claude reads a file and asserts "here's what we know
// about it". If the file was rewritten after the newest observation, all
// of that describes a version that no longer exists — and it is presented
// as current, right as Claude forms its impression.
func TestFileContextSkipsWhenTheFileIsNewerThanTheMemory(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Base(dir)
	target := filepath.Join(dir, "parser.go")
	if err := os.WriteFile(target, []byte("package p // rewritten since\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	dbPath := seedFileObservation(t, project, target)
	// Move the file's mtime an hour AFTER the observation was recorded:
	// the file has been rewritten since anything we remember about it.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	out, logOut := runFileContext(t, dbPath, payloadFor(dir, target))

	if strings.Contains(out, "the parser used a regex here") {
		t.Fatalf("injected memory that predates the file's current contents:\n%s", out)
	}
	if !strings.Contains(logOut, "older version of this file") {
		t.Fatalf("no staleness skip in the log:\n%s", logOut)
	}
}

// TestFileContextInjectsWhenTheMemoryIsNewerThanTheFile is the
// counterweight, and the one that matters most: a staleness check that
// suppressed everything would pass the test above perfectly while
// silently disabling file-context injection altogether.
func TestFileContextInjectsWhenTheMemoryIsNewerThanTheFile(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Base(dir)
	target := filepath.Join(dir, "parser.go")
	if err := os.WriteFile(target, []byte("package p\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	dbPath := seedFileObservation(t, project, target)
	// Move the file's mtime an hour BEFORE the observation: what we
	// remember was recorded against the contents on disk right now.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(target, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	out, logOut := runFileContext(t, dbPath, payloadFor(dir, target))

	if !strings.Contains(out, "the parser used a regex here") {
		t.Fatalf("current memory was NOT injected — the staleness gate is suppressing valid context.\nstdout: %s\nlog: %s", out, logOut)
	}
}

// TestFileContextInjectsWhenTheFileCannotBeStatted pins the fail-open
// direction. An unknown mtime is not evidence of staleness, and silently
// dropping context for an unstattable path would be far harder to notice
// than injecting slightly-old context.
func TestFileContextInjectsWhenTheFileCannotBeStatted(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Base(dir)
	missing := filepath.Join(dir, "deleted.go")
	dbPath := seedFileObservation(t, project, missing)

	out, logOut := runFileContext(t, dbPath, payloadFor(dir, missing))

	if !strings.Contains(out, "the parser used a regex here") {
		t.Fatalf("context was suppressed for a file that could not be statted; unknown must fail open.\nstdout: %s\nlog: %s", out, logOut)
	}
}
