// claude-mem-go is the application built on claude-agent-sdk-go: it wires
// together transcript ingestion, an observer session, error classification,
// SQLite persistence, and the worker-daemon/hook-client split proven
// necessary by go-observer-spike's real hook tests.
//
// Subcommands:
//
//	claude-mem-go worker   — run the persistent daemon (usually launched by `start`, not run directly)
//	claude-mem-go start    — idempotent: spawn a detached worker if one isn't already running (use from SessionStart)
//	claude-mem-go hook     — thin PostToolUse client: forward stdin to the daemon, exit
//	claude-mem-go ingest   — one-shot: read a real transcript, observe N tool calls, persist them
//	claude-mem-go search           — full-text (keyword) search over persisted observations (FTS5)
//	claude-mem-go semantic-search  — meaning-based search via local Ollama embeddings + cosine similarity
//	claude-mem-go mcp              — MCP server exposing search/semantic-search as tools (stdio transport)
//	claude-mem-go context         — SessionStart hook: inject recent memory for this project as context
//	claude-mem-go stop            — Stop hook: synthesize and persist a session-level summary observation
//	claude-mem-go doctor          — check that the claude CLI, worker, database, and Ollama are all reachable
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/hook"
	"claude-mem-go/mcpserver"
	"claude-mem-go/observer"
	"claude-mem-go/store"
	"claude-mem-go/transcript"
	"claude-mem-go/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "worker":
		os.Exit(cmdWorker(os.Args[2:]))
	case "start":
		os.Exit(cmdStart(os.Args[2:]))
	case "hook":
		os.Exit(cmdHook(os.Args[2:]))
	case "ingest":
		os.Exit(cmdIngest(os.Args[2:]))
	case "search":
		os.Exit(cmdSearch(os.Args[2:]))
	case "semantic-search":
		os.Exit(cmdSemanticSearch(os.Args[2:]))
	case "mcp":
		os.Exit(cmdMCP(os.Args[2:]))
	case "context":
		os.Exit(cmdContext(os.Args[2:]))
	case "stop":
		os.Exit(cmdStop(os.Args[2:]))
	case "doctor":
		os.Exit(cmdDoctor(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: claude-mem-go <worker|hook|ingest> [flags]")
}

func openLog(name string) *log.Logger {
	f, err := os.OpenFile(store.DefaultHome()+"/"+name,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return log.New(os.Stderr, "", log.LstdFlags)
	}
	return log.New(f, "", log.LstdFlags)
}

func cmdWorker(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(empty to skip embedding — observations are still persisted, just not semantically searchable)")
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket to listen on")
	maxConcurrent := fs.Int("max-concurrent", 2, "max concurrent observer sessions")
	fs.Parse(args)

	d := &worker.Daemon{
		Model:         *model,
		EmbedModel:    *embedModel,
		DBPath:        *dbPath,
		SocketPath:    *socketPath,
		MaxConcurrent: *maxConcurrent,
		Log:           openLog("worker.log"),
	}

	// Graceful shutdown: SIGTERM/SIGINT cancel the context Run() watches,
	// which closes the listener cleanly (see worker.Daemon.Run) instead of
	// the process just dying mid-accept and leaving the socket file behind
	// for the next launcher to clean up.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := d.Run(ctx); err != nil {
		if ctx.Err() != nil {
			d.Log.Printf("daemon shut down cleanly on signal")
			return 0
		}
		d.Log.Printf("daemon exited: %v", err)
		return 1
	}
	return 0
}

