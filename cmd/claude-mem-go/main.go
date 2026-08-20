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
//	claude-mem-go prompt-context  — UserPromptSubmit hook: embed the submitted prompt, inject the semantically closest memory
//	claude-mem-go stop            — Stop hook: synthesize and persist a session-level summary observation
//	claude-mem-go doctor          — check that the claude CLI, worker, database, and Ollama are all reachable
//	claude-mem-go file-context    — PreToolUse hook (Read): inject prior memory about the specific file being read
//	claude-mem-go prune           — delete observations older than a cutoff (dry-run by default; retention has no other story)
//	claude-mem-go version         — print the build's commit/time, for correlating a bug report with an exact build
//	claude-mem-go export          — dump every observation as JSON Lines (backup, and the SQLite<->Postgres migration path)
//	claude-mem-go import          — restore/migrate a file written by export; idempotent (matched by content_hash)
package main

import (
	"fmt"
	"log"
	"os"

	"claude-mem-go/store"
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
	case "prompt-context":
		os.Exit(cmdPromptContext(os.Args[2:]))
	case "stop":
		os.Exit(cmdStop(os.Args[2:]))
	case "doctor":
		os.Exit(cmdDoctor(os.Args[2:]))
	case "file-context":
		os.Exit(cmdFileContext(os.Args[2:]))
	case "prune":
		os.Exit(cmdPrune(os.Args[2:]))
	case "version":
		os.Exit(cmdVersion(os.Args[2:]))
	case "export":
		os.Exit(cmdExport(os.Args[2:]))
	case "import":
		os.Exit(cmdImport(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: claude-mem-go <worker|hook|ingest> [flags]")
}

func openLog(name string) *log.Logger {
	w, err := newRotatingWriter(store.DefaultHome()+"/"+name, defaultMaxLogBytes)
	if err != nil {
		return log.New(os.Stderr, "", log.LstdFlags)
	}
	return log.New(w, "", log.LstdFlags)
}
