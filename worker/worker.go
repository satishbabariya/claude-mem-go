// Package worker is the persistent daemon a PostToolUse hook notifies
// instead of doing observation work itself — the fix for a real, tested
// failure mode: an "async": true hook's child process does not outlive its
// parent `claude` process. Two real Claude-Code-triggered hook invocations
// were killed mid-observation when their invoking `claude -p` process
// exited a few seconds later; the identical work run inside an
// already-detached daemon (started once, independent of any single tool
// call) completed both times. This package is that daemon — the Go analog
// of worker-service.cjs, started once (e.g. from a SessionStart hook) and
// long-lived, versus hook.go's thin client that only ever forwards and
// exits immediately.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/classify"
	"claude-mem-go/embed"
	"claude-mem-go/observer"
	"claude-mem-go/pool"
	"claude-mem-go/store"
	"claude-mem-go/transcript"
)

// DefaultSocketPath is ~/.claude-mem-go/worker.sock.
func DefaultSocketPath() string { return filepath.Join(store.DefaultHome(), "worker.sock") }

// idleEvictInterval is how often the background sweep checks for sessions
// past sessionIdleTimeout — doesn't need to be frequent, this is just
// resource hygiene for a long-lived daemon, not a correctness deadline.
const idleEvictInterval = time.Minute

// Daemon is the long-lived worker: bind a Unix socket, accept
// PostToolUse-shaped payloads forever, process each through a bounded pool
// of cached, per-session Observers.
type Daemon struct {
	Model         string
	EmbedModel    string // Ollama model for semantic-search embeddings; empty disables
	DBPath        string
	SocketPath    string
	MaxConcurrent int // mirrors CLAUDE_MEM_MAX_CONCURRENT_AGENTS's default of 2
	Log           *log.Logger
	// StatsPath is where Stats snapshots are written after every processed
	// event — see stats.go. Empty disables writing (tests mostly want this;
	// a real daemon always wants it, so cmd's daemon construction sets it to
	// DefaultStatsPath()).
	StatsPath string

	sessions *sessionCache
	counters statsCounters
	st       store.Backend
}

// Stats returns a snapshot of the daemon's current activity and
// concurrency utilization — see stats.go's Stats type. Safe to call
// concurrently with Run/process.
func (d *Daemon) Stats() Stats {
	cachedSessions, poolInFlight, poolCapacity := 0, 0, 0
	if d.sessions != nil {
		cachedSessions = d.sessions.size()
		poolInFlight = d.sessions.poolInFlight()
		poolCapacity = d.sessions.poolCapacity()
	}
	return d.counters.snapshot(cachedSessions, poolInFlight, poolCapacity)
}

func (d *Daemon) recordStats() {
	if d.StatsPath == "" {
		return
	}
	if err := writeStatsFile(d.StatsPath, d.Stats()); err != nil {
		d.Log.Printf("FAILED writing stats file %s: %v", d.StatsPath, err)
	}
}

// Run binds the socket and accepts connections until the listener errors
// (or ctx is canceled). It does not return on a healthy daemon.
func (d *Daemon) Run(ctx context.Context) error {
	// A stale socket from a previous crashed/killed daemon must not block
	// this one from binding — same "a leftover lock must self-heal" idea as
	// the real spawn-lock staleness handling, simplified: only one daemon is
	// ever meant to hold this socket, so remove-then-bind is enough.
	_ = os.Remove(d.SocketPath)

	// Opened once for the whole daemon lifetime, not per event — a daemon
	// that lives for days handling occasional hook events must not pay a
	// fresh connection (a real TCP handshake against Postgres, or SQLite's
	// own per-open PRAGMA setup) on every single tool call. Real risk this
	// avoids, not a hypothetical one: many concurrent tool-call events
	// (up to MaxConcurrent observer sessions at once, each ending in an
	// Insert) each opening their own Postgres connection is exactly the
	// kind of connection churn that can exhaust a shared server's
	// max_connections under real load.
	st, err := backend.Open(ctx, d.DBPath, 0)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", d.DBPath, err)
	}
	d.st = st
	defer st.Close()

	ln, err := net.Listen("unix", d.SocketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	// Also removed at the top of the NEXT Run() regardless, but cleaning up
	// here too means a graceful shutdown doesn't leave a dead socket file
	// for IsRunning callers to trip over in the gap before a new daemon starts.
	defer os.Remove(d.SocketPath)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	p := pool.New(d.MaxConcurrent)
	d.sessions = newSessionCache(p, func(ctx context.Context) (observer.Handle, error) {
		return observer.New(ctx, d.Model)
	})
	defer d.sessions.closeAll()

	go func() {
		ticker := time.NewTicker(idleEvictInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.sessions.evictIdle()
			}
		}
	}()

	d.Log.Printf("worker daemon up, pid=%d, listening on %s, max_concurrent=%d",
		os.Getpid(), d.SocketPath, d.MaxConcurrent)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go d.handleConn(ctx, conn)
	}
}