// cmdStart is the idempotent SessionStart entry point: mirrors
// worker-service.cjs's "start" plus worker-spawn-gate.ts's mutual exclusion.
// Safe to invoke from every session's SessionStart hook, concurrently.
func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings (empty to skip)")
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker listens on")
	maxConcurrent := fs.Int("max-concurrent", 2, "max concurrent observer sessions")
	fs.Parse(args)

	l := openLog("start.log")

	if worker.IsRunning(*socketPath) {
		l.Printf("worker already running at %s, nothing to do", *socketPath)
		return 0
	}

	lockPath := *socketPath + ".lock"
	if !worker.AcquireSpawnLock(lockPath) {
		l.Printf("another launcher is already starting the worker, waiting for it")
		waitForReady(*socketPath, l)
		return 0
	}
	defer worker.ReleaseSpawnLock(lockPath)

	// The winner of the race above might have finished between our first
	// IsRunning check and acquiring the lock — recheck before spawning a
	// redundant second worker.
	if worker.IsRunning(*socketPath) {
		l.Printf("worker became ready while we were acquiring the spawn lock")
		return 0
	}

	self, err := os.Executable()
	if err != nil {
		l.Printf("FAILED to resolve our own executable path: %v", err)
		return 1
	}
	workerArgs := []string{
		"worker",
		"-model", *model,
		"-embed-model", *embedModel,
		"-db", *dbPath,
		"-socket", *socketPath,
		"-max-concurrent", strconv.Itoa(*maxConcurrent),
	}
	if err := worker.SpawnDetached(self, workerArgs); err != nil {
		l.Printf("FAILED to spawn worker: %v", err)
		return 1
	}
	l.Printf("spawned a detached worker, waiting for it to become ready")
	waitForReady(*socketPath, l)
	return 0
}

func waitForReady(socketPath string, l *log.Logger) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if worker.IsRunning(socketPath) {
			l.Printf("worker is ready at %s", socketPath)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	l.Printf("WARNING: worker did not become ready within 5s")
}

func cmdHook(args []string) int {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker daemon listens on")
	fs.Parse(args)

	l := openLog("hook.log")
	n, err := hook.Forward(*socketPath, os.Stdin)
	if err != nil {
		// A missing/unreachable daemon must never surface as a Claude
		// Code-visible hook failure — log loudly, exit clean.
		l.Printf("FAILED forwarding to worker at %s: %v (is `claude-mem-go worker` running?)", *socketPath, err)
		return 0
	}
	l.Printf("forwarded %d bytes to worker, exiting immediately", n)
	return 0
}

func cmdIngest(args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	transcriptPath := fs.String("transcript", "", "transcript .jsonl path; "+
		"defaults to the most recently modified one under ~/.claude/projects/*/*.jsonl")
	limit := fs.Int("limit", 3, "how many real tool_use/tool_result pairs to ingest")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(empty to skip embedding — observations are still persisted, just not semantically searchable)")
	fs.Parse(args)

	tp := *transcriptPath
	if tp == "" {
		found, err := transcript.FindMostRecent()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED to find a transcript: %v\n", err)
			return 1
		}
		tp = found
	}
	fmt.Printf("transcript: %s\n", tp)

	calls, err := transcript.Parse(tp, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to parse transcript: %v\n", err)
		return 1
	}
	if len(calls) == 0 {
		fmt.Fprintf(os.Stderr, "FAILED: found zero tool_use/tool_result pairs in %s\n", tp)
		return 1
	}
	fmt.Printf("ingested %d real tool_use/tool_result pairs\n\n", len(calls))

	obs, err := observer.New(context.Background(), *model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to start observer: %v\n", err)
		return 1
	}
	defer obs.Close()

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	project := filepath.Base(filepath.Dir(tp))

	for i, tc := range calls {
		turn, err := observer.ObserveResilient(context.Background(), obs, *model, tc, observer.DefaultRetryPolicy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "turn %d FAILED: %v\n", i+1, err)
			return 1
		}
		fmt.Printf("turn %d: session=%s cost=$%.4f title=%q\n",
			i+1, turn.Result.SessionID, turn.Result.CostUSD, turn.Observation.Title)

		hash := store.ContentHash(turn.Result.SessionID, tc.ToolName, tc.ToolInput, tc.ToolOutput)
		res, err := st.Insert(turn.Result.SessionID, project, tc.ToolName, hash, turn.Observation, turn.Result.CostUSD)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: sqlite insert failed: %v\n", err)
			continue
		}
		if !res.Inserted {
			fmt.Printf("  -> already ingested as observations.id=%d (same tool call, skipped duplicate)\n", res.ID)
			continue
		}
		fmt.Printf("  -> persisted as observations.id=%d\n", res.ID)

		if *embedModel == "" {
			continue
		}
		text := embed.ObservationText(turn.Observation.Title, turn.Observation.Subtitle,
			turn.Observation.Narrative, turn.Observation.Facts)
		vec, err := embed.NewClient(*embedModel).Embed(text)
		if err != nil {
			// Embedding is additive — keyword search (already persisted above)
			// still works without it. A missing/unreachable Ollama must not
			// fail the whole ingest.
			fmt.Fprintf(os.Stderr, "  WARNING: embedding failed, semantic search won't find this one: %v\n", err)
			continue
		}
		if err := st.SaveEmbedding(res.ID, vec); err != nil {
			fmt.Fprintf(os.Stderr, "  WARNING: saving embedding failed: %v\n", err)
			continue
		}
		fmt.Printf("  -> embedded (%d dims)\n", len(vec))
	}
	return 0
}

