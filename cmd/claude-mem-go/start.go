package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"github.com/satishbabariya/claude-mem-go/internal/memory"

	"github.com/satishbabariya/claude-mem-go/internal/worker"
)

// cmdStart is the idempotent SessionStart entry point: mirrors
// worker-service.cjs's "start" plus worker-spawn-gate.ts's mutual exclusion.
// Safe to invoke from every session's SessionStart hook, concurrently.
func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	model := fs.String("model", "haiku", "model alias for observer sessions")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings (empty to skip)")
	dbPath := fs.String("db", memory.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker listens on")
	// Forwarded to the spawned worker, and read here to identify the
	// daemon already running on -socket. Symmetric with the worker's and
	// doctor's own -stats flags: all three must be able to name the same
	// file, or they end up describing different daemons.
	statsPath := fs.String("stats", worker.DefaultStatsPath(), "worker stats file (forwarded to the spawned worker, and read to detect a stale one)")
	maxConcurrent := fs.Int("max-concurrent", 2, "max concurrent observer sessions")
	metricsAddr := fs.String("metrics-addr", "", "if set, the spawned worker serves Prometheus metrics at http://<addr>/metrics")
	excludedProjects := fs.String("excluded-projects", "", "comma-separated glob patterns forwarded to the spawned worker's "+
		"-excluded-projects (see `worker`'s own flag help); empty (the default) excludes nothing")
	fs.Parse(args)

	l := openLog("start.log")

	if worker.IsRunning(*socketPath) {
		// "Running" was the only question asked here, never "which one".
		// The daemon is the single long-lived process in this system, so
		// after an upgrade it keeps applying OLD rules indefinitely while
		// every short-lived hook around it runs the new binary.
		//
		// Found by running the whole loop end to end: a daemon up for ~28
		// hours across sixteen commits was still using the pre-git-root
		// project naming, so a real session in a subdirectory wrote its
		// observations under project "auth" (basename) while the fresh
		// SessionStart hook looked them up under "repo" (git root).
		// Writes and reads silently disagreed, and the naming fix was
		// defeated by a process that simply never restarted.
		//
		// Replacing it is safe at exactly this moment: the daemon drains
		// in-flight work on SIGTERM (see its shutdown path), and
		// SessionStart is the start of a new session, not the middle of
		// one. If anything about the replacement fails, the existing
		// daemon is left alone — a stale daemon still captures, so
		// degrading to "stale but working" beats risking none at all.
		if stale, running, pid := staleDaemon(*statsPath); stale {
			l.Warnf("worker at %s is running an older build (%s); this binary is %s — replacing it",
				*socketPath, running, currentBuildVersion())
			if err := stopDaemon(pid, *socketPath); err != nil {
				l.Warnf("could not stop the stale worker (pid=%d): %v — leaving it running", pid, err)
				return 0
			}
		} else {
			l.Printf("worker already running at %s, nothing to do", *socketPath)
			return 0
		}
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
		l.Errorf("FAILED to resolve our own executable path: %v", err)
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
	if *metricsAddr != "" {
		workerArgs = append(workerArgs, "-metrics-addr", *metricsAddr)
	}
	if *statsPath != "" {
		workerArgs = append(workerArgs, "-stats", *statsPath)
	}
	if *excludedProjects != "" {
		workerArgs = append(workerArgs, "-excluded-projects", *excludedProjects)
	}
	if err := worker.SpawnDetached(self, workerArgs); err != nil {
		l.Errorf("FAILED to spawn worker: %v", err)
		return 1
	}
	l.Printf("spawned a detached worker, waiting for it to become ready")
	waitForReady(*socketPath, l)
	return 0
}
func waitForReady(socketPath string, l *logging.Logger) {
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

// staleDaemon reports whether the daemon described by statsPath is running
// a different build than this binary, along with that build string and its
// pid.
//
// Deliberately conservative: anything unknown means "not stale". A stats
// file that is missing, unreadable, or predates the version field (as every
// daemon started before it necessarily does) reports false, because
// killing a working daemon on a guess is worse than leaving a possibly-old
// one running. The same reasoning the Stop hook's privacy check uses — an
// unknown signal is not evidence.
func staleDaemon(statsPath string) (stale bool, running string, pid int) {
	st, err := worker.ReadStatsFile(statsPath)
	if err != nil || st.Version == "" || st.PID <= 0 {
		return false, "", 0
	}
	if st.Version == currentBuildVersion() {
		return false, st.Version, st.PID
	}
	return true, st.Version, st.PID
}

// stopDaemon asks the daemon to shut down gracefully and waits for its
// socket to go away, so the caller can bind a replacement without racing
// the old process.
//
// SIGTERM rather than SIGKILL: the daemon drains in-flight observations on
// a clean signal, and losing whatever was queued would trade a stale-code
// problem for a lost-memory one.
func stopDaemon(pid int, socketPath string) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	// Bounded: a daemon that will not exit must not hang SessionStart,
	// which is on the user's critical path.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !worker.IsRunning(socketPath) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("still running %s after SIGTERM", 10*time.Second)
}
