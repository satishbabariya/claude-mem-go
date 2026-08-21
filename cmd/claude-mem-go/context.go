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

// cmdContext is the SessionStart hook that makes this project actually
// function as *memory*, not just an on-demand search tool: it reads recent
// observations for the current project and injects them as context Claude
// sees at the start of the session, unprompted. Everything else this
// project built (search, semantic-search, the MCP tools) requires someone
// to think to ask; this is the part that surfaces relevant past work
// automatically, which is the actual point of "claude-mem."
//
// Diagnostics go to a log file, never stdout — stdout here is real hook
// output Claude Code parses as JSON; a stray log line would corrupt it,
// exactly like cmdMCP's stdout constraint.
func cmdContext(args []string) int {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	limit := fs.Int("limit", 5, "how many recent observations to inject")
	fs.Parse(args)
	*limit = clampLimit(*limit, 5, 100)

	l := openLog("context.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Printf("FAILED parsing hook payload: %v", err)
		fmt.Println("{}")
		return 0
	}

	project := filepath.Base(in.Cwd)
	if project == "" || project == "." {
		l.Printf("no usable project from cwd=%q, skipping", in.Cwd)
		fmt.Println("{}")
		return 0
	}

	st, err := backend.Open(context.Background(), *dbPath, 0, 0)
	if err != nil {
		l.Printf("FAILED opening store at %s: %v", store.RedactDSN(*dbPath), err)
		fmt.Println("{}")
		return 0
	}
	defer st.Close()

	recent, err := st.RecentByProject(project, *limit)
	if err != nil {
		l.Printf("FAILED RecentByProject(%s): %v", project, err)
		fmt.Println("{}")
		return 0
	}
	if len(recent) == 0 {
		l.Printf("no prior observations for project=%s, nothing to inject", project)
		fmt.Println("{}")
		return 0
	}

	ctx := formatContext(recent)
	out := hookOutput{HookSpecificOutput: &hookSpecificOutput{
		HookEventName:     "SessionStart",
		AdditionalContext: ctx,
	}}
	enc, err := json.Marshal(out)
	if err != nil {
		l.Printf("FAILED marshaling output: %v", err)
		fmt.Println("{}")
		return 0
	}
	l.Printf("injected %d recent observations for project=%s (%d bytes)", len(recent), project, len(ctx))
	fmt.Println(string(enc))
	return 0
}
func formatContext(recent []store.SearchResult) string {
	var b strings.Builder
	b.WriteString("Relevant memory from previous sessions in this project:\n\n")
	for _, r := range recent {
		fmt.Fprintf(&b, "- %s", r.Observation.Title)
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, " — %s", r.Observation.Subtitle)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
