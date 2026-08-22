package main

import (
	"flag"
	"os"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/mcpserver"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// cmdMCP runs the MCP stdio server. Diagnostics go to a log file, never
// stdout — stdout is the JSON-RPC protocol channel, and a single stray log
// line there would corrupt the stream for whatever real client is reading it.
func cmdMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := cli.DBFlag(fs)
	embedModel := fs.String("embed-model", cli.DefaultEmbedModel, "Ollama model for "+
		"semantic_search_observations (empty disables that tool)")
	hnswEfSearch := fs.Int("hnsw-ef-search", 0, "Postgres backend only: override pgvector's hnsw.ef_search "+
		"query-time recall/speed tradeoff for semantic_search_observations/observation_context, "+
		"valid range 1-1000 (default 200 — measured 94% recall@10; pgvector's own 40 measured 71-80%)")
	fs.Parse(args)

	l := openLog("mcp.log")
	project := ""
	if cwd, err := os.Getwd(); err == nil {
		project = memory.ProjectFor(cwd)
		if project == "." {
			project = ""
		}
	}
	srv := &mcpserver.Server{DBPath: *dbPath, EmbedModel: *embedModel, HNSWEfSearch: *hnswEfSearch, Project: project, Log: l}
	if err := srv.Run(os.Stdin, os.Stdout); err != nil {
		l.Printf("server exited: %v", err)
		return 1
	}
	return 0
}
