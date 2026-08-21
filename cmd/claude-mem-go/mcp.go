package main

import (
	"flag"
	"os"

	"claude-mem-go/mcpserver"
	"claude-mem-go/store"
)

// cmdMCP runs the MCP stdio server. Diagnostics go to a log file, never
// stdout — stdout is the JSON-RPC protocol channel, and a single stray log
// line there would corrupt the stream for whatever real client is reading it.
func cmdMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for "+
		"semantic_search_observations (empty disables that tool)")
	hnswEfSearch := fs.Int("hnsw-ef-search", 0, "Postgres backend only: override pgvector's hnsw.ef_search "+
		"query-time recall/speed tradeoff for semantic_search_observations/observation_context, "+
		"valid range 1-1000 (default 0 leaves pgvector's own default of 40 in place)")
	fs.Parse(args)

	l := openLog("mcp.log")
	project := ""
	if cwd, err := os.Getwd(); err == nil {
		project = store.ProjectFor(cwd)
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