func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	limit := fs.Int("limit", 10, "max results")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go search [-db path] [-limit N] <query>")
		return 2
	}
	query := fs.Arg(0)

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	results, err := st.Search(query, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED search: %v\n", err)
		return 1
	}
	if len(results) == 0 {
		fmt.Printf("no matches for %q\n", query)
		return 0
	}
	for _, r := range results {
		fmt.Printf("[%d] %-10s %-8s %s\n", r.ID, r.Project, r.ToolName, r.Observation.Title)
		if r.Observation.Subtitle != "" {
			fmt.Printf("     %s\n", r.Observation.Subtitle)
		}
	}
	return 0
}

func cmdSemanticSearch(args []string) int {
	fs := flag.NewFlagSet("semantic-search", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(must match the model used when ingesting, or scores will be meaningless)")
	limit := fs.Int("limit", 10, "max results")
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go semantic-search [-db path] [-limit N] <query>")
		return 2
	}
	query := fs.Arg(0)

	queryVec, err := embed.NewClient(*embedModel).Embed(query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to embed query: %v\n", err)
		return 1
	}

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	results, err := st.SemanticSearch(queryVec, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED semantic search: %v\n", err)
		return 1
	}
	if len(results) == 0 {
		fmt.Printf("no embedded observations to search (ingest with -embed-model set first)\n")
		return 0
	}
	for _, r := range results {
		fmt.Printf("[%d] score=%.4f %-10s %-8s %s\n", r.ID, r.Score, r.Project, r.ToolName, r.Observation.Title)
		if r.Observation.Subtitle != "" {
			fmt.Printf("     %s\n", r.Observation.Subtitle)
		}
	}
	return 0
}

// cmdMCP runs the MCP stdio server. Diagnostics go to a log file, never
// stdout — stdout is the JSON-RPC protocol channel, and a single stray log
// line there would corrupt the stream for whatever real client is reading it.
func cmdMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for "+
		"semantic_search_observations (empty disables that tool)")
	fs.Parse(args)

	l := openLog("mcp.log")
	srv := &mcpserver.Server{DBPath: *dbPath, EmbedModel: *embedModel, Log: l}
	if err := srv.Run(os.Stdin, os.Stdout); err != nil {
		l.Printf("server exited: %v", err)
		return 1
	}
	return 0
}

