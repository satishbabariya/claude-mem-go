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

// privacySetPrefix opens the fire-and-forget protocol SetSessionPrivate
// speaks and ParsePrivacyMarker recognizes, the same way
// inFlightQueryPrefix does for the (request/response) in-flight query —
// never a JSON payload's actual first byte, so the two can't collide.
const privacySetPrefix = "PRIVATE "

// privacyQueryPrefix opens QueryPrivate's own (request/response) query,
// distinct from privacySetPrefix so the worker's handleConn can tell
// "record this session's new privacy state" apart from "tell me its
// current one" on the same socket.
const privacyQueryPrefix = "ISPRIVATE "

// ParsePrivacyMarker reports whether raw is a privacy-state marker (as
// sent by SetSessionPrivate) rather than a hook-forwarding payload or an
// in-flight query, and if so, the session_id and the private flag it
// carries. Called by the worker daemon's handleConn.
func ParsePrivacyMarker(raw []byte) (sessionID string, private bool, ok bool) {
	s := string(raw)
	if !strings.HasPrefix(s, privacySetPrefix) {
		return "", false, false
	}
	fields := strings.Fields(strings.TrimPrefix(s, privacySetPrefix))
	if len(fields) != 2 {
		return "", false, false
	}
	return fields[0], fields[1] == "1", true
}

// SetSessionPrivate tells the worker daemon at socketPath whether
// sessionID's most recently submitted prompt was entirely private (empty
// after privacy.StripMemoryTags) — the UserPromptSubmit hook
// (prompt-context) calls this on every single prompt, private or not, so
// the daemon's flag never goes stale once a later, non-private prompt
// supersedes an earlier private one.
//
// Fire-and-forget like Forward: this call returning success means only
// "the daemon's process now has the marker," and a caller should treat an
// unreachable daemon the same way Forward's own callers do — log it, but
// never fail or block the hook that triggered this.
func SetSessionPrivate(socketPath, sessionID string, private bool) error {
	flag := "0"
	if private {
		flag = "1"
	}
	conn, err := net.DialTimeout("unix", socketPath, DialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(privacySetPrefix + sessionID + " " + flag))
	return err
}

// ParsePrivacyQuery reports whether raw is a privacy query (as sent by
// QueryPrivate) rather than a hook payload or one of this protocol's other
// message kinds, and if so, the session_id it's asking about.
func ParsePrivacyQuery(raw []byte) (sessionID string, ok bool) {
	s := string(raw)
	if !strings.HasPrefix(s, privacyQueryPrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(s, privacyQueryPrefix)), true
}

// QueryPrivate asks the worker daemon at socketPath whether sessionID's
// current turn was marked private, the Stop hook's (cmdStop) counterpart
// to SetSessionPrivate — Stop runs as its own separate process, so it has
// no direct access to the worker's in-memory flag and must ask over the
// socket, the same way it already asks QueryInFlight for in-flight state.
//
// Returns an error if the daemon isn't reachable — a caller should treat
// that as "unknown," not "not private," and fall back to summarizing
// rather than silently dropping a real session's summary because the
// daemon happened to be unreachable.
func QueryPrivate(socketPath, sessionID string) (bool, error) {
	conn, err := net.DialTimeout("unix", socketPath, DialTimeout)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(DialTimeout))

	if _, err := conn.Write([]byte(privacyQueryPrefix + sessionID)); err != nil {
		return false, err
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	raw, err := io.ReadAll(io.LimitReader(conn, 8))
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(raw)) {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("malformed privacy-query response %q", raw)
	}
}

// dedupePrefix opens CheckDuplicatePrompt's own request/response
// protocol, distinct from every other prefix this package defines so the
// worker's handleConn can tell them all apart on the same socket.
const dedupePrefix = "DEDUPE "

// ParseDedupeQuery reports whether raw is a duplicate-prompt check (as
// sent by CheckDuplicatePrompt) rather than a hook payload or one of this
// protocol's other message kinds, and if so, the session_id and prompt
// hash it carries. Called by the worker daemon's handleConn.
func ParseDedupeQuery(raw []byte) (sessionID, promptHash string, ok bool) {
	s := string(raw)
	if !strings.HasPrefix(s, dedupePrefix) {
		return "", "", false
	}
	fields := strings.Fields(strings.TrimPrefix(s, dedupePrefix))
	if len(fields) != 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

// CheckDuplicatePrompt asks the worker daemon at socketPath whether
// promptHash (a hex-encoded hash of a session's tag-stripped prompt text,
// computed by the caller) was already seen for sessionID within the
// dedup window, real claude-mem's own USER_PROMPT_DEDUPE_WINDOW_MS
// (10s) — the fix for a real, previously-shipped bug there (real
// claude-mem's own issue #2515): Claude Code can fire UserPromptSubmit
// more than once for the same prompt, and without this check each firing
// pays its own embedding call and injects its own duplicate context
// block in the same turn.
//
// This is a check-AND-record call, atomic on the worker's side: if the
// hash is genuinely new (or the previous one aged out of the window),
// the worker records it as the new "most recent prompt" for this session
// and reports false; a session's window only ever resets on a
// genuinely-new prompt, never merely by asking again — matching real
// claude-mem's own semantics (a duplicate is checked against the
// original saved prompt's timestamp, not extended by repeated duplicate
// hits).
//
// Returns an error if the daemon isn't reachable — a caller should treat
// that as "unknown," not "duplicate," and proceed normally rather than
// silently dropping a real prompt's context injection because the daemon
// happened to be unreachable.
func CheckDuplicatePrompt(socketPath, sessionID, promptHash string) (bool, error) {
	conn, err := net.DialTimeout("unix", socketPath, DialTimeout)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(DialTimeout))

	if _, err := conn.Write([]byte(dedupePrefix + sessionID + " " + promptHash)); err != nil {
		return false, err
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	raw, err := io.ReadAll(io.LimitReader(conn, 8))
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(raw)) {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("malformed dedupe-query response %q", raw)
	}
}

