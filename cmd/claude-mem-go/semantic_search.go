package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/store"
)

func cmdSemanticSearch(args []string) int {
	fs := flag.NewFlagSet("semantic-search", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(must match the model used when ingesting, or scores will be meaningless)")
	limit := fs.Int("limit", 10, "max results")
	project := fs.String("project", "", "scope to one project (default: every project in the store)")
	hnswEfSearch := fs.Int("hnsw-ef-search", 0, "Postgres backend only: override pgvector's hnsw.ef_search "+
		"query-time recall/speed tradeoff, valid range 1-1000 (default 0 leaves pgvector's own default of 40 in place)")
	fs.Parse(args)
	*limit = clampLimit(*limit, 10, 100)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go semantic-search [-db path] [-project name] [-limit N] <query>")
		return 2
	}
	query := fs.Arg(0)

	queryVec, err := embed.NewClient(*embedModel).Embed(query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to embed query: %v\n", err)
		return 1
	}

	st, err := backend.Open(context.Background(), *dbPath, 0, *hnswEfSearch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	results, err := st.SemanticSearch(*project, queryVec, *limit)
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
