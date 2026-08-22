// Spawn coordination for auto-launching the worker daemon from a
// SessionStart hook — the Go analog of
// src/shared/worker-spawn-gate.ts + worker-service.cjs's "start" command.
//
// Two things have to be true for this to work, and both were learned the
// hard way earlier in this project, not assumed:
//
//  1. The spawned worker must be fully detached (new session/process group),
//     not just backgrounded — otherwise it inherits the SAME failure mode
//     the PostToolUse hook had: a child tied to its parent's process group
//     dies when that parent (the short-lived SessionStart hook process)
//     exits. SpawnDetached uses Setsid for exactly this reason.
//  2. Multiple SessionStart hooks can fire concurrently (several Claude Code
//     sessions starting at once) and must not race to spawn duplicate
//     workers. AcquireSpawnLock/ReleaseSpawnLock is a narrowed port of
//     worker-spawn-gate.ts's O_CREAT|O_EXCL lockfile: whoever creates the
//     lock file atomically is the one launcher allowed to spawn; a lock
//     older than spawnLockStaleAfter is presumed abandoned (holder crashed
//     mid-spawn) and may be broken once.
package worker

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// spawnLockStaleAfter mirrors worker-spawn-gate.ts's SPAWN_LOCK_STALE_MS: a
// holder that hasn't finished spawning within this window is presumed dead.
const spawnLockStaleAfter = 90 * time.Second

// IsRunning reports whether a worker daemon is already listening on
// socketPath. Used both to make auto-launch idempotent (don't spawn a
// second worker) and, after spawning, to poll for readiness.
func IsRunning(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// AcquireSpawnLock tries to become the one launcher allowed to spawn the
// worker. Returns true when this process holds the lock (caller MUST
// ReleaseSpawnLock in all cases — success or failure). Returns false when
// another launcher holds a fresh lock: the caller should wait for that
// launcher's worker instead of spawning a competitor.
//
// Like the real gate, this fails OPEN on non-contention errors (permission
// issues, read-only filesystem): a broken lock mechanism must degrade to
// "spawn anyway," never suppress every launch forever.
func AcquireSpawnLock(lockPath string) bool {
	if tryCreateLock(lockPath) {
		return true
	}
	// Contention: a lock file already exists. Judge its staleness by mtime,
	// never by clock values stored in the file content.
	info, err := os.Stat(lockPath)
	if err != nil {
		// Vanished between the failed create and the stat — the holder just
		// released. One retry is enough; if this also fails, treat it as
		// live contention rather than looping.
		return tryCreateLock(lockPath)
	}
	if time.Since(info.ModTime()) <= spawnLockStaleAfter {
		return false // fresh lock: another launcher is mid-spawn
	}
	// Stale: the holder died mid-spawn. Break it and retry once. A
	// competing breaker winning this race just means we yield, not error.
	if os.Remove(lockPath) != nil {
		return false
	}
	return tryCreateLock(lockPath)
}

func tryCreateLock(lockPath string) bool {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return !os.IsExist(err) // non-contention error: fail open (true = "you may spawn")
	}
	fmt.Fprintf(f, "%d", os.Getpid())
	f.Close()
	return true
}

// ReleaseSpawnLock releases the lock IF this process owns it — owner-checked
// so a launcher can never delete a competitor's live lock (e.g. after its
// own stale lock was broken and re-acquired by someone else). Safe to call
// even if AcquireSpawnLock's fail-open path never actually created a file.
func ReleaseSpawnLock(lockPath string) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		return
	}
	os.Remove(lockPath)
}

// SpawnDetached launches the worker daemon fully detached from the calling
// process: a new session (Setsid) so it survives the caller — a
// short-lived SessionStart hook — exiting, and stdio redirected to
// /dev/null since nothing will ever read this process's stdout/stderr
// again (it logs to its own file instead, same as claude-mem-go's other
// long-running paths).
func SpawnDetached(binPath string, args []string) error {
	cmd := exec.Command(binPath, args...)
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The child holds its own dup of the descriptor once Start returns
	// (whether or not it succeeded); the parent's copy was never closed
	// and leaked an FD per spawn attempt.
	err = cmd.Start()
	devnull.Close()
	return err
}

// maxSocketPathLen is the longest unix socket path the kernel accepts:
// sockaddr_un.sun_path is 104 bytes on macOS/BSD and 108 on Linux,
// including the trailing NUL. Exceeding it fails bind(2) with EINVAL —
// "invalid argument" — which says nothing about the path. Found for real
// when a daemon was pointed at a deeply nested temp directory: it died
// within milliseconds, logged one INFO line, and `start` reported only
// "did not become ready within 5s".
func maxSocketPathLen() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// ValidateSocketPath rejects a socket path the kernel cannot bind, with a
// message that names the limit, so the failure surfaces where the path
// was chosen (start, worker) instead of as a bare EINVAL in worker.log.
func ValidateSocketPath(path string) error {
	if path == "" {
		return fmt.Errorf("socket path is empty")
	}
	if n, max := len(path), maxSocketPathLen(); n > max {
		return fmt.Errorf("socket path is %d bytes, longer than the %d the OS allows for a unix socket; use -socket to pick a shorter path: %s", n, max, path)
	}
	return nil
}
