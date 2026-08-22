package main

import (
	"context"
	"flag"
	"os/signal"
	"syscall"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/worker"
)

func cmdWorker(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(empty to skip embedding — observations are still persisted, just not semantically searchable)")
	dbPath := fs.String("db", memory.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket to listen on")
	// Symmetric with -socket, and needed for the same reason: a daemon
	// can already be run on a non-default socket, but its stats file was
	// hardcoded — so two daemons on different sockets would fight over
	// one stats file, and `doctor -stats` (which can be pointed anywhere)
	// had nothing to point AT.
	statsPath := fs.String("stats", worker.DefaultStatsPath(), "file to write stats snapshots to")
	maxConcurrent := fs.Int("max-concurrent", 2, "max concurrent observer sessions")
	metricsAddr := fs.String("metrics-addr", "", "if set, serve Prometheus metrics at http://<addr>/metrics (e.g. 127.0.0.1:9090); empty disables it")
	excludedProjects := fs.String("excluded-projects", "", "comma-separated glob patterns (supports *, **, ?, and a leading ~) — "+
		"a project whose path or directory name matches one is never observed automatically, the real claude-mem "+
		"CLAUDE_MEM_EXCLUDED_PROJECTS feature; empty (the default) excludes nothing")
	fs.Parse(args)

	d := &worker.Daemon{
		Model:            *model,
		EmbedModel:       *embedModel,
		DBPath:           *dbPath,
		SocketPath:       *socketPath,
		MaxConcurrent:    *maxConcurrent,
		Log:              openLog("worker.log"),
		StatsPath:        *statsPath,
		Version:          currentBuildVersion(),
		MetricsAddr:      *metricsAddr,
		ExcludedProjects: *excludedProjects,
	}

	// Graceful shutdown: SIGTERM/SIGINT cancel the context Run() watches,
	// which closes the listener cleanly (see worker.Daemon.Run) instead of
	// the process just dying mid-accept and leaving the socket file behind
	// for the next launcher to clean up.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := d.Run(ctx); err != nil {
		if ctx.Err() != nil {
			d.Log.Printf("daemon shut down cleanly on signal")
			return 0
		}
		d.Log.Printf("daemon exited: %v", err)
		return 1
	}
	return 0
}
