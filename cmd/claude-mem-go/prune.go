package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
)

// cmdPrune deletes observations older than a cutoff — this store had no
// retention story at all before this: it only ever grew. Dry-run by
// default (just reports a count) since this is the one genuinely
// destructive operation this CLI exposes; -yes is required to actually
// delete anything.
func cmdPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	dbPath := fs.String("db", memory.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	project := fs.String("project", "", "scope to one project (default: every project in the store)")
	olderThanDays := fs.Int("older-than-days", 0, "delete observations older than this many days (required, must be > 0)")
	yes := fs.Bool("yes", false, "actually delete — without this, prune only reports how many rows WOULD be deleted")
	fs.Parse(args)

	if *olderThanDays <= 0 {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go prune -older-than-days N [-project name] [-yes]")
		return 2
	}

	// created_at_epoch is stored in MILLISECONDS (see sqlite/store.go's and postgres/postgres.go's
	// Insert — both stamp now.UnixMilli(), not now.Unix()). A seconds-based
	// cutoff here would be ~1000x smaller than any real row's timestamp,
	// making created_at_epoch < cutoff false for every row that ever
	// existed — prune would silently delete nothing, ever, for any
	// reasonable -older-than-days value. Caught by a real Insert-backed
	// test, not the hand-picked epoch values the earlier unit tests used
	// (which were unit-agnostic and couldn't have caught this).
	cutoff := time.Now().AddDate(0, 0, -*olderThanDays).UnixMilli()

	st, err := backend.Open(context.Background(), *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	n, err := st.Prune(*project, cutoff, !*yes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED prune: %v\n", err)
		return 1
	}

	scope := "every project"
	if *project != "" {
		scope = fmt.Sprintf("project %q", *project)
	}
	if *yes {
		fmt.Printf("Deleted %d observation(s) older than %d days (%s).\n", n, *olderThanDays, scope)
	} else {
		fmt.Printf("%d observation(s) older than %d days (%s) would be deleted. Re-run with -yes to actually delete them.\n", n, *olderThanDays, scope)
	}
	return 0
}
