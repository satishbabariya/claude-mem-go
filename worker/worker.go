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
	"claude-mem-go/logging"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/backend"
	"claude-mem-go/classify"
	"claude-mem-go/embed"
	"claude-mem-go/excludeproject"
	"claude-mem-go/hook"
	"claude-mem-go/observer"
	"claude-mem-go/pool"
	"claude-mem-go/privacy"
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
	Log           *logging.Logger
	// Version is the build string of the binary running this daemon,
	// supplied by the caller (cmd/worker) because runtime/debug's build
	// info belongs to the main package. Published in Stats so a stale
	// daemon is detectable — see Stats.Version.
	Version string
	// StatsPath is where Stats snapshots are written after every processed
	// event — see stats.go. Empty disables writing (tests mostly want this;
	// a real daemon always wants it, so cmd's daemon construction sets it to
	// DefaultStatsPath()).
	StatsPath string
	// MetricsAddr, when non-empty, serves d.Stats() in Prometheus text
	// format at "<MetricsAddr>/metrics" — see metrics.go. Empty (the
	// default) disables it entirely: this is the one thing about this
	// daemon that listens on more than a Unix socket, so it's opt-in, not
	// on by default.
	MetricsAddr string
	// ExcludedProjects is a comma-separated list of glob patterns (see
	// excludeproject) — a project matching one is never observed
	// automatically, the real claude-mem CLAUDE_MEM_EXCLUDED_PROJECTS
	// feature this port previously had no equivalent of at all. Empty
	// (the default) excludes nothing, identical to today's behavior.
	ExcludedProjects string

	sessions *sessionCache
	counters statsCounters
	st       store.Backend

	// inflight and inflightOnce back getInflight (inflight.go) — see its
	// own doc comment for why a caller like the Stop hook can query this
	// directly instead of inferring "is the worker still catching up on
	// this session" from watching row counts.
	inflight     *inflightTracker
	inflightOnce sync.Once

	// processWG tracks every dispatched process() goroutine — see
	// dispatchProcess and Run's own doc comment on why this exists:
	// sessionCache.closeAll's own doc comment documents this exact gap it
	// deliberately didn't attempt. A handleConn goroutine dispatched from
	// Run's Accept loop a moment before shutdown began, for a session
	// that's never been seen before, calls sessionCache.getOrCreate AFTER
	// closeAll's snapshot of its map was already taken — closeAll returns
	// without ever knowing that goroutine exists, and without this
	// WaitGroup, st.Close() could run while its own Insert/SaveEmbedding
	// call is still in flight against the now-closed store, silently
	// dropping the observation.
	processWG sync.WaitGroup
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
	snap := d.counters.snapshot(cachedSessions, poolInFlight, poolCapacity)
	// Redacted here rather than at the reader: this snapshot is written
	// to a world-readable file in the user's home, and a Postgres DSN
	// carries a password.
	snap.Store = store.RedactDSN(d.DBPath)
	snap.Version = d.Version
	snap.PID = os.Getpid()
	return snap
}

func (d *Daemon) recordStats() {
	if d.StatsPath == "" {
		return
	}
	if err := writeStatsFile(d.StatsPath, d.Stats()); err != nil {
		d.Log.Errorf("FAILED writing stats file %s: %v", d.StatsPath, err)
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
	st, err := backend.Open(ctx, d.DBPath, 0, 0)
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
	// Registered AFTER closeAll's defer, so it runs BEFORE closeAll (defers
	// run LIFO): drains every dispatched process() goroutine — including
	// one for a brand-new session that closeAll's own map snapshot could
	// never have known about — before closeAll (and st.Close after it)
	// ever run. See processWG's own doc comment for the exact gap this
	// closes.
	defer d.waitForProcessDrain()

	go func() {
		ticker := time.NewTicker(idleEvictInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.sessions.evictIdle()
				d.sessions.evictStalePrivacy()
				d.sessions.evictStaleDedupe()
			}
		}
	}()

	if d.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", MetricsHandler(d))
		metricsSrv := &http.Server{Addr: d.MetricsAddr, Handler: mux}
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				d.Log.Errorf("FAILED serving metrics on %s: %v", d.MetricsAddr, err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsSrv.Shutdown(shutdownCtx)
		}()
		d.Log.Printf("worker metrics listening on http://%s/metrics", d.MetricsAddr)
	}

	// p.Capacity(), not d.MaxConcurrent: pool.New clamps a non-positive
	// value to 1 rather than passing it straight through (see its own doc
	// comment — a negative value crashes Go's make(chan) outright), so
	// logging the raw flag value here would claim a max_concurrent that
	// doesn't match what the pool is actually enforcing.
	d.Log.Printf("worker daemon up, pid=%d, listening on %s, max_concurrent=%d",
		os.Getpid(), d.SocketPath, p.Capacity())

	// Publish the daemon's identity — crucially, which store it opened —
	// as soon as it is listening, not only once it has processed
	// something. recordStats was previously reached only from the event
	// path, so a freshly started daemon wrote no stats file at all and
	// `doctor` could not report anything about it.
	//
	// That is backwards for the mismatch this file now detects: a worker
	// writing to a different store than everything else resolves is most
	// likely immediately after $CLAUDE_MEM_DB changed, which is exactly
	// before any work has happened. The window where the problem was
	// undetectable was the window where it was most likely.
	d.recordStats()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go d.handleConn(ctx, conn)
	}
}

