package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"
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
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	res, err := st.Insert("s1", project, "Read", memory.ContentHash("s1", "Read", filePath, "1"),
		memory.Observation{Type: "discovery", Title: "the parser used a regex here", FilesRead: []string{filePath}}, 0)
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

// TestFileContextInjectsAcrossSessionsNotJustTheLatest is the end-to-end
// version of memory.TestSelectFileContextRecoversOlderSessions, through
// the real hook: the wiring matters as much as the selection function,
// because the hook previously queried exactly `limit` rows, which left
// selection nothing to select from.
func TestFileContextInjectsAcrossSessionsNotJustTheLatest(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Base(dir)
	target := filepath.Join(dir, "auth.go")
	if err := os.WriteFile(target, []byte("package auth\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// Keep the file older than the observations so the staleness gate
	// (a separate feature, tested above) does not suppress injection.
	past := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(target, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "fc.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ins := func(sess, title string) {
		if _, err := st.Insert(sess, project, "Read", memory.ContentHash(sess, "Read", title, target),
			memory.Observation{Type: "change", Title: title, FilesRead: []string{target}}, 0); err != nil {
			t.Fatalf("Insert %q: %v", title, err)
		}
	}
	ins("sess-old", "auth.go: never log the raw token")
	for i := 1; i <= 6; i++ {
		ins("sess-today", "today step "+string(rune('0'+i))+": tweaked a helper")
	}
	st.Close()

	out, logOut := runFileContext(t, dbPath, payloadFor(dir, target))

	if !strings.Contains(out, "never log the raw token") {
		t.Fatalf("the older session's durable fact was crowded out by today's repeated touches — "+
			"which is exactly the cross-session memory this product exists to provide.\nstdout: %s\nlog: %s", out, logOut)
	}
	if strings.Count(out, "tweaked a helper") > 1 {
		t.Fatalf("more than one observation from the same session was injected:\n%s", out)
	}
}
