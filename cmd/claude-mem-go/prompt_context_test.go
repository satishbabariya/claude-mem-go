package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"
)

// TestTruncateForLogIsRuneSafe: the preview must never split a multi-byte
// character. 79 ASCII bytes followed by a 3-byte rune straddles the
// 80-byte cut exactly.
func TestTruncateForLogIsRuneSafe(t *testing.T) {
	s := strings.Repeat("a", 79) + "日本語"
	got := truncateForLog(s)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateForLog produced invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", 79) + "…"; got != want {
		t.Fatalf("truncateForLog = %q, want %q (cut back to the rune boundary)", got, want)
	}
	if short := "short prompt"; truncateForLog(short) != short {
		t.Fatalf("truncateForLog altered a prompt under the cap")
	}
}

// runPromptContext drives cmdPromptContext end to end with payload on
// stdin, against an isolated HOME (for the log) and a socket path nothing
// listens on (so every worker call takes its documented best-effort
// failure path). -embed-model "" keeps Ollama out of it: prompt storage
// must work, and be tested, with the injection half disabled.
func runPromptContext(t *testing.T, dbPath, payload string, extra ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if _, err := w.WriteString(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	args := append([]string{"-db", dbPath, "-embed-model", "", "-socket", filepath.Join(home, "none.sock")}, extra...)
	_ = captureStdout(t, func() { cmdPromptContext(args) })
}

func promptPayload(cwd, prompt string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": "sess-1", "cwd": cwd, "transcript_path": "/tmp/t.jsonl",
		"hook_event_name": "UserPromptSubmit", "prompt": prompt,
	})
	return string(b)
}

func storedPrompts(t *testing.T, dbPath string) []memory.PromptResult {
	t.Helper()
	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	rows, err := st.SearchPrompts(context.Background(), "", "", 100, 0)
	if err != nil {
		t.Fatalf("SearchPrompts: %v", err)
	}
	return rows
}

// TestPromptContextStoresPromptsOnlyWhenEnabled: off by default — this is
// the one feature that persists the user's verbatim words, so nothing
// may be written unless -store-prompts or CLAUDE_MEM_STORE_PROMPTS=1
// says so.
func TestPromptContextStoresPromptsOnlyWhenEnabled(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "proj-x")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "pc.db")

	t.Setenv("CLAUDE_MEM_STORE_PROMPTS", "")
	runPromptContext(t, dbPath, promptPayload(cwd, "please remember this exact question"))
	if got := storedPrompts(t, dbPath); len(got) != 0 {
		t.Fatalf("prompt stored with the feature off: %+v", got)
	}

	runPromptContext(t, dbPath, promptPayload(cwd, "please remember this exact question"), "-store-prompts")
	got := storedPrompts(t, dbPath)
	if len(got) != 1 || got[0].Text != "please remember this exact question" || got[0].Project != "proj-x" || got[0].SessionID != "sess-1" || got[0].PromptNumber != 1 {
		t.Fatalf("with -store-prompts, stored = %+v, want exactly the submitted prompt", got)
	}

	// The env var form, and a prompt far under -min-prompt-len, which
	// must still be stored: too short to embed is not too short to keep.
	t.Setenv("CLAUDE_MEM_STORE_PROMPTS", "1")
	runPromptContext(t, dbPath, promptPayload(cwd, "yes"))
	got = storedPrompts(t, dbPath)
	if len(got) != 2 {
		t.Fatalf("CLAUDE_MEM_STORE_PROMPTS=1 did not store a short prompt: %+v", got)
	}
	if got[0].Text != "yes" || got[0].PromptNumber != 2 {
		t.Fatalf("second prompt = %+v, want \"yes\" as prompt #2 of sess-1", got[0])
	}
}

// TestPromptContextNeverStoresAWhollyPrivatePrompt and the partially
// private case below are the privacy contract from the issue: the write
// sits after privacy.StripMemoryTags and the session-private check, so a
// <private>…</private> span never reaches the store under any setting.
func TestPromptContextNeverStoresAWhollyPrivatePrompt(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "proj-x")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "pc.db")
	runPromptContext(t, dbPath, promptPayload(cwd, "<private>my api key is hunter2</private>"), "-store-prompts")
	if got := storedPrompts(t, dbPath); len(got) != 0 {
		t.Fatalf("a wholly private prompt was stored: %+v", got)
	}
}

func TestPromptContextStoresOnlyTheStrippedTextOfAPartiallyPrivatePrompt(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "proj-x")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "pc.db")
	runPromptContext(t, dbPath, promptPayload(cwd, "rotate the token <private>hunter2</private> before Friday"), "-store-prompts")
	got := storedPrompts(t, dbPath)
	if len(got) != 1 {
		t.Fatalf("stored %d prompts, want 1", len(got))
	}
	if strings.Contains(got[0].Text, "hunter2") || strings.Contains(got[0].Text, "<private>") {
		t.Fatalf("private span leaked into the store: %q", got[0].Text)
	}
	if !strings.Contains(got[0].Text, "rotate the token") || !strings.Contains(got[0].Text, "before Friday") {
		t.Fatalf("stripped text lost the public part: %q", got[0].Text)
	}
}

// An internal protocol payload and an excluded project are gates that run
// before the write too; both must leave the store empty.
func TestPromptContextHonoursEarlierGatesBeforeStoring(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "proj-x")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "pc.db")
	runPromptContext(t, dbPath, promptPayload(cwd, "<task-notification>done</task-notification>"), "-store-prompts")
	if got := storedPrompts(t, dbPath); len(got) != 0 {
		t.Fatalf("an internal protocol payload was stored as a prompt: %+v", got)
	}
	runPromptContext(t, dbPath, promptPayload(cwd, "a real question in an excluded project"), "-store-prompts", "-excluded-projects", "proj-*")
	if got := storedPrompts(t, dbPath); len(got) != 0 {
		t.Fatalf("a prompt from an excluded project was stored: %+v", got)
	}
}