// RecallSource names which read path a recall report came from. It is a
// closed set on purpose: the counters behind it are fixed fields rather
// than a map, and an unrecognized source is rejected at parse time rather
// than silently folded into another bucket, where it would corrupt the
// one signal these counters exist to provide.
type RecallSource string

const (
	// RecallPrompt is UserPromptSubmit's semantic recall against the
	// prompt just submitted. An empty result here is genuinely suspicious
	// on a project that has any content at all.
	RecallPrompt RecallSource = "prompt"
	// RecallSession is SessionStart's context injection. Empty means the
	// session began with no memory whatsoever, which is the single most
	// consequential silent failure this system has.
	RecallSession RecallSource = "session"
	// RecallFile is the PreToolUse file-context lookup. Empty is ORDINARY
	// here — most files have never been touched before — which is exactly
	// why it is counted separately instead of being averaged in with the
	// two above and destroying their signal.
	RecallFile RecallSource = "file"
)

func validRecallSource(s RecallSource) bool {
	return s == RecallPrompt || s == RecallSession || s == RecallFile
}

// recallPrefix opens ReportRecall's one-way protocol, distinct from every
// other prefix this package defines so handleConn can tell them apart on
// the shared socket.
const recallPrefix = "RECALL "

// ParseRecallReport reports whether raw is a recall outcome report (as
// sent by ReportRecall) rather than a hook payload or another message
// kind, and if so how many results the search returned. Called by the
// worker daemon's handleConn.
func ParseRecallReport(raw []byte) (source RecallSource, results int, ok bool) {
	s := string(raw)
	if !strings.HasPrefix(s, recallPrefix) {
		return "", 0, false
	}
	fields := strings.Fields(strings.TrimPrefix(s, recallPrefix))
	if len(fields) != 2 {
		return "", 0, false
	}
	src := RecallSource(fields[0])
	if !validRecallSource(src) {
		return "", 0, false
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n < 0 {
		return "", 0, false
	}
	return src, n, true
}

// ReportRecall tells the worker daemon that a semantic recall ran and how
// many observations it returned, so the daemon can aggregate what is
// otherwise unobservable.
//
// Every metric this project exposes describes the WRITE path — observations
// persisted, duplicates, insert and embed errors. Nothing described the
// READ path, and that is the half with the worse failure mode: a semantic
// search that returns nothing raises no error and writes no log anyone
// reads. It simply injects less memory than the store holds. This project
// has already shipped that exact failure twice — a cross-project search
// leak, and a project post-filter that returned zero rows for a project
// holding 60,000 observations — and in both cases every counter stayed
// green while recall was silently broken.
//
// Fire-and-forget, and deliberately so: recall is what the user is
// waiting on, and instrumentation that can delay or fail an injection is
// worse than no instrumentation.
//
// The cost was measured rather than assumed, because RecallFile runs on
// EVERY Read tool call — the hottest hook path in the system. Against a
// warmed store, 25 calls per variant: 20.0ms per call with no daemon
// (connect fails instantly with ENOENT) and 22.4ms with one running, so
// the report itself costs about 2.4ms against a ~20ms baseline that is
// dominated by process startup and opening the store. Re-measuring the
// no-daemon case afterwards reproduced 20.4ms, confirming the difference
// is the telemetry and not ordering. An earlier version of this
// measurement showed the opposite — no daemon appearing SLOWER — which
// was an artifact of the first variant paying for creating and migrating
// a fresh SQLite file. Every error here is swallowed by the
// caller — a daemon that is not running simply means no metric, which is
// the correct tradeoff for a telemetry write on a latency path.
func ReportRecall(socketPath string, source RecallSource, results int) error {
	if !validRecallSource(source) {
		return fmt.Errorf("unknown recall source %q", source)
	}
	conn, err := net.DialTimeout("unix", socketPath, DialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(DialTimeout))
	_, err = conn.Write([]byte(fmt.Sprintf("%s%s %d", recallPrefix, source, results)))
	return err
}
