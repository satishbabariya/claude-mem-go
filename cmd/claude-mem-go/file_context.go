package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/store"
)

// cmdFileContext is the PreToolUse hook (matcher "Read"): injects whatever
// memory already exists about the SPECIFIC file about to be read, not the
// whole project — real claude-mem's own PreToolUse hook is literally named
// "file-context" for the same purpose. Verified empirically that PreToolUse
// supports the same hookSpecificOutput.additionalContext injection
// SessionStart uses, with a real distinctive marker round-tripped through
// an actual Read tool call, before writing any of this.
func cmdFileContext(args []string) int {
	fs := flag.NewFlagSet("file-context", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	limit := fs.Int("limit", 5, "how many prior observations about this file to inject")
	fs.Parse(args)
	*limit = clampLimit(*limit, 5, 100)

	l := openLog("file-context.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Printf("FAILED parsing hook payload: %v", err)
		fmt.Println("{}")
		return 0
	}
	if in.ToolName != "Read" {
		// Defensive: hooks.json's matcher already restricts this to Read,
		// but a hook must never assume its own registration is the only
		// thing that can invoke it.
		l.Printf("skip: tool_name=%q is not Read", in.ToolName)
		fmt.Println("{}")
		return 0
	}

	var toolInput struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(in.ToolInput, &toolInput); err != nil || toolInput.FilePath == "" {
		l.Printf("FAILED reading file_path from tool_input: %v (%s)", err, in.ToolInput)
		fmt.Println("{}")
		return 0
	}

	project := filepath.Base(in.Cwd)
	if project == "" || project == "." {
		project = filepath.Base(filepath.Dir(in.TranscriptPath))
	}

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		l.Printf("FAILED opening store at %s: %v", store.RedactDSN(*dbPath), err)
		fmt.Println("{}")
		return 0
	}
	defer st.Close()

	results, err := st.ObservationsForFile(project, toolInput.FilePath, *limit)
	if err != nil {
		l.Printf("FAILED ObservationsForFile(%s): %v", toolInput.FilePath, err)
		fmt.Println("{}")
		return 0
	}
	if len(results) == 0 {
		l.Printf("no prior observations for file=%s project=%s", toolInput.FilePath, project)
		fmt.Println("{}")
		return 0
	}

	ctx := formatFileContext(toolInput.FilePath, results)
	out := hookOutput{HookSpecificOutput: &hookSpecificOutput{
		HookEventName:     "PreToolUse",
		AdditionalContext: ctx,
	}}
	enc, err := json.Marshal(out)
	if err != nil {
		l.Printf("FAILED marshaling output: %v", err)
		fmt.Println("{}")
		return 0
	}
	l.Printf("injected %d observations for file=%s project=%s", len(results), toolInput.FilePath, project)
	fmt.Println(string(enc))
	return 0
}
func formatFileContext(filePath string, results []store.SearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Prior memory about %s:\n\n", filePath)
	for _, r := range results {
		fmt.Fprintf(&b, "- %s", r.Observation.Title)
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, " — %s", r.Observation.Subtitle)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
