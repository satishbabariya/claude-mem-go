package transcript

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeFixture writes lines (already-JSON-encoded, one per line) to a temp
// file and returns its path.
func writeFixture(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestParsePairsToolUseWithToolResult(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file1.txt\nfile2.txt"}]}}`,
	}
	path := writeFixture(t, lines)

	calls, err := Parse(path, 10)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("Parse returned %d calls, want 1", len(calls))
	}
	if calls[0].ToolName != "Bash" {
		t.Fatalf("ToolName = %q, want Bash", calls[0].ToolName)
	}
	if !strings.Contains(calls[0].ToolInput, "ls -la") {
		t.Fatalf("ToolInput = %q, want it to contain the command", calls[0].ToolInput)
	}
	if calls[0].ToolOutput != "file1.txt\nfile2.txt" {
		t.Fatalf("ToolOutput = %q, want the plain-string content", calls[0].ToolOutput)
	}
}

func TestParseHandlesArrayShapedToolResultContent(t *testing.T) {
	// tool_result.content is sometimes an array of {type:"text"} blocks
	// instead of a plain string — both forms appear in real transcripts.
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file":"a.go"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"package main"}]}]}}`,
	}
	path := writeFixture(t, lines)

	calls, err := Parse(path, 10)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolOutput != "package main" {
		t.Fatalf("got %+v", calls)
	}
}

func TestParseSkipsUnmatchedAndMalformedLines(t *testing.T) {
	lines := []string{
		`not json at all`,
		`{"type":"summary","summary":"nothing to see here"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"echo hi"}}]}}`,
		// no matching tool_result for toolu_3 — should be left unpaired, not error
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_never_seen","content":"orphan result"}]}}`,
	}
	path := writeFixture(t, lines)

	calls, err := Parse(path, 10)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("Parse returned %d calls, want 0 (tool_use with no matching result, malformed lines skipped)", len(calls))
	}
}

func TestParseRespectsLimit(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"one"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"r1"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"two"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"r2"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"three"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"r3"}]}}`,
	}
	path := writeFixture(t, lines)

	calls, err := Parse(path, 2)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("Parse with limit=2 returned %d calls", len(calls))
	}
	if calls[0].ToolOutput != "r1" || calls[1].ToolOutput != "r2" {
		t.Fatalf("got %+v, want the first two pairs in order", calls)
	}
}

func TestTruncateLongFields(t *testing.T) {
	long := strings.Repeat("x", FieldCap+500)
	got := Truncate(long)
	if len(got) <= FieldCap {
		t.Fatalf("Truncate result length %d, want it to exceed FieldCap (marker text appended)", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", FieldCap)) {
		t.Fatal("Truncate should keep the first FieldCap bytes verbatim")
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("Truncate result should note truncation happened, got %q", got[FieldCap:])
	}

	short := "short string"
	if Truncate(short) != short {
		t.Fatalf("Truncate should leave short strings unchanged, got %q", Truncate(short))
	}
}

func TestParseNonexistentFile(t *testing.T) {
	if _, err := Parse("/no/such/file.jsonl", 10); err == nil {
		t.Fatal("Parse on a missing file: want an error, got nil")
	}
}

func TestToolCallFieldsAreExported(t *testing.T) {
	// Compile-time-ish sanity check that ToolCall's shape didn't drift —
	// cheap insurance against an accidental field rename breaking every
	// caller silently at the zero-value level.
	tc := ToolCall{ToolName: "x", ToolInput: "y", ToolOutput: "z"}
	if !reflect.DeepEqual(tc, ToolCall{ToolName: "x", ToolInput: "y", ToolOutput: "z"}) {
		t.Fatal("ToolCall literal round-trip failed")
	}
}
