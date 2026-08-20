package main

import (
	"flag"
	"os"
	"path/filepath"

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
	fs.Parse(args)

	l := openLog("mcp.log")
	project := ""
	if cwd, err := os.Getwd(); err == nil {
		project = filepath.Base(cwd)
		if project == "." {
			project = ""
		}
	}
	srv := &mcpserver.Server{DBPath: *dbPath, EmbedModel: *embedModel, Project: project, Log: l}
	if err := srv.Run(os.Stdin, os.Stdout); err != nil {
		l.Printf("server exited: %v", err)
		return 1
	}
	return 0
}
