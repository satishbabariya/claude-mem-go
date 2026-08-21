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
	obsType := fs.String("type", "", "filter by observation type: discovery, change, decision, summary, or manual (default: every type)")
	offset := fs.Int("offset", 0, "skip this many leading results, for paging past a prior call's limit")
	fs.Parse(args)
	*limit = clampLimit(*limit, 10, 100)
	if *offset < 0 {
		*offset = 0
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go search [-db path] [-project name] [-type discovery|change|decision|summary|manual] [-limit N] [-offset N] <query>")
		return 2
	}
	query := fs.Arg(0)

	st, err := backend.Open(context.Background(), *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	results, err := st.Search(*project, query, *obsType, *limit, *offset)
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
