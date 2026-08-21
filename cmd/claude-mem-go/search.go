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
	obsType := fs.String("type", "", "filter by observation type: discovery, change, decision, summary, or manual. Comma-separated for multiple (default: every type)")
	offset := fs.Int("offset", 0, "skip this many leading results, for paging past a prior call's limit")
	dateStart := fs.String("date-start", "", "only observations created on or after this date (RFC3339 or YYYY-MM-DD)")
	dateEnd := fs.String("date-end", "", "only observations created on or before this date (RFC3339 or YYYY-MM-DD)")
	orderBy := fs.String("order-by", "", "sort order: date_desc or date_asc (default: relevance)")
	fs.Parse(args)
	*limit = clampLimit(*limit, 10, 100)
	if *offset < 0 {
		*offset = 0
	}

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go search [-db path] [-project name] [-type discovery|change|decision|summary|manual] "+
			"[-limit N] [-offset N] [-date-start date] [-date-end date] [-order-by relevance|date_desc|date_asc] <query>")
		return 2
	}
	query := fs.Arg(0)

	dateStartMs, err := store.ParseDateArg(*dateStart)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED parsing -date-start: %v\n", err)
		return 2
	}
	dateEndMs, err := store.ParseDateArg(*dateEnd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED parsing -date-end: %v\n", err)
		return 2
	}

	st, err := backend.Open(context.Background(), *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	results, err := st.Search(*project, query, *obsType, *limit, *offset, dateStartMs, dateEndMs, *orderBy)
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
