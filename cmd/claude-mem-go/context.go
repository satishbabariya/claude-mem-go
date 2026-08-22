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
	"claude-mem-go/hook"
	"claude-mem-go/store"
	"claude-mem-go/worker"
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
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "worker daemon's unix socket, told how many observations this "+
		"recall returned so the read path is observable (best-effort — never affects this hook's output)")
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
	// The most consequential recall outcome in the system: an empty result
	// here means the session began with no memory at all. Best-effort and
	// never surfaced — see hook.ReportRecall.
	if err := hook.ReportRecall(*socketPath, hook.RecallSession, len(recent)); err != nil {
		l.Debugf("skip: could not report recall outcome to the worker: %v", err)
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
	// Unfinished work goes FIRST and separately, because it is the one
	// thing here that is not merely context. Everything below this block
	// records what happened; these are the things that had not happened
	// yet when the last session ended, which is what a new session most
	// needs to know before deciding what to do.
	//
	// Taken from the most recent observation that carries any — in
	// practice the newest session summary, since only the summary prompt
	// asks for them. Deliberately not merged across sessions: next steps
	// from three sessions ago were most likely done, and presenting stale
	// intentions as current is worse than omitting them.
	if steps := latestNextSteps(recent); len(steps) > 0 {
		b.WriteString("Unfinished from the last session in this project:\n\n")
		for _, st := range steps {
			fmt.Fprintf(&b, "- %s\n", st)
		}
		b.WriteString("\n")
	}
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

// latestNextSteps returns the next steps from the newest observation that
// has any. recent is already newest-first, so the first hit is the most
// recent — and only that one is used, rather than accumulating every
// session's leftovers into a growing list of things probably long done.
func latestNextSteps(recent []store.SearchResult) []string {
	for _, r := range recent {
		if len(r.Observation.NextSteps) > 0 {
			return r.Observation.NextSteps
		}
	}
	return nil
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
