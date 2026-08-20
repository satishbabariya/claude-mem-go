package main

import (
	"flag"
	"log"
	"os"
	"strconv"
	"time"

	"claude-mem-go/store"
	"claude-mem-go/worker"
)

// cmdStart is the idempotent SessionStart entry point: mirrors
// worker-service.cjs's "start" plus worker-spawn-gate.ts's mutual exclusion.
// Safe to invoke from every session's SessionStart hook, concurrently.
func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings (empty to skip)")
	dbPath := fs.String("db", store.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker listens on")
	maxConcurrent := fs.Int("max-concurrent", 2, "max concurrent observer sessions")
	fs.Parse(args)

	l := openLog("start.log")

	if worker.IsRunning(*socketPath) {
		l.Printf("worker already running at %s, nothing to do", *socketPath)
		return 0
	}

	lockPath := *socketPath + ".lock"
	if !worker.AcquireSpawnLock(lockPath) {
		l.Printf("another launcher is already starting the worker, waiting for it")
		waitForReady(*socketPath, l)
		return 0
	}
	defer worker.ReleaseSpawnLock(lockPath)

	// The winner of the race above might have finished between our first
	// IsRunning check and acquiring the lock — recheck before spawning a
	// redundant second worker.
	if worker.IsRunning(*socketPath) {
		l.Printf("worker became ready while we were acquiring the spawn lock")
		return 0
	}

	self, err := os.Executable()
	if err != nil {
		l.Printf("FAILED to resolve our own executable path: %v", err)
		return 1
	}
	workerArgs := []string{
		"worker",
		"-model", *model,
		"-embed-model", *embedModel,
		"-db", *dbPath,
		"-socket", *socketPath,
		"-max-concurrent", strconv.Itoa(*maxConcurrent),
	}
	if err := worker.SpawnDetached(self, workerArgs); err != nil {
		l.Printf("FAILED to spawn worker: %v", err)
		return 1
	}
	l.Printf("spawned a detached worker, waiting for it to become ready")
	waitForReady(*socketPath, l)
	return 0
}
func waitForReady(socketPath string, l *log.Logger) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if worker.IsRunning(socketPath) {
			l.Printf("worker is ready at %s", socketPath)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	l.Printf("WARNING: worker did not become ready within 5s")
}
