package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/excludeproject"
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
	excludedProjects := fs.String("excluded-projects", "", "comma-separated glob patterns (supports *, **, ?, and a leading ~) — "+
		"a matching project gets no automatic context injection, the real claude-mem CLAUDE_MEM_EXCLUDED_PROJECTS feature; "+
		"empty (the default) excludes nothing")
	fs.Parse(args)
	*limit = clampLimit(*limit, 5, 100)

	l := openLog("context.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Errorf("FAILED parsing hook payload: %v", err)
		fmt.Println("{}")
		return 0
	}
	if excludeproject.IsExcluded(in.Cwd, *excludedProjects) {
		l.Printf("skip: project excluded (cwd=%s)", in.Cwd)
		fmt.Println("{}")
		return 0
	}

	pc := store.ProjectContextFor(in.Cwd)
	project := pc.Primary
	if project == "" || project == "." {
		l.Printf("no usable project from cwd=%q, skipping", in.Cwd)
		fmt.Println("{}")
		return 0
	}

	st, err := backend.Open(context.Background(), *dbPath, 0, 0)
	if err != nil {
		l.Errorf("FAILED opening store at %s: %v", store.RedactDSN(*dbPath), err)
		fmt.Println("{}")
		return 0
	}
	defer st.Close()

	// Read across every project this working directory maps to, not just
	// the one writes go to. For an ordinary checkout that is exactly one
	// name and this behaves as before; for a git WORKTREE it is the
	// parent repository as well, because a worktree is a branch of the
	// same work and a session started in it should still see what the
	// repository already knows. Writes still go to the worktree's own
	// composite name, so the two stay distinguishable — this only widens
	// the read. Real claude-mem does the same (context.ts injects over
	// getProjectContext(cwd).allProjects, not .primary).
	recent, err := recentAcrossProjects(st, pc.AllProjects, *limit)
	if err != nil {
		l.Errorf("FAILED RecentByProject(%v): %v", pc.AllProjects, err)
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
		l.Errorf("FAILED marshaling output: %v", err)
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

// recentAcrossProjects returns the newest `limit` observations across
// every given project, merged and re-sorted newest-first.
//
// Merging in Go rather than widening the Backend interface with an
// IN-clause variant: this is the only caller that needs more than one
// project, the list is at most two (a worktree and its parent), and a
// per-project query is one indexed lookup each. An interface change would
// touch both backends to serve one call site.
//
// Each project is fetched at the full limit and the merge trims, so a
// worktree with plenty of its own history is not forced to give up half
// its slots to the parent — the newest observations win regardless of
// which project they came from.
func recentAcrossProjects(st store.Backend, projects []string, limit int) ([]store.SearchResult, error) {
	if len(projects) <= 1 {
		p := ""
		if len(projects) == 1 {
			p = projects[0]
		}
		return st.RecentByProject(p, limit)
	}
	var all []store.SearchResult
	seen := make(map[int64]bool)
	for _, p := range projects {
		rs, err := st.RecentByProject(p, limit)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			// A row can only belong to one project, but dedupe by id
			// anyway: it costs nothing and makes this safe if the project
			// list ever contains a duplicate.
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			all = append(all, r)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAtEpoch > all[j].CreatedAtEpoch })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
