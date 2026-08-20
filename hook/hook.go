// Package hook is the thin PostToolUse hook client: forward stdin to the
// worker daemon's socket and return immediately. This is the fix side of
// the async-hook-child-doesn't-survive-its-parent finding documented in the
// worker package — this client does no observation work itself, so there is
// nothing for Claude Code to kill mid-flight when it tears down the
// process that invoked this hook.
package hook

import (
	"io"
	"net"
	"time"
)

// DialTimeout bounds how long Forward waits to reach the worker daemon.
// Short on purpose: a hook process is meant to return almost instantly:
const DialTimeout = 500 * time.Millisecond

// Forward reads all of r (typically os.Stdin) and writes it to the worker
// daemon's Unix socket at socketPath, then returns the byte count sent.
//
// It intentionally does not wait for a response — the daemon has nothing to
// ack over this connection; by design, this call returning success only
// means "the daemon's process now has the payload," not "the observation is
// persisted." A caller wanting the latter should be reading the daemon's
// log or the database, not this return value.
//
// If the daemon isn't reachable (not started, wrong socket path, crashed),
// Forward returns an error the caller should log, not surface as a Claude
// Code-visible hook failure — a missing daemon must never block or fail
// the tool call that triggered this hook.
func Forward(socketPath string, r io.Reader) (bytesSent int, err error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}

	conn, err := net.DialTimeout("unix", socketPath, DialTimeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	n, err := conn.Write(raw)
	return n, err
}