// handleConnReadTimeout bounds how long handleConn will wait for a client
// to finish sending its payload — see handleConn's own doc comment for
// the real leak this closes. A var, not a const, so a test can shrink it
// to run the same "did the deadline actually fire" assertion near-
// instantly rather than waiting out the real production value.
var handleConnReadTimeout = 30 * time.Second

// handleConn reads exactly one hook payload, then processes it in the
// background. The client (hook.Forward) doesn't wait for a response — it
// writes and closes — so there's nothing to ACK over the wire; what matters
// is that the work now happens on the daemon's own process, which the
// client's exit cannot kill.
//
// A real gap found by hand, not yet hit in production but real
// nonetheless: nothing here bounded wall-clock time, only byte count
// (hook.MaxPayloadBytes) — a client that dials this socket and then never
// writes or closes (a stalled process, a hung network namespace, or
// simply a bug in some future or different client that doesn't go
// through hook.Forward) leaks this goroutine and its underlying FD for as
// long as the daemon runs, which per sessionIdleTimeout's own doc comment
// is meant to be days. More relevant now that INFLIGHT queries
// (hook.QueryInFlight) share this same socket as a synchronous
// request/response protocol, not just the original one-way hook forward
// — the client side already sets its own deadline (see QueryInFlight),
// but nothing on this, the server side, ever did. handleConnReadTimeout
// is generous enough that neither a normal hook forward (near-instant on
// a local Unix socket) nor a real INFLIGHT query ever comes close to it.
func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(handleConnReadTimeout))
	// hook.MaxPayloadBytes, not unbounded: hook.Forward already enforces
	// this on the client side, but this daemon is the one long-lived
	// process every project on the machine shares — a future or
	// different client writing to this socket without going through
	// Forward must not be able to balloon its memory with a single
	// abnormally large payload. LimitReader+1 so the size check below can
	// tell "exactly at the cap" apart from "over it" without needing to
	// buffer more than one byte past the limit.
	raw, err := io.ReadAll(io.LimitReader(bufio.NewReader(conn), hook.MaxPayloadBytes+1))
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			d.Log.Printf("client connected but never sent a complete payload within %s — closing (stalled client, or a bug in a caller not going through hook.Forward)", handleConnReadTimeout)
		} else {
			d.Log.Errorf("FAILED reading from client: %v", err)
		}
		return
	}
	if len(raw) > hook.MaxPayloadBytes {
		d.Log.Warnf("REJECTED payload exceeding %d bytes from a client (likely an abnormally large tool_response) — not processing", hook.MaxPayloadBytes)
		return
	}

	// The in-flight query protocol is the one request on this socket that
	// gets a synchronous reply — everything else (a hook payload) is
	// fire-and-forget, matching hook.Forward's own contract that it never
	// waits for a response. A query is distinguished by a plain-text
	// prefix that can never collide with a real hook payload, which is
	// always JSON and so always starts with '{'.
	if sid, ok := hook.ParseInFlightQuery(raw); ok {
		n := d.getInflight().count(sid)
		_, _ = conn.Write([]byte(strconv.Itoa(n)))
		return
	}
	if sid, private, ok := hook.ParsePrivacyMarker(raw); ok {
		d.sessions.setPrivate(sid, private)
		return
	}
	if sid, ok := hook.ParsePrivacyQuery(raw); ok {
		reply := "0"
		if d.sessions.isPrivate(sid) {
			reply = "1"
		}
		_, _ = conn.Write([]byte(reply))
		return
	}
	if sid, promptHash, ok := hook.ParseDedupeQuery(raw); ok {
		reply := "0"
		if d.sessions.checkAndRecordPrompt(sid, promptHash) {
			reply = "1"
		}
		_, _ = conn.Write([]byte(reply))
		return
	}

	d.dispatchProcess(ctx, raw)
}

