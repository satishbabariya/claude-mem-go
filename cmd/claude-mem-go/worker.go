package main

import (
	"context"
	"flag"
	"os/signal"
	"syscall"

	"claude-mem-go/store"
	"claude-mem-go/worker"
)

func cmdWorker(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings "+
		"(empty to skip embedding — observations are still persisted, just not semantically searchable)")
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket to listen on")
	maxConcurrent := fs.Int("max-concurrent", 2, "max concurrent observer sessions")
	fs.Parse(args)

	d := &worker.Daemon{
		Model:         *model,
		EmbedModel:    *embedModel,
		DBPath:        *dbPath,
		SocketPath:    *socketPath,
		MaxConcurrent: *maxConcurrent,
		Log:           openLog("worker.log"),
		StatsPath:     worker.DefaultStatsPath(),
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
