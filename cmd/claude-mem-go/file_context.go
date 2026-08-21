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
	"claude-mem-go/excludeproject"
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
	excludedProjects := fs.String("excluded-projects", "", "comma-separated glob patterns (supports *, **, ?, and a leading ~) — "+
		"a matching project gets no automatic file-context injection, the real claude-mem CLAUDE_MEM_EXCLUDED_PROJECTS feature; "+
		"empty (the default) excludes nothing")
	fs.Parse(args)
	*limit = clampLimit(*limit, 5, 100)

	l := openLog("file-context.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Printf("FAILED parsing hook payload: %v", err)
		fmt.Println("{}")
		return 0
	}
	if in.AgentID != "" || in.AgentType != "" {
		l.Printf("skip: subagent context detected (agent_id=%s agent_type=%s)", in.AgentID, in.AgentType)
		fmt.Println("{}")
		return 0
	}
	if excludeproject.IsExcluded(in.Cwd, *excludedProjects) {
		l.Printf("skip: project excluded (cwd=%s)", in.Cwd)
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

	st, err := backend.Open(context.Background(), *dbPath, 0, 0)
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

	// Don't inject memory that predates the file's current contents.
	//
	// This hook fires immediately before Claude reads a file, and says
	// "here's what we already know about it." If the file has been
	// rewritten since the newest of those observations was recorded, then
	// everything being injected describes a version that no longer
	// exists — and it is being asserted as current, right at the moment
	// Claude is about to form an impression of the file. Stale memory
	// presented confidently is worse than no memory: Claude is about to
	// read the real contents anyway, so suppressing this costs nothing
	// and injecting it can actively mislead.
	//
	// Real claude-mem's own file-context handler makes exactly this
	// comparison (buildFileContextTimeline: "File modified since last
	// observation, skipping context injection"). This port had no way to
	// even ask — SearchResult carried no timestamp until now.
	if mtimeMs, ok := fileMtimeMs(in.Cwd, toolInput.FilePath); ok {
		newest := int64(0)
		for _, r := range results {
			if r.CreatedAtEpoch > newest {
				newest = r.CreatedAtEpoch
			}
		}
		if newest > 0 && mtimeMs >= newest {
			l.Printf("skip: file=%s modified at %d, after the newest of %d observation(s) (%d) — "+
				"what we remember describes an older version of this file",
				toolInput.FilePath, mtimeMs, len(results), newest)
			fmt.Println("{}")
			return 0
		}
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

// fileMtimeMs returns filePath's modification time in Unix epoch
// milliseconds, resolving a relative path against cwd. The bool reports
// whether the answer is usable.
//
// Every failure returns false, which means "inject anyway" — the caller
// only suppresses on a definite answer. That direction is deliberate and
// matches the reasoning the Stop hook's privacy check already uses: an
// unknown signal must never be treated as a reason to withhold. A file
// this hook cannot stat (permissions, a path shape it does not
// understand, a race with a delete) is not evidence that the memory is
// stale, and silently dropping context on it would be a much harder
// failure to notice than injecting slightly-old context.
func fileMtimeMs(cwd, filePath string) (int64, bool) {
	path := filePath
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	// A directory has an mtime, but comparing it against observations
	// about a file is meaningless — and a directory's mtime changes
	// whenever anything inside it does, so this would suppress context
	// almost always.
	if !fi.Mode().IsRegular() {
		return 0, false
	}
	return fi.ModTime().UnixMilli(), true
}