// dispatchProcess runs process in its own goroutine, tracked by
// processWG — see processWG's own doc comment for the shutdown-drain
// gap this closes. The Add happens synchronously, on the caller's own
// goroutine, before the tracked goroutine is even started: a shutdown
// racing this exact call must see the Add happen-before it can possibly
// reach processWG.Wait (Run's defer ordering places that wait after
// every handleConn dispatch from the Accept loop has already returned),
// so there's no window where a real dispatch could be missed.
func (d *Daemon) dispatchProcess(ctx context.Context, raw []byte) {
	d.processWG.Add(1)
	go func() {
		defer d.processWG.Done()
		d.process(ctx, raw)
	}()
}

// waitForProcessDrain waits for every process() goroutine dispatchProcess
// has ever started to finish, up to processDrainGracePeriod — matching
// the same bounded-wait shape sessionCache.closeAll already uses
// (ctx cancellation aborts an in-flight observer call promptly, see
// process's own reliance on that during shutdown, so this is expected to
// return well before the grace period in the overwhelming majority of
// real shutdowns, not routinely hit it).
func (d *Daemon) waitForProcessDrain() {
	done := make(chan struct{})
	go func() {
		d.processWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(processDrainGracePeriod):
		d.Log.Printf("shutdown: %s grace period elapsed waiting for in-flight events to finish — proceeding anyway", processDrainGracePeriod)
	}
}

// processDrainGracePeriod bounds waitForProcessDrain the same way
// closeAllGracePeriod (sessions.go) bounds closeAll's own per-session
// wait — a separate constant, not a shared one, since the two guard
// conceptually different things even though they currently agree on
// the same duration.
var processDrainGracePeriod = 5 * time.Second