// handleConn reads exactly one hook payload, then processes it in the
// background. The client (hook.Forward) doesn't wait for a response — it
// writes and closes — so there's nothing to ACK over the wire; what matters
// is that the work now happens on the daemon's own process, which the
// client's exit cannot kill.
func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	raw, err := io.ReadAll(bufio.NewReader(conn))
	if err != nil {
		d.Log.Printf("FAILED reading from client: %v", err)
		return
	}
	go d.process(ctx, raw)
}

func (d *Daemon) process(ctx context.Context, raw []byte) {
	in, err := claudeagent.ParseHookInput(bytes.NewReader(raw))
	if err != nil {
		d.Log.Printf("FAILED parsing payload: %v (%d bytes)", err, len(raw))
		return
	}
	if in.ToolName == "" {
		d.Log.Printf("skip: no tool_name (hook_event_name=%s)", in.Event)
		return
	}
	// Stats are written once after every genuine work attempt below — not
	// for the two early-return cases above, which counted nothing.
	defer d.recordStats()

	queueStart := time.Now()
	entry, err := d.sessions.getOrCreate(ctx, in.SessionID)
	if err != nil {
		d.Log.Printf("FAILED to get/create observer session for %s: %v", in.SessionID, err)
		d.counters.observerErrors.Add(1)
		return
	}
	queued := time.Since(queueStart)

	// Serialize turns on this one session's subprocess — held only for the
	// observer call itself, not the sqlite/embedding work below, so a slow
	// embedding call on one turn doesn't block the next turn on the SAME
	// session from at least starting its observer call... actually it must:
	// turns for one session are inherently sequential on one pipe. Held
	// across the whole function body below is correct, not incidental.
	entry.mu.Lock()
	defer entry.mu.Unlock()

	d.Log.Printf("observing tool=%s session=%s cwd=%s (queued %s, cached_sessions=%d)",
		in.ToolName, in.SessionID, in.Cwd, queued.Round(time.Millisecond), d.sessions.size())

	tc := transcript.ToolCall{
		ToolName:   in.ToolName,
		ToolInput:  transcript.Truncate(string(in.ToolInput)),
		ToolOutput: transcript.Truncate(string(in.ToolResponse)),
	}

	turn, turnErr := entry.handle.Observe(tc)
	if turnErr != nil {
		// A broken subprocess must not stay cached to fail identically on
		// every future turn for this session — evict it regardless of what
		// happens next. The next turn for this session_id will lazily spawn
		// a fresh one via getOrCreate.
		d.sessions.evict(in.SessionID)

		// Transient/rate-limit failures get exactly one retry on a fresh
		// one-shot session — the same class of failure this project's
		// classify package exists to distinguish from a hopeless retry
		// (auth/setup/quota/unrecoverable, which would just fail identically
		// again). Anything else fails this turn immediately.
		if ce, ok := turnErr.(*classify.Error); ok && (ce.Kind == classify.Transient || ce.Kind == classify.RateLimit) {
			d.Log.Printf("observer turn failed (session evicted), retrying once on a fresh session: %v", turnErr)
			turn, turnErr = observer.ObserveOneShot(ctx, d.Model, tc)
		}
		if turnErr != nil {
			d.Log.Printf("FAILED observer turn: %v", turnErr)
			d.counters.observerErrors.Add(1)
			return
		}
	}

	project := filepath.Base(in.Cwd)
	if project == "" || project == "." {
		project = filepath.Base(filepath.Dir(in.TranscriptPath))
	}
	hash := store.ContentHash(in.SessionID, in.ToolName, tc.ToolInput, tc.ToolOutput)
	res, err := d.st.Insert(in.SessionID, project, in.ToolName, hash, turn.Observation, turn.Result.CostUSD)
	if err != nil {
		d.Log.Printf("FAILED sqlite insert: %v", err)
		d.counters.insertErrors.Add(1)
		return
	}
	if !res.Inserted {
		// Not an error — a hook can legitimately fire more than once for the
		// same event (documented as at-least-once delivery), and re-ingesting
		// an already-processed transcript should be a no-op, not a duplicate.
		d.Log.Printf("duplicate observation (same tool call already persisted as id=%d), skipped", res.ID)
		d.counters.duplicates.Add(1)
		return
	}
	d.Log.Printf("persisted observations.id=%d title=%q cost=$%.4f", res.ID, turn.Observation.Title, turn.Result.CostUSD)
	d.counters.processed.Add(1)
	d.counters.touch()

	if d.EmbedModel == "" {
		return
	}
	text := embed.ObservationText(turn.Observation.Title, turn.Observation.Subtitle,
		turn.Observation.Narrative, turn.Observation.Facts)
	vec, err := embed.NewClient(d.EmbedModel).Embed(text)
	if err != nil {
		// Additive only — keyword search on the row just inserted still
		// works without it. A missing/unreachable Ollama must not undo a
		// successful observation.
		d.Log.Printf("embedding failed for observations.id=%d (semantic search won't find it): %v", res.ID, err)
		d.counters.embedErrors.Add(1)
		return
	}
	if err := d.st.SaveEmbedding(res.ID, vec); err != nil {
		d.Log.Printf("saving embedding for observations.id=%d failed: %v", res.ID, err)
		d.counters.embedErrors.Add(1)
	}
}
