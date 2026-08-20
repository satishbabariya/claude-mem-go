package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"claude-mem-go/backend"
	"claude-mem-go/store"
)

func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	limit := fs.Int("limit", 10, "max results")
	project := fs.String("project", "", "scope to one project (default: every project in the store)")
	fs.Parse(args)
	*limit = clampLimit(*limit, 10, 100)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go search [-db path] [-project name] [-limit N] <query>")
		return 2
	}
	query := fs.Arg(0)

	st, err := backend.Open(context.Background(), *dbPath, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	results, err := st.Search(*project, query, *limit)
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
