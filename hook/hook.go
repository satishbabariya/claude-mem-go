// Package hook is the thin PostToolUse hook client: forward stdin to the
// worker daemon's socket and return immediately. This is the fix side of
// the async-hook-child-doesn't-survive-its-parent finding documented in the
// worker package — this client does no observation work itself, so there is
// nothing for Claude Code to kill mid-flight when it tears down the
// process that invoked this hook.
package hook

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// DialTimeout bounds how long Forward waits to reach the worker daemon.
// Short on purpose: a hook process is meant to return almost instantly:
const DialTimeout = 500 * time.Millisecond

// MaxPayloadBytes bounds a single hook payload — this client's stdin read
// (Forward) and the worker daemon's own socket read (handleConn) both
// enforce it, so an abnormally large tool_response (a Bash command that
// cats a multi-gigabyte file, a Read of a huge log) can't balloon memory
// in the one long-lived daemon process every project on the machine
// shares, and can't get stringified verbatim into an observer prompt at a
// real per-token API cost. Matches mcpserver's own JSON-RPC line cap
// (8MB) — no real observation this project has ever produced needs
// anywhere near this much, so the cap only ever fires on the pathological
// case it exists for.
const MaxPayloadBytes = 8 * 1024 * 1024

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
// or the payload exceeds MaxPayloadBytes, Forward returns an error the
// caller should log, not surface as a Claude Code-visible hook failure —
// a missing daemon, or one abnormally large tool call, must never block or
// fail the tool call that triggered this hook. An oversized payload is
// rejected outright rather than truncated: a truncated JSON hook payload
// is corrupt, not just short, so there is no safe partial-forward here.
func Forward(socketPath string, r io.Reader) (bytesSent int, err error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxPayloadBytes+1))
	if err != nil {
		return 0, err
	}
	if len(raw) > MaxPayloadBytes {
		return 0, fmt.Errorf("hook payload exceeds %d bytes, not forwarding (likely an abnormally large tool_response)", MaxPayloadBytes)
	}

	conn, err := net.DialTimeout("unix", socketPath, DialTimeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	n, err := conn.Write(raw)
	return n, err
}

// inFlightQueryPrefix opens the small plain-text query protocol the worker
// daemon's handleConn recognizes ahead of the real (always-JSON,
// always-"{"-prefixed) hook-forwarding payload, so the two can never be
// confused on the wire. Unexported: ParseInFlightQuery and QueryInFlight
// are the only sanctioned way to speak or parse this protocol, so the
// client and server side can never drift out of sync on the exact format.
const inFlightQueryPrefix = "INFLIGHT "

// ParseInFlightQuery reports whether raw is an in-flight query (as sent by
// QueryInFlight) rather than a hook-forwarding payload, and if so, the
// session_id it's asking about. Called by the worker daemon's handleConn.
func ParseInFlightQuery(raw []byte) (sessionID string, ok bool) {
	s := string(raw)
	if !strings.HasPrefix(s, inFlightQueryPrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(s, inFlightQueryPrefix)), true
}

// QueryInFlight asks the worker daemon at socketPath how many PostToolUse
// events for sessionID it currently has between "received" and "fully
// processed," and returns that count.
//
// This is the direct fix for the gap the Stop hook's original polling
// heuristic (waitForSessionObservations) documented as its own known
// limitation: rather than inferring "has the worker caught up" by
// watching the observations table's row count stabilize — which a
// sequential-processing plateau can fool, and which has no way to tell
// "genuinely done" apart from "still working" without guessing at a
// timeout — a caller can now ask the one process that actually knows,
// directly.
//
// Returns an error if the daemon isn't reachable (not started, wrong
// socket path, crashed) or the connection fails for any other reason — a
// caller should treat that as "unknown," not "zero," and fall back to a
// heuristic rather than assuming nothing is in flight.
func QueryInFlight(socketPath, sessionID string) (int, error) {
	conn, err := net.DialTimeout("unix", socketPath, DialTimeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(DialTimeout))

	if _, err := conn.Write([]byte(inFlightQueryPrefix + sessionID)); err != nil {
		return 0, err
	}
	// The server's own read (handleConn) blocks until EOF, same as it does
	// for a real hook-forwarding payload — it has no length prefix to know
	// otherwise. CloseWrite half-closes just this side, signaling "nothing
	// more coming" without tearing down the connection, so the read below
	// can still get the server's reply. Without this, both ends would
	// block forever: the server waiting to see EOF before it even looks at
	// what was sent, this call waiting to read a response the server never
	// gets around to writing.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	raw, err := io.ReadAll(io.LimitReader(conn, 32))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("malformed in-flight response %q: %w", raw, err)
	}
	return n, nil
}
