package main

import (
	"context"
	"flag"
	"fmt"
	"runtime/debug"
	"sort"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/store"
	"claude-mem-go/worker"
)

// cmdDoctor is an operational health check — the kind of thing a real
// deployment needs and a personal dev setup can get away without: is the
// model backend reachable, is the database reachable, is the worker up, is
// semantic search actually usable. Distinguishes critical failures (claude
// CLI missing, database unreachable — nothing works without these) from
// informational ones (worker not running — `start` launches it lazily;
// Ollama unreachable — keyword search still works, just not semantic).
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker listens on")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model semantic search would use")
	fs.Parse(args)

	critical := true

	buildInfo, _ := debug.ReadBuildInfo()
	fmt.Println("doctor —", buildVersionString(buildInfo))
	fmt.Println()

	if path, err := claudeagent.FindClaudeExecutable(); err != nil {
		fmt.Printf("✘ claude CLI: %v\n", err)
		critical = false
	} else {
		fmt.Printf("✔ claude CLI found at %s\n", path)
	}

	if worker.IsRunning(*socketPath) {
		fmt.Printf("✔ worker daemon reachable at %s\n", *socketPath)
	} else {
		fmt.Printf("… worker daemon not running at %s (not necessarily a problem — `start` launches it lazily from SessionStart)\n", *socketPath)
	}

	// Informational only, never critical: a missing stats file just means
	// the worker hasn't processed anything yet (or predates this feature),
	// not that anything is broken.
	if stats, err := worker.ReadStatsFile(worker.DefaultStatsPath()); err == nil {
		fmt.Printf("… worker activity: processed=%d duplicates=%d observer_errors=%d insert_errors=%d embed_errors=%d pool=%d/%d cached_sessions=%d",
			stats.Processed, stats.Duplicates, stats.ObserverErrors, stats.InsertErrors, stats.EmbedErrors,
			stats.PoolInFlight, stats.PoolCapacity, stats.CachedSessions)
		if stats.LastActivityAt != "" {
			fmt.Printf(" last_activity=%s", stats.LastActivityAt)
		}
		fmt.Println()
	}

	redactedDBPath := store.RedactDSN(*dbPath)
	if st, err := backend.Open(context.Background(), *dbPath, 0); err != nil {
		fmt.Printf("✘ database (%s): %v\n", redactedDBPath, err)
		critical = false
	} else {
		if _, cerr := st.CountByProject(""); cerr != nil {
			fmt.Printf("✘ database (%s) opened but a query failed: %v\n", redactedDBPath, cerr)
			critical = false
		} else {
			fmt.Printf("✔ database reachable (%s)\n", redactedDBPath)
		}
		// Informational only, never critical on its own — a detail like
		// hnsw_index_exists=false is a real problem worth surfacing, but
		// it's a degraded-performance signal, not "nothing works."
		if details, herr := st.HealthDetails(); herr == nil {
			keys := make([]string, 0, len(details))
			for k := range details {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Print("  ")
			for i, k := range keys {
				if i > 0 {
					fmt.Print(" ")
				}
				fmt.Printf("%s=%s", k, details[k])
			}
			fmt.Println()
		}
		st.Close()
	}

	if err := embed.NewClient(*embedModel).Ping(); err != nil {
		fmt.Printf("… semantic search unavailable: %v (keyword search still works)\n", err)
	} else {
		fmt.Printf("✔ Ollama reachable, model %q pulled — semantic search available\n", *embedModel)
	}

	fmt.Println()
	if !critical {
		fmt.Println("Critical checks failed — claude-mem-go will not function until these are fixed.")
		return 1
	}
	fmt.Println("All critical checks passed.")
	return 0
}
