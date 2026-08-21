// Package transcript reads real Claude Code session transcripts
// (~/.claude/projects/<cwd-slug>/<session-id>.jsonl) and extracts
// tool_use/tool_result pairs — the same data claude-mem's live PostToolUse
// hook receives per-call, but recovered here from the file Claude Code
// itself writes every line to. Not part of claude-agent-sdk-go: transcript
// parsing has nothing to do with the query() protocol.
//
// Verified against real transcripts on disk (see the go-observer-spike
// project this was ported from), not guessed from docs:
//
//	{"type":"assistant","message":{"content":[
//	  {"type":"tool_use","id":"toolu_...","name":"Bash","input":{"command":"..."}}
//	]}}
//	{"type":"user","message":{"content":[
//	  {"type":"tool_result","tool_use_id":"toolu_...","content":"<string, or
//	     an array of {"type":"text","text":"..."} blocks>"}
//	]}}
package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// FieldCap bounds each field this package extracts. Real claude-mem clamps
// large fields too (cloud-sync.mdx notes a 200KB per-field clamp) — this is
// the same idea at a much smaller scale, since a tool output can be
// arbitrarily large and this is meant to feed an LLM prompt.
const FieldCap = 1500

// truncate cuts s to at most max bytes at a real UTF-8 rune boundary, not
// a raw byte offset. Found the hard way, not anticipated: a plain `s[:max]`
// produces invalid UTF-8 whenever a multi-byte rune (any non-ASCII
// character — accented file paths, emoji, box-drawing characters from
// `tree`/`ls` output, non-English text) happens to straddle the cut point,
// confirmed directly against a real multi-byte string. This runs on every
// single real tool_input/tool_response the worker daemon ever processes
// (see worker.go's process()), so "happens to straddle" isn't a rare edge
// case over that much real traffic — it's an eventual certainty. Walking
// back to the nearest rune-start byte trims at most 3 extra bytes (the
// longest UTF-8 encoding is 4 bytes) to guarantee the result is always
// valid UTF-8.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("...[truncated %d bytes]", len(s)-cut)
}

// Truncate applies FieldCap to s. Exported so callers building a ToolCall
// from a source other than a transcript file (e.g. worker's live hook
// payloads) truncate large fields the same way Parse does.
func Truncate(s string) string { return truncate(s, FieldCap) }

// ToolCall is one completed tool_use/tool_result pair — the raw material an
// observation gets built from. Deliberately distinct from a persisted
// observation (store.Observation): this is what the model reads, not what
// it produces.
type ToolCall struct {
	ToolName   string
	ToolInput  string
	ToolOutput string
}

type line struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock covers both shapes pulled out of message.content[]: a
// tool_use block (id/name/input) and a tool_result block (tool_use_id +
// content, where content itself can be a plain string or another array of
// {type:"text"} blocks — both forms appear in real transcripts).
type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Text      string          `json:"text"`
}

func extractResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Parse scans a real Claude Code JSONL transcript top to bottom, pairing
// each tool_use with the tool_result that carries its id, and returns the
// first `limit` completed pairs in the order they occurred. A tool_use with
// no result yet (the last turn of an in-progress session) is left unpaired
// — claude-mem only ever observes a tool call once its result exists.
//
// Tolerant like claude-mem's own transcript-parser.ts: any line that isn't
// valid JSON, or doesn't have this shape (session markers, plain-text
// turns, subagent sidechains), is silently skipped rather than treated as
// an error.
func Parse(path string, limit int) ([]ToolCall, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	pending := make(map[string]struct{ name, input string })
	var out []ToolCall

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // transcript lines can be large
	for scanner.Scan() {
		if len(out) >= limit {
			break
		}
		var ln line
		if json.Unmarshal(scanner.Bytes(), &ln) != nil {
			continue
		}
		if len(ln.Message.Content) == 0 {
			continue
		}
		var blocks []contentBlock
		if json.Unmarshal(ln.Message.Content, &blocks) != nil {
			continue
		}

		switch ln.Type {
		case "assistant":
			for _, b := range blocks {
				if b.Type == "tool_use" && b.ID != "" {
					pending[b.ID] = struct{ name, input string }{b.Name, string(b.Input)}
				}
			}
		case "user":
			for _, b := range blocks {
				if b.Type != "tool_result" || b.ToolUseID == "" {
					continue
				}
				call, ok := pending[b.ToolUseID]
				if !ok {
					continue
				}
				delete(pending, b.ToolUseID)
				out = append(out, ToolCall{
					ToolName:   call.name,
					ToolInput:  truncate(call.input, FieldCap),
					ToolOutput: truncate(extractResultText(b.Content), FieldCap),
				})
				if len(out) >= limit {
					break
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return out, fmt.Errorf("scan transcript: %w", err)
	}
	return out, nil
}

// FindMostRecent locates the most recently modified transcript under
// ~/.claude/projects/*/*.jsonl — the same directory Claude Code itself
// writes to.
func FindMostRecent() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no transcripts found under ~/.claude/projects/*/*.jsonl")
	}
	sort.Slice(matches, func(i, j int) bool {
		si, erri := os.Stat(matches[i])
		sj, errj := os.Stat(matches[j])
		if erri != nil || errj != nil {
			return false
		}
		return si.ModTime().After(sj.ModTime())
	})
	return matches[0], nil
}