// sessionStartOutput is the JSON shape Claude Code expects back from a
// SessionStart hook on stdout — confirmed against a real session earlier in
// this project's development (a SessionStart hook injecting a skill this
// exact way was observed directly), not guessed from docs.
type sessionStartOutput struct {
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

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

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		l.Printf("FAILED opening store at %s: %v", *dbPath, err)
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
	out := sessionStartOutput{HookSpecificOutput: &hookSpecificOutput{
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

// cmdStop is the Stop hook: real claude-mem's "summarize" step,
// reimplemented from what's already persisted per tool call rather than
// re-reading the raw transcript. Fire-and-forget like PostToolUse (real
// hooks.json marks Stop "async": true too) — nothing reads this process's
// stdout, so diagnostics go to a log file and that's the only output.
func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	limit := fs.Int("limit", 50, "max observations from this session to include in the summary")
	fs.Parse(args)

	l := openLog("stop.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Printf("FAILED parsing hook payload: %v", err)
		return 0
	}
	if in.SessionID == "" {
		l.Printf("no session_id in Stop payload, skipping")
		return 0
	}

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		l.Printf("FAILED opening store at %s: %v", *dbPath, err)
		return 0
	}
	defer st.Close()

	observations, err := st.BySessionID(in.SessionID, *limit)
	if err != nil {
		l.Printf("FAILED BySessionID(%s): %v", in.SessionID, err)
		return 0
	}
	if len(observations) == 0 {
		l.Printf("no observations for session=%s, nothing to summarize", in.SessionID)
		return 0
	}

	// Idempotency key is the session_id alone, not what's being summarized —
	// exactly one summary per session regardless of how many times Stop
	// fires or how the observation count changes between firings.
	hash := store.ContentHash(in.SessionID, "SessionSummary", "session-summary", "")

	project := filepath.Base(in.Cwd)
	if project == "" || project == "." {
		project = filepath.Base(filepath.Dir(in.TranscriptPath))
	}

	obs, err := observer.New(context.Background(), *model)
	if err != nil {
		l.Printf("FAILED to start observer: %v", err)
		return 0
	}
	defer obs.Close()

	summaryTurn, err := obs.Summarize(observations)
	if err != nil {
		l.Printf("FAILED summarizing session %s: %v", in.SessionID, err)
		return 0
	}

	res, err := st.Insert(in.SessionID, project, "SessionSummary", hash, summaryTurn.Observation, summaryTurn.Result.CostUSD)
	if err != nil {
		l.Printf("FAILED sqlite insert: %v", err)
		return 0
	}
	if !res.Inserted {
		l.Printf("session %s already summarized (observations.id=%d)", in.SessionID, res.ID)
		return 0
	}
	l.Printf("persisted session summary observations.id=%d title=%q from %d observations, cost=$%.4f",
		res.ID, summaryTurn.Observation.Title, len(observations), summaryTurn.Result.CostUSD)
	return 0
}

// cmdDoctor is an operational health check — the kind of thing a real
// deployment needs and a personal dev setup can get away without: is the
// model backend reachable, is the database reachable, is the worker up, is
// semantic search actually usable. Distinguishes critical failures (claude
// CLI missing, database unreachable — nothing works without these) from
// informational ones (worker not running — `start` launches it lazily;
// Ollama unreachable — keyword search still works, just not semantic).
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker listens on")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model semantic search would use")
	fs.Parse(args)

	critical := true

	fmt.Println("claude-mem-go doctor")
	fmt.Println()

	if path, err := claudeagent.FindClaudeExecutable(); err != nil {
		fmt.Printf("✘ claude CLI: %v\n", err)
		critical = false
	} else {
		fmt.Printf("✔ claude CLI found at %s\n", path)
	}

	if worker.IsRunning(*socketPath) {
		fmt.Printf("✔ worker daemon reachable at %s\n", *socketPath)
	} else {
		fmt.Printf("… worker daemon not running at %s (not necessarily a problem — `start` launches it lazily from SessionStart)\n", *socketPath)
	}

	if st, err := backend.Open(context.Background(), *dbPath, 0); err != nil {
		fmt.Printf("✘ database (%s): %v\n", *dbPath, err)
		critical = false
	} else {
		if _, cerr := st.CountByProject(""); cerr != nil {
			fmt.Printf("✘ database (%s) opened but a query failed: %v\n", *dbPath, cerr)
			critical = false
		} else {
			fmt.Printf("✔ database reachable (%s)\n", *dbPath)
		}
		st.Close()
	}

	if err := embed.NewClient(*embedModel).Ping(); err != nil {
		fmt.Printf("… semantic search unavailable: %v (keyword search still works)\n", err)
	} else {
		fmt.Printf("✔ Ollama reachable, model %q pulled — semantic search available\n", *embedModel)
	}

	fmt.Println()
	if !critical {
		fmt.Println("Critical checks failed — claude-mem-go will not function until these are fixed.")
		return 1
	}
	fmt.Println("All critical checks passed.")
	return 0
}