func (d *Daemon) process(ctx context.Context, raw []byte) {
	// This runs in its own goroutine (see dispatchProcess) — an
	// unrecovered panic here doesn't just fail this one event, it
	// crashes the ENTIRE daemon process, taking memory capture down for
	// every project on the machine sharing this one daemon. Found not
	// hypothetically: a real, reproducible panic existed in
	// store.Store.SemanticSearch (a negative limit slicing out of
	// bounds) before that was fixed — this recover is the backstop for
	// that entire class of bug (this one and any other not yet found),
	// not a substitute for fixing root causes when they're found.
	defer func() {
		if r := recover(); r != nil {
			d.Log.Printf("PANIC recovered in process (%d byte payload): %v", len(raw), r)
			d.counters.observerErrors.Add(1)
		}
	}()

	// An empty payload is a liveness probe, not a failure. worker.IsRunning
	// dials the socket and closes it immediately, which is how `start`
	// decides whether to spawn and how `doctor` reports reachability — so
	// on a perfectly healthy system this arrives on every SessionStart.
	//
	// It was logged as an error, and after severities were introduced that
	// became actively misleading: ERROR is exactly what an operator greps
	// for, and a clean three-session soak produced three of them. Zero
	// bytes with an immediate EOF is precisely the probe's signature — a
	// client that died mid-send leaves a partial payload, which still
	// takes the error path below.
	if len(raw) == 0 {
		d.Log.Debugf("liveness probe (empty payload), nothing to process")
		return
	}

	in, err := claudeagent.ParseHookInput(bytes.NewReader(raw))
	if err != nil {
		d.Log.Errorf("FAILED parsing payload: %v (%d bytes)", err, len(raw))
		return
	}
	if in.ToolName == "" {
		d.Log.Printf("skip: no tool_name (hook_event_name=%s)", in.Event)
		return
	}
	// Checked before anything else genuinely happens — deliberately
	// before ever spawning/reusing an observer subprocess, not just
	// before the store Insert — so an excluded project never pays for a
	// real LLM call it's about to throw away. Real claude-mem's
	// equivalent (shouldTrackProject) is checked at the very top of
	// every automatic hook handler for the identical reason.
	if excludeproject.IsExcluded(in.Cwd, d.ExcludedProjects) {
		d.Log.Printf("skip: project excluded (cwd=%s)", in.Cwd)
		return
	}
	// Real claude-mem's own PrivacyCheckValidator suppresses observation
	// generation for an entire turn once its prompt stripped to nothing
	// (see privacy package) — this port had tag-stripping on the prompt
	// and tool payloads individually, but nothing carried that same
	// suppression across to a private turn's tool calls. d.sessions.isPrivate
	// defaults to false for a session with no flag set at all, not just one
	// explicitly cleared — see its own doc comment for why that default
	// matters (a session whose UserPromptSubmit hasn't reported in yet must
	// never be silently treated as private).
	if d.sessions.isPrivate(in.SessionID) {
		d.Log.Printf("skip: session %s marked private for this turn", in.SessionID)
		return
	}
	// Stats are written once after every genuine work attempt below — not
	// for the three early-return cases above, which counted nothing.
	defer d.recordStats()

	// Marks this session "in flight" for the ENTIRE remainder of this
	// function — through the observer call, the store insert, and the
	// embedding call — not just the part guarded by entry.mu below, so a
	// caller querying in-flight state sees "still working" for exactly as
	// long as a row for this event is genuinely not yet persisted (or, for
	// the embedding step, not yet fully queryable by semantic search).
	d.getInflight().inc(in.SessionID)
	defer d.getInflight().dec(in.SessionID)

	queueStart := time.Now()
	entry, err := d.sessions.getOrCreate(ctx, in.SessionID)
	if err != nil {
		d.Log.Errorf("FAILED to get/create observer session for %s: %v", in.SessionID, err)
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

	// Privacy tags are stripped BEFORE truncation, not after: a <private>
	// block that happens to straddle Truncate's own cutoff would otherwise
	// leave a dangling, unclosed tag that this package's regex can never
	// match — stripping first guarantees any tag present is still whole.
	// Privacy tags are stripped BEFORE truncation, not after: a <private>
	// block that happens to straddle Truncate's own cutoff would otherwise
	// leave a dangling, unclosed tag that this package's regex can never
	// match — stripping first guarantees any tag present is still whole.
	tc := transcript.ToolCall{
		ToolName:   in.ToolName,
		ToolInput:  transcript.Truncate(privacy.StripMemoryTags(string(in.ToolInput))),
		ToolOutput: transcript.Truncate(privacy.StripMemoryTags(string(in.ToolResponse))),
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
			d.Log.Warnf("observer turn failed (session evicted), retrying once on a fresh session: %v", turnErr)
			turn, turnErr = observer.ObserveOneShot(ctx, d.Model, tc)
		}
		if turnErr != nil {
			d.Log.Errorf("FAILED observer turn: %v", turnErr)
			d.counters.observerErrors.Add(1)
			return
		}
	}
	// Refresh lastUsed now that the turn actually finished, not just when
	// it started (getOrCreate already covers that half) — see touch's own
	// doc comment for why evictIdle's safety margin depends on this
	// reflecting real last-activity time, not merely a turn's start.
	d.sessions.touch(in.SessionID)

	project := store.ProjectFor(in.Cwd)
	if project == "" || project == "." {
		project = filepath.Base(filepath.Dir(in.TranscriptPath))
	}
	hash := store.ContentHash(in.SessionID, in.ToolName, tc.ToolInput, tc.ToolOutput)
	// Canonicalize the observer's file paths before they are stored.
	//
	// The model is told only "<file>...</file>", so it sometimes shortens
	// an absolute tool input to a repo-relative path. The PreToolUse
	// file-context hook looks up the absolute path Claude Code puts in
	// the payload, so a relative row never matches and the lookup
	// silently finds nothing. Measured in this project's own store: 8
	// absolute vs 7 relative paths recorded, and only 2 successful
	// injections in the entire history of file-context.log.
	//
	// Done here, on the write path, so there is exactly one canonical
	// form in the column — which the SQLite observation_files trigger and
	// Postgres's GIN indexes both derive from, so they stay consistent
	// for free.
	turn.Observation.FilesRead = store.NormalizeFilePaths(in.Cwd, turn.Observation.FilesRead)
	turn.Observation.FilesModified = store.NormalizeFilePaths(in.Cwd, turn.Observation.FilesModified)

	res, err := d.st.Insert(in.SessionID, project, in.ToolName, hash, turn.Observation, turn.Result.CostUSD)
	if err != nil {
		d.Log.Errorf("FAILED sqlite insert: %v", err)
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
		d.Log.Warnf("embedding failed for observations.id=%d (semantic search won't find it): %v", res.ID, err)
		d.counters.embedErrors.Add(1)
		return
	}
	if err := d.st.SaveEmbedding(res.ID, vec); err != nil {
		d.Log.Warnf("saving embedding for observations.id=%d failed: %v", res.ID, err)
		d.counters.embedErrors.Add(1)
	}
}
