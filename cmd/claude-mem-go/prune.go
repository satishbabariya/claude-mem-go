package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
)

// cmdPrune deletes observations older than a cutoff — this store had no
// retention story at all before this: it only ever grew. Dry-run by
// default (just reports a count) since this is the one genuinely
// destructive operation this CLI exposes; -yes is required to actually
// delete anything.
func cmdPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	dbPath := cli.DBFlag(fs)
	project := fs.String("project", "", "scope to one project (default: every project in the store)")
	olderThanDays := fs.Int("older-than-days", 0, "delete observations older than this many days (required, must be > 0)")
	yes := fs.Bool("yes", false, "actually delete — without this, prune only reports how many rows WOULD be deleted")
	relPaths := fs.Bool("relative-paths", false, "instead of deleting observations, strip RELATIVE entries from files_read/"+
		"files_modified (rows from before paths were canonicalized; file-context can never match them). "+
		"Observations are kept. Dry-run without -yes.")
	fs.Parse(args)

	if *relPaths == (*olderThanDays > 0) {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go prune (-older-than-days N | -relative-paths) [-project name] [-yes]")
		return 2
	}
	if *relPaths {
		return pruneRelativePaths(*dbPath, *project, *yes)
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

	ctx, cancel := cliContext()
	defer cancel()
	st, err := backend.Open(ctx, *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	n, err := st.Prune(ctx, *project, cutoff, !*yes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED prune: %v\n", err)
		return 1
	}

	scope := "every project"
	if *project != "" {
		scope = fmt.Sprintf("project %q", *project)
	}
	// Prune also deletes stored user prompts older than the same cutoff
	// (see memory.Backend.Prune); the count it returns is observations
	// only, so both messages say so rather than implying the number
	// covers everything that went.
	if *yes {
		fmt.Printf("Deleted %d observation(s) older than %d days (%s), plus any stored user prompts older than that.\n", n, *olderThanDays, scope)
	} else {
		fmt.Printf("%d observation(s) older than %d days (%s) would be deleted, plus any stored user prompts older than that. Re-run with -yes to actually delete them.\n", n, *olderThanDays, scope)
	}
	return 0
}

// pruneRelativePaths is `prune -relative-paths`: the remediation for
// observations written before memory.NormalizeFilePath existed, whose
// relative file paths the PreToolUse file-context lookup can never match.
// See memory.Backend.RepairFilePaths for why the entries are dropped rather
// than guessed into absolute paths.
func pruneRelativePaths(dbPath, project string, yes bool) int {
	ctx, cancel := cliContext()
	defer cancel()
	st, err := backend.Open(ctx, dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	n, err := st.RepairFilePaths(ctx, project, !yes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED repairing file paths: %v\n", err)
		return 1
	}
	scope := "every project"
	if project != "" {
		scope = fmt.Sprintf("project %q", project)
	}
	if yes {
		fmt.Printf("Stripped relative file paths from %d observation(s) (%s); the observations themselves were kept.\n", n, scope)
	} else {
		fmt.Printf("%d observation(s) (%s) carry relative file paths that file-context cannot match. "+
			"Re-run with -yes to strip those entries (the observations are kept).\n", n, scope)
	}
	return 0
}
