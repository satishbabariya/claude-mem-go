package privacy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// marshalNoEscape mirrors JSON.stringify's behavior of leaving literal `<`
// and `>` alone — Go's json.Marshal HTML-escapes them by default, which
// tag-stripping.ts's real payloads (produced by JSON.stringify, not Go)
// never do, so a test fixture built with plain json.Marshal wouldn't
// exercise the same literal-tag-inside-JSON shape a real hook payload has.
func marshalNoEscape(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("marshalNoEscape: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// Every case here mirrors a real assertion from real claude-mem's own
// tests/utils/tag-stripping.test.ts, run directly against that file while
// porting this package, not re-derived from reading tag-stripping.ts alone.
func TestStripMemoryTagsBasicRemoval(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"single private", "public content <private>secret stuff</private> more public", "public content  more public"},
		{"single claude-mem-context", "public content <claude-mem-context>injected context</claude-mem-context> more public", "public content  more public"},
		{"mixed private and context", "<private>secret</private> public <claude-mem-context>context</claude-mem-context> end", "public  end"},
		{"persisted-output", "public <persisted-output>large output</persisted-output> after", "public  after"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripMemoryTags(c.input); got != c.want {
				t.Errorf("StripMemoryTags(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestStripMemoryTagsMultipleBlocks(t *testing.T) {
	input := "<private>first secret</private> middle <private>second secret</private> end"
	if got, want := StripMemoryTags(input), "middle  end"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	input2 := "<claude-mem-context>ctx1</claude-mem-context><claude-mem-context>ctx2</claude-mem-context> content"
	if got, want := StripMemoryTags(input2), "content"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripMemoryTagsManyInterleavedTags(t *testing.T) {
	var b strings.Builder
	b.WriteString("start")
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, " <private>p%d</private> <claude-mem-context>c%d</claude-mem-context>", i, i)
	}
	b.WriteString(" end")
	result := StripMemoryTags(b.String())
	if strings.Contains(result, "<private>") || strings.Contains(result, "<claude-mem-context>") {
		t.Errorf("result still contains a tag: %q", result)
	}
	if !strings.Contains(result, "start") || !strings.Contains(result, "end") {
		t.Errorf("result lost surrounding content: %q", result)
	}
}

func TestStripMemoryTagsEmptyAndPrivateOnly(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"entirely private", "<private>entire prompt is private</private>", ""},
		{"entirely context-tagged", "<claude-mem-context>all is context</claude-mem-context>", ""},
		{"no tags at all", "no tags here at all", "no tags here at all"},
		{"empty input", "", ""},
		{"whitespace-only after stripping", "<private>content</private>   <claude-mem-context>more</claude-mem-context>", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripMemoryTags(c.input); got != c.want {
				t.Errorf("StripMemoryTags(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestStripMemoryTagsPreservesSurroundingContent(t *testing.T) {
	if got, want := StripMemoryTags("keep this <private>remove this</private> and this"), "keep this  and this"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := StripMemoryTags(`code: const x = 1; <private>secret</private> more: { "key": "value" }`), `code: const x = 1;  more: { "key": "value" }`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := StripMemoryTags("line1\n<private>secret</private>\nline2"), "line1\n\nline2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripMemoryTagsMultilineTagContent(t *testing.T) {
	input := "public\n<private>\nmulti\nline\nsecret\n</private>\nend"
	if got, want := StripMemoryTags(input), "public\n\nend"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	input2 := "start\n<claude-mem-context>\n# Recent Activity\n- Item 1\n- Item 2\n</claude-mem-context>\nfinish"
	if got, want := StripMemoryTags(input2), "start\n\nfinish"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The RE2-based implementation has no backtracking, so this is really just
// confirming correctness at volume rather than guarding against the
// specific catastrophic-backtracking failure mode tag-stripping.ts's own
// version of this test exists to catch — but the 1-second budget and tag
// count this mirrors from the real test are kept identical.
func TestStripMemoryTagsManyTagsNoHang(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&b, "<private>secret%d</private> text%d ", i, i)
	}
	start := time.Now()
	result, count := StripTags(b.String())
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v, want < 1s", d)
	}
	if strings.Contains(result, "<private>") {
		t.Errorf("result still contains <private>: %q", result)
	}
	if count != 150 {
		t.Errorf("count = %d, want 150", count)
	}
	if count <= MaxTagCount {
		t.Fatalf("test setup invalid: count %d must exceed MaxTagCount %d to exercise the over-limit path", count, MaxTagCount)
	}
}

func TestStripMemoryTagsLargeSingleTagNoHang(t *testing.T) {
	input := "<private>" + strings.Repeat("x", 10000) + "</private> keep this"
	start := time.Now()
	result := StripMemoryTags(input)
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v, want < 1s", d)
	}
	if result != "keep this" {
		t.Errorf("got %q, want %q", result, "keep this")
	}
}

func TestStripMemoryTagsOnJSONStrings(t *testing.T) {
	t.Run("strips private from stringified JSON", func(t *testing.T) {
		raw := marshalNoEscape(t, map[string]string{
			"file_path": "/path/to/file",
			"content":   "<private>secret</private> public",
		})
		result := StripMemoryTags(raw)
		var parsed map[string]string
		if err := json.Unmarshal([]byte(result), &parsed); err != nil {
			t.Fatalf("stripped output is not valid JSON: %v (%s)", err, result)
		}
		if parsed["content"] != " public" {
			t.Errorf("content = %q, want %q", parsed["content"], " public")
		}
	})

	t.Run("strips claude-mem-context from JSON", func(t *testing.T) {
		raw := marshalNoEscape(t, map[string]string{"data": "<claude-mem-context>injected</claude-mem-context> real data"})
		result := StripMemoryTags(raw)
		var parsed map[string]string
		json.Unmarshal([]byte(result), &parsed)
		if parsed["data"] != " real data" {
			t.Errorf("data = %q, want %q", parsed["data"], " real data")
		}
	})

	t.Run("tool_input with tags", func(t *testing.T) {
		raw := marshalNoEscape(t, map[string]string{"command": "echo hello", "args": "<private>secret args</private>"})
		result := StripMemoryTags(raw)
		var parsed map[string]string
		json.Unmarshal([]byte(result), &parsed)
		if parsed["args"] != "" {
			t.Errorf("args = %q, want empty", parsed["args"])
		}
	})

	t.Run("empty JSON object unaffected", func(t *testing.T) {
		if got, want := StripMemoryTags("{}"), "{}"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("nested JSON structures", func(t *testing.T) {
		raw := marshalNoEscape(t, map[string]any{
			"outer": map[string]string{"inner": "<private>secret</private> visible"},
		})
		result := StripMemoryTags(raw)
		var parsed map[string]map[string]string
		json.Unmarshal([]byte(result), &parsed)
		if parsed["outer"]["inner"] != " visible" {
			t.Errorf("outer.inner = %q, want %q", parsed["outer"]["inner"], " visible")
		}
	})
}

func TestStripMemoryTagsSystemInstructionVariants(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"underscore variant", "user content <system_instruction>injected instructions</system_instruction> more content", "user content  more content"},
		{"underscore mixed with private", "<system_instruction>instructions</system_instruction> public <private>secret</private> end", "public  end"},
		{"underscore entirely", "<system_instruction>entire prompt is system instructions</system_instruction>", ""},
		{"hyphen variant", "user content <system-instruction>injected instructions</system-instruction> more content", "user content  more content"},
		{"both variants same prompt", "<system_instruction>underscore</system_instruction> middle <system-instruction>hyphen</system-instruction> end", "middle  end"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripMemoryTags(c.input); got != c.want {
				t.Errorf("StripMemoryTags(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestStripMemoryTagsSystemReminder(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"basic", "user content <system-reminder>CLAUDE.md contents here</system-reminder> more content", "user content  more content"},
		{"mixed with other tag types", "<system-reminder>reminder</system-reminder> public <private>secret</private> <claude-mem-context>ctx</claude-mem-context> end", "public   end"},
		{"entirely a reminder", "<system-reminder>entire content is a system reminder</system-reminder>", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripMemoryTags(c.input); got != c.want {
				t.Errorf("StripMemoryTags(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}

	// The realistic case real claude-mem's own test exists for: a Read
	// tool's output carrying an injected CLAUDE.md dump that itself
	// contains a nested claude-mem-context block.
	input := "Here is the file content.\n\n<system-reminder>\nContents of /project/src/CLAUDE.md:\n\n<claude-mem-context>\n# Recent Activity\n\n### Dec 14, 2025\n| ID | Time | Title |\n|-----|------|-------|\n| #123 | 11:30 PM | Some observation |\n</claude-mem-context>\n</system-reminder>"
	if got, want := StripMemoryTags(input), "Here is the file content."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripMemoryTagsPrivacyEnforcementIntegration(t *testing.T) {
	t.Run("entirely private triggers skip", func(t *testing.T) {
		cleaned := StripMemoryTags("<private>entirely private prompt</private>")
		if shouldSkip := cleaned == "" || strings.TrimSpace(cleaned) == ""; !shouldSkip {
			t.Errorf("cleaned = %q, want a value that triggers skip", cleaned)
		}
	})

	t.Run("partial private content preserved", func(t *testing.T) {
		cleaned := StripMemoryTags("<private>password123</private> Please help me with my code")
		if shouldSkip := cleaned == "" || strings.TrimSpace(cleaned) == ""; shouldSkip {
			t.Errorf("cleaned = %q, should not trigger skip", cleaned)
		}
		if want := "Please help me with my code"; strings.TrimSpace(cleaned) != want {
			t.Errorf("got %q, want %q", strings.TrimSpace(cleaned), want)
		}
	})
}
