package worker

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
)

func WaitForReady(socketPath string, l *logging.Logger) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if IsRunning(socketPath) {
			l.Printf("worker is ready at %s", socketPath)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	l.Warnf("worker did not become ready within 5s — if it exited, the reason is the last line of worker.log")
}

// stopDaemon asks the daemon to shut down gracefully and waits for its
// socket to go away, so the caller can bind a replacement without racing
// the old process.
//
// SIGTERM rather than SIGKILL: the daemon drains in-flight observations on
// a clean signal, and losing whatever was queued would trade a stale-code
// problem for a lost-memory one.
func StopDaemon(pid int, socketPath string) error {
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
		if !IsRunning(socketPath) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("still running %s after SIGTERM", 10*time.Second)
}
