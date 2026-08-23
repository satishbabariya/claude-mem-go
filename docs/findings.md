# Findings: defects found and fixed, with the evidence

The record of what this port got wrong and how each fix was proven. Every
entry follows the same shape — **Symptom**, **Cause**, **Fix**, **How it was
verified** — and "break/restore" means the fix was temporarily reverted to
confirm the new test actually fails without it. Numbers are as measured at the
time. Current behaviour is documented in [hooks.md](hooks.md),
[postgres.md](postgres.md), [plugin-install.md](plugin-install.md), and
[development.md](development.md); this file is the why.

## Contents

- [Capture and the worker daemon](#capture-and-the-worker-daemon)
  - [PostToolUse hook lost observations when run inline](#posttooluse-hook-lost-observations-when-run-inline)
  - [Daemon reopened its store on every event](#daemon-reopened-its-store-on-every-event)
  - [Hook payloads had no size bound](#hook-payloads-had-no-size-bound)
  - [Socket reads had no deadline](#socket-reads-had-no-deadline)
  - [Truncation corrupted multi-byte characters](#truncation-corrupted-multi-byte-characters)
  - [Negative limits, and no panic recovery anywhere](#negative-limits-and-no-panic-recovery-anywhere)
  - [`-max-concurrent -1` crashed the daemon at startup](#-max-concurrent--1-crashed-the-daemon-at-startup)
  - [Idle-session reaper could close a subprocess mid-turn](#idle-session-reaper-could-close-a-subprocess-mid-turn)
  - [Shutdown had the same mid-turn gap](#shutdown-had-the-same-mid-turn-gap)
  - [Shutdown raced a brand-new session's goroutine](#shutdown-raced-a-brand-new-sessions-goroutine)
  - [A third concurrent session was never captured](#a-third-concurrent-session-was-never-captured)
  - [Concurrent sessions were dropped after the slot deadline](#concurrent-sessions-were-dropped-after-the-slot-deadline)
  - [A stale daemon applied old rules after an upgrade](#a-stale-daemon-applied-old-rules-after-an-upgrade)
  - [Embedding failed permanently on a transient Ollama error](#embedding-failed-permanently-on-a-transient-ollama-error)
  - [Log files grew without bound](#log-files-grew-without-bound)
  - [Failures logged at INFO; WARN unused](#failures-logged-at-info-warn-unused)
  - [Socket path over the OS limit killed the daemon silently](#socket-path-over-the-os-limit-killed-the-daemon-silently)
- [Hooks](#hooks)
  - [Stop summarized before the last observation landed](#stop-summarized-before-the-last-observation-landed)
  - [Stop's first settle rule misread a one-call session](#stops-first-settle-rule-misread-a-one-call-session)
  - [Stop's second settle rule misread a sequential plateau](#stops-second-settle-rule-misread-a-sequential-plateau)
  - [Stop's ceiling cut off a 104-second observation](#stops-ceiling-cut-off-a-104-second-observation)
  - [Stop never produced a summary under `claude -p`](#stop-never-produced-a-summary-under-claude--p)
  - [Stop summaries never embedded](#stop-summaries-never-embedded)
  - [Stop summarized the start of long sessions and dropped the end](#stop-summarized-the-start-of-long-sessions-and-dropped-the-end)
  - [Stop re-ran on `stop_hook_active` retries](#stop-re-ran-on-stop_hook_active-retries)
  - [File-context and Stop leaked into subagents](#file-context-and-stop-leaked-into-subagents)
  - [Prompt-context embedded internal protocol notifications](#prompt-context-embedded-internal-protocol-notifications)
  - [Prompt-context injected twice on a duplicate `UserPromptSubmit`](#prompt-context-injected-twice-on-a-duplicate-userpromptsubmit)
  - [Worktree sessions read and wrote different project names](#worktree-sessions-read-and-wrote-different-project-names)
  - [The handler audit against real claude-mem](#the-handler-audit-against-real-claude-mem)
  - [Hooks were verified live against real sessions](#hooks-were-verified-live-against-real-sessions)
  - [`hooks.json` could name a subcommand that did not exist](#hooksjson-could-name-a-subcommand-that-did-not-exist)
- [Privacy](#privacy)
  - [No `<private>` tag redaction anywhere](#no-private-tag-redaction-anywhere)
  - [A private prompt did not suppress its tool calls or summary](#a-private-prompt-did-not-suppress-its-tool-calls-or-summary)
  - [No way to exclude a project](#no-way-to-exclude-a-project)
- [Search and MCP tools](#search-and-mcp-tools)
  - [Unscoped search leaked across projects](#unscoped-search-leaked-across-projects)
  - [Paging needed a deterministic order](#paging-needed-a-deterministic-order)
  - [Postgres placeholder numbering was hardcoded](#postgres-placeholder-numbering-was-hardcoded)
  - [`search` lacked date range and sort order](#search-lacked-date-range-and-sort-order)
  - [`search` could not filter on more than one type](#search-could-not-filter-on-more-than-one-type)
  - [`get_observations` failed on large id lists](#get_observations-failed-on-large-id-lists)
  - [`timeline` crossed project boundaries and accepted negative depth](#timeline-crossed-project-boundaries-and-accepted-negative-depth)
  - [`timeline` defaulted to depth 3 and ignored a caller mistake](#timeline-defaulted-to-depth-3-and-ignored-a-caller-mistake)
  - [`add_observation` was invisible to semantic search](#add_observation-was-invisible-to-semantic-search)
  - [`add_observation` had no size bounds](#add_observation-had-no-size-bounds)
  - [`recent_observations` did not match what SessionStart injects](#recent_observations-did-not-match-what-sessionstart-injects)
  - [`observation_context` and `important_workflow`](#observation_context-and-important_workflow)
  - [MCP `serverInfo.version` was three releases stale](#mcp-serverinfoversion-was-three-releases-stale)
- [Store: SQLite](#store-sqlite)
  - [Concurrent writers hit "database is locked"; cascades never fired](#concurrent-writers-hit-database-is-locked-cascades-never-fired)
  - [The FTS5 delete trigger was invalid](#the-fts5-delete-trigger-was-invalid)
  - [A corrupt backup imported "successfully"](#a-corrupt-backup-imported-successfully)
- [Store: Postgres](#store-postgres)
  - [`CLAUDE_MEM_DB` did not exist, so an installed plugin could not reach Postgres](#claude_mem_db-did-not-exist-so-an-installed-plugin-could-not-reach-postgres)
  - [DSN passwords reached logs, and the first redactor leaked on malformed input](#dsn-passwords-reached-logs-and-the-first-redactor-leaked-on-malformed-input)
  - [`hnsw.ef_search` was not range-checked by Postgres](#hnswef_search-was-not-range-checked-by-postgres)
  - [No statement timeout](#no-statement-timeout)
  - [No per-attempt timeout on the initial ping](#no-per-attempt-timeout-on-the-initial-ping)
  - [Pool size and idle timeout were hardcoded, and the idle timeout was 10x off](#pool-size-and-idle-timeout-were-hardcoded-and-the-idle-timeout-was-10x-off)
  - [Open failed permanently if Postgres was still starting](#open-failed-permanently-if-postgres-was-still-starting)
  - [Keyword search ignored `facts` and `concepts`](#keyword-search-ignored-facts-and-concepts)
  - [Boolean operators worked on SQLite and broke on Postgres](#boolean-operators-worked-on-sqlite-and-broke-on-postgres)
  - [The project filter made semantic search return zero rows](#the-project-filter-made-semantic-search-return-zero-rows)
  - [Hard-wired to 768-dimension embeddings](#hard-wired-to-768-dimension-embeddings)
  - [`observations.type` was never validated](#observationstype-was-never-validated)
  - [Scale problems at 250,000 rows](#scale-problems-at-250000-rows)
  - [A 95% recall figure was wrong and retracted](#a-95-recall-figure-was-wrong-and-retracted)
- [Operations](#operations)
  - [`doctor` never checked the plugin was installed](#doctor-never-checked-the-plugin-was-installed)
  - ["Installed" did not mean "able to run"](#installed-did-not-mean-able-to-run)
  - [`doctor` reported a dead daemon's stats as live](#doctor-reported-a-dead-daemons-stats-as-live)
  - [`doctor` could not see dimension drift or a missing index](#doctor-could-not-see-dimension-drift-or-a-missing-index)
  - [The read path had no instrumentation](#the-read-path-had-no-instrumentation)
  - [`prune` deleted nothing, ever](#prune-deleted-nothing-ever)
  - [Export dropped embeddings](#export-dropped-embeddings)
  - [`reembed` and the stats file](#reembed-and-the-stats-file)
  - [`CLAUDE_MEM_LOG_LEVEL` worked but was undiscoverable](#claude_mem_log_level-worked-but-was-undiscoverable)
  - [The usage message advertised 3 of 18 subcommands](#the-usage-message-advertised-3-of-18-subcommands)
- [Install and skills](#install-and-skills)
  - [A git-installed plugin shipped no binary](#a-git-installed-plugin-shipped-no-binary)
  - [Self-heal could not repair a broken binary](#self-heal-could-not-repair-a-broken-binary)
  - [Skills went stale](#skills-went-stale)
- [Build, tests, and migrations](#build-tests-and-migrations)
  - [`migrate.Run` did not sort and did not reject duplicates](#migraterun-did-not-sort-and-did-not-reject-duplicates)
  - [Concurrent first-open migrations raced](#concurrent-first-open-migrations-raced)
  - [Tests wrote into the real store](#tests-wrote-into-the-real-store)
  - [Release binaries were stamped `-dirty`](#release-binaries-were-stamped--dirty)
  - [CI history](#ci-history)

## Capture and the worker daemon

### PostToolUse hook lost observations when run inline

**Symptom:** The first design had the `PostToolUse` hook run the observer
itself; against a real session, both attempts died mid-observation.
**Cause:** `"async": true` only means Claude Code does not wait for the hook;
the hook's child process does not survive the `claude` process exiting.
**Fix:** A daemon started once from `SessionStart`, detached via `Setsid`, with
the hook doing nothing but a socket write. See [hooks.md](hooks.md#why-the-workerhook-split).
**How it was verified:** Real Claude Code sessions through the real hooks;
observations persisted where the inline version lost them.

### Daemon reopened its store on every event

**Symptom:** A fresh `memory.Backend` (a TCP handshake against Postgres) per
tool call, indefinitely, on a process meant to run for days; the Postgres
pool was also unbounded (`database/sql`'s default).
**Fix:** Open once for the daemon's lifetime; `SetMaxOpenConns`/`SetMaxIdleConns`
on the pool.
**How it was verified:** A live daemon restarted and sent multiple real events
through the one handle.

### Hook payloads had no size bound

**Symptom:** A `Bash` that cats a multi-gigabyte file or a `Read` of a huge log
would be read whole by the one daemon every project shares and stringified
into an observer prompt at per-token cost.
**Fix:** `hook.MaxPayloadBytes` (8MB, matching the MCP JSON-RPC line cap).
`hook.Forward` rejects an oversized payload and the daemon enforces the same
bound on its read. Rejected whole, not truncated: a truncated JSON payload is
corrupt.
**How it was verified:** An 8MB+100-byte payload over a throwaway daemon's
real socket was logged as rejected, the daemon stayed up, and a normal
payload then processed normally.

### Socket reads had no deadline

**Symptom:** A client that dialled the socket and never wrote or closed leaked
its goroutine and file descriptor for the daemon's lifetime — more exposed
once `INFLIGHT` made the socket a request/response channel (the client set a
deadline; the server did not).
**Fix:** `handleConnReadTimeout` of 30s, far above a local forward or query.
**How it was verified:** A real `net.Conn` test with a shrunk timeout and a
client that never writes confirms `handleConn` returns; break/restore (the
test times out without the deadline).

### Truncation corrupted multi-byte characters

**Symptom:** `transcript.Truncate` cut `tool_input`/`tool_response` at
`FieldCap` (1500 bytes) with a plain byte slice; any non-ASCII character
straddling byte 1500 (accented paths, emoji, `tree` box-drawing) produced
invalid UTF-8 — an eventual certainty over a daemon's lifetime.
**Fix:** Walk back to the nearest rune start before cutting (at most 3 bytes).
**How it was verified:** A unit test engineers the cut mid-character and
asserts valid UTF-8; a real `tool_response` straddling the boundary went
through an isolated daemon, observer, `claude` subprocess, and store cleanly.

### Negative limits, and no panic recovery anywhere

**Symptom:** `Search`, `RecentByProject`, `BySessionID`, `ObservationsForFile`,
`ExportAll`, and `ObservationsNeedingEmbedding` returned every row for
`limit=-1` (SQLite treats a negative `LIMIT` as unlimited); `SemanticSearch`
sliced `all[:limit]` in Go and panicked. Neither the daemon's per-event
goroutine nor the MCP handler recovered panics, so one would kill the whole
process — the daemon for every project, or the session's MCP connection.
**Cause:** No clamping at the store layer; live MCP callers substituted
defaults first, which is not the same as the API being safe.
**Fix:** `clampNegativeLimit` in both backends; `recover` in
`worker.Daemon.process` and `mcpserver.Server.handle` (later also in
`handleConn`).
**How it was verified:** A regression test per method on both backends
(SQLite: zero rows; Postgres: a clean clamp instead of `LIMIT must not be
negative`); fault-injection tests with a `Backend` that panics on every call
through the real request paths; a live `claude` session whose MCP server kept
serving afterward.

### `-max-concurrent -1` crashed the daemon at startup

**Symptom:** `make(chan T, n)` panics on a negative `n`, so the daemon died
before binding its socket; zero would have blocked every `Acquire` forever.
The startup log also printed the raw flag rather than the pool's capacity.
**Fix:** `pool.New` clamps anything below 1 to 1; the log line uses
`pool.Capacity()`.
**How it was verified:** A real worker started with `-max-concurrent -1`
stayed alive and logged `max_concurrent=1`.

### Idle-session reaper could close a subprocess mid-turn

**Symptom:** `evictIdle` closed a cached `claude` subprocess after
`sessionIdleTimeout` (10 minutes) based on a `lastUsed` refreshed only at
turn *start*; a single observation has measured 104 seconds, so a turn could
still be reading that subprocess's stdout when the sweep closed it.
`Session.Close`/`Send` in the SDK have no synchronization either.
**Fix:** `entry.mu.TryLock()` before evicting (failure means a turn is
active; the sweep skips it and retries a minute later); `touch` refreshes
`lastUsed` when a turn finishes.
**How it was verified:** A concurrency test holds the per-session mutex with a
blocking `Observe`, back-dates `lastUsed`, confirms the sweep leaves the
handle open, releases, and confirms a later sweep evicts it; passes under
`-race`; break/restore. Live: with the timeout shrunk to 3s and the sweep to
1s, a ~9-second real observation spanned several sweeps and persisted.

### Shutdown had the same mid-turn gap

**Symptom:** `closeAll` (daemon shutdown) evicted every cached session with no
mutex awareness — the same hazard triggered by a supervisor restart or `kill`
mid-observation.
**Fix:** `closeAll` takes a real `Lock` per session, bounded by
`closeAllGracePeriod` (5s, the same shape as the metrics server's shutdown),
then force-closes; waits run concurrently so shutdown stays bounded.
**How it was verified:** Tests under `-race` confirm the close waits for a
simulated turn's `Unlock` and that a turn outliving a shrunk grace period is
force-closed; break/restore. Live: a real daemon sent `SIGTERM` mid-observation
exited cleanly with no orphan.

### Shutdown raced a brand-new session's goroutine

**Symptom:** A `process()` goroutine dispatched for a never-seen session just
before `SIGTERM` called `getOrCreate` after `closeAll` took its snapshot; its
later `Insert` hit a closed store and was dropped. The code's own comment had
flagged this as unaddressed.
**Fix:** `processWG` tracks every `process()` from dispatch
(`dispatchProcess`); `Run` waits on it (`waitForProcessDrain`, bounded) before
`closeAll`, matching real claude-mem's stop-accepting → drain → close order.
**How it was verified:** A test dispatches `process()` for an unregistered
session with a blocking fake observer and confirms the drain waits and the
observation persists; break/restore (the wait returned immediately). A live
`SIGTERM` smoke test showed no regression.

### A third concurrent session was never captured

**Symptom:** Three real sessions against one daemon: two captured, the third's
event accepted and then vanished with no log line and `doctor` reporting the
worker reachable. The only evidence was `pool_in_flight: 2 / pool_capacity: 2`
at 0% CPU.
**Cause:** An observer slot is held for a cached session's whole lifetime
(10 minutes idle), so with capacity 2 the third session blocked on an
unbounded `pool.Acquire()` pinning a goroutine and connection.
**Fix:** `AcquireWithin` bounded at 2 minutes; contention logged at `WARN`
naming `-max-concurrent`; an actionable error instead of a hang; `doctor`
reports a saturated pool.
**How it was verified:** Two real sessions against a capacity-1 daemon logged
`WARN waiting for an observer slot: all 1 in use by cached sessions`;
break/restore (the unbounded version hangs the test).

### Concurrent sessions were dropped after the slot deadline

**Symptom:** With the then-default `-max-concurrent 2`, one idle cached
session plus eight arriving at once captured **1 of 8**; the other seven
waited the full two-minute deadline and were dropped.
**Cause:** The wait blocked on a pool release that a cached session never
makes after its turn.
**Fix:** An arriving session evicts the least-recently-used idle cached session
(never one mid-turn) and retries every 250ms. The default later moved to 4
after a three-window soak at 2 evicted and respawned an observer (~1–2s) on
every third tool call (3 evictions for 9 events).
**How it was verified:** The same scenario after: **9/9 in 42s**, 7 evictions,
0 drops. (#3, PR #10; PR #17.)

### A stale daemon applied old rules after an upgrade

**Symptom:** A daemon up ~28 hours across sixteen commits still used
pre-git-root project naming: a session in a subdirectory wrote observations
under `auth` while the new `SessionStart` looked them up under `repo`.
**Cause:** `start` only asked whether a daemon was running, never which build.
**Fix:** The stats file records `version`, `pid`, and `store`; `start`
replaces a daemon on an older build, or one on the built-in store when the
session is configured for another, and warns without acting when two explicit
stores differ. Anything unknown reads as "not stale". (#4, PR #13.)
**How it was verified:** Unit tests for `staleDaemon`/`storeMismatch`; the end-
to-end loop that found it.

### Embedding failed permanently on a transient Ollama error

**Symptom:** The observer call retried once on a transient failure;
`embed.Client.Embed` did not, so a brief Ollama blip left an observation
permanently invisible to semantic search.
**Fix:** One retry after 250ms on a network error or 5xx; no retry on a 4xx or
an empty embedding (the model is not pulled and will not be moments later).
**How it was verified:** Unit tests plus a real Ollama call.

### Log files grew without bound

**Symptom:** `worker.log` gains a line per `PostToolUse` for months.
**Fix:** Every log rotates at 5MB keeping one generation (`name.log.1`).
**How it was verified:** A real `worker.log` grown past the cap rotated to
`worker.log.1` on the restarted daemon's first log line.

### Failures logged at INFO; WARN unused

**Symptom:** 32 sites at ERROR, 64 at INFO, and 2 at WARN; nine lower-case
failure messages (`embedding failed for observations.id=…`, `failed to report
privacy state to worker`) sat at INFO beside routine output.
**Cause:** A previous severity pass reclassified by the `FAILED` prefix only.
**Fix:** Degradations — the observation stored, a secondary capability lost —
go to `WARN`; a recovered panic logs at `ERROR`, not `INFO`.
**How it was verified:** Level-distribution audit; live hook runs.

### Socket path over the OS limit killed the daemon silently

**Symptom:** A Unix socket path over 104 bytes (macOS) made the daemon die
with a bare `bind: invalid argument` at INFO, and `start` said only "did not
become ready".
**Fix:** Both validate the path up front naming the limit; daemon exit logs at
`ERROR`. (PR #10.)

## Hooks

### Stop summarized before the last observation landed

**Symptom:** Two real concurrent sessions through real hooks: one session's
`Stop` found zero observations 4 seconds before the daemon persisted the one
it had, and skipped the summary. In a longer session the summary would
silently miss the last action.
**Cause:** `Stop` fires the instant a session ends; the last `PostToolUse`'s
observer call is still running in the daemon.
**Fix:** Poll until the count stops growing (initially up to ~12s).
**How it was verified:** Re-running the two real sessions.

### Stop's first settle rule misread a one-call session

**Symptom:** Two consecutive zero reads were treated as "confirmed empty", but
a session with exactly one tool call reads zero on every check until the
observer finishes, which routinely exceeded one poll interval.
**Fix:** Only a count that has gone positive and then stops growing counts as
stable; a tool-call-free session pays the full budget.
**How it was verified:** Tests for false stabilization at zero, a perpetually
growing count that must terminate, and an empty session paying the full
budget; the two real sessions re-run twice — the first re-run still failed,
the second summarized both.

### Stop's second settle rule misread a sequential plateau

**Symptom:** Two matching non-zero reads were still wrong: one session's
observations are serialized by a per-session mutex, so the count sat at 1 for
seconds while the second tool call's observation was mid-flight. Reproduced to
the timestamp: the second observation started six seconds after the first and
`Stop` reported "from 1 observations" instead of 2.
**Fix:** A streak of 10 consecutive matching non-zero reads (~9s, above a
single observation's ~5–8s) within a budget raised from 12 to 45 polls.
**How it was verified:** A test reproducing the plateau (a stale count held for
fewer checks than the streak); break/restore with the threshold at 2.

### Stop's ceiling cut off a 104-second observation

**Symptom:** A live re-verification hit a single observation that took 104
seconds, longer than the 45-poll ceiling; the summary would have been cut
mid-flight. The row-count heuristic could never know whether work remained.
**Fix:** The daemon answers `INFLIGHT <session_id>` synchronously from its
`inflightTracker`; `Stop` exits once the daemon confirms nothing is in flight
(2 consecutive checks) and extends the ceiling to 300 polls (five minutes)
only when the daemon confirms real activity. A stable row-count streak no
longer overrides a "still busy" answer.
**How it was verified:** Tests for the override (break/restore: the old
heuristic locked in early despite the worker reporting work); live, a real
two-tool-call session whose `stop.log` reported "from 2 observations" via the
query path.

### Stop never produced a summary under `claude -p`

**Symptom:** A probe plugin whose Stop hook merely slept and wrote a file
produced nothing after `claude -p` exited — not at 25 seconds, not at 1. Two
soak sessions captured observations and no summary; every `-p`-based
verification had silently been missing them.
**Cause:** Claude Code tears hook processes down when the session ends, and
`-p` sessions end when the answer prints. Backgrounded work does outlive it.
**Fix:** `run-hook.sh` reads stdin first (a detached child inheriting the pipe
got `EOF`; `</dev/null` did too) and detaches `stop` with the payload replayed.
Only `stop`; `PostToolUse` exits in milliseconds.
**How it was verified:** The probe, then real sessions producing summaries.

### Stop summaries never embedded

**Symptom:** The hook had no `-embed-model` flag and never called
`SaveEmbedding`, so the most information-dense observation a session produces
was findable only by keyword.
**Cause:** Found by pattern-matching against the identical `add_observation`
gap.
**Fix:** Embed the summary; `-embed-model ""` keeps it persisted but unembedded.
**How it was verified:** Seeded observations, ran `stop` with the real model,
and found the summary via `semantic-search` with zero keyword overlap.

### Stop summarized the start of long sessions and dropped the end

**Symptom:** On a real 150-observation session at the default cap of 50 the
summary described routine early edits, said "an extensive series of 50
sequential edits", and contained no trace of the decision recorded 50 times in
the tail.
**Cause:** `BySessionID` orders oldest-first; a plain `LIMIT` keeps the head.
**Fix:** Re-read up to `summaryFetchCap` (2000) rows and choose a head + tail
window (`observer.SelectSummaryWindow`), telling the prompt the true total.
**How it was verified:** Window tests; the same session re-summarized.

### Stop re-ran on `stop_hook_active` retries

**Symptom:** When another Stop hook blocks a turn, Claude Code retries with
`stop_hook_active=true`; a repeat Stop on an already-summarized session took
**17.3 seconds** and a billed observer call before the content hash rejected
the insert — at the default cap of 8, roughly two and a half minutes and eight
wasted calls per turn.
**Fix:** Return success immediately on `stop_hook_active`, as Claude Code asks;
also skip when a summary row already exists, before the wait and the model
call.
**How it was verified:** `stop_guards_test.go`.

### File-context and Stop leaked into subagents

**Symptom:** `file-context` injected file memory into a Task subagent's `Read`
calls and `stop` could summarize a subagent as if it were the user's session.
Real claude-mem's `file-context.ts` and `summarize.ts` skip on `agentId`.
**Cause:** `claude-agent-sdk-go`'s `HookInput` did not model
`agent_id`/`agent_type`.
**Fix:** `AgentID`/`AgentType` on `HookInput` (SDK v0.1.2); the skip in both
hooks after the project-exclusion check, matching real claude-mem's order.
**How it was verified:** A `--plugin-dir` session: a direct `Read` proceeded;
a `Task` subagent's `Read` (`agent_type=general-purpose`) logged the skip;
break/restore showed the unguarded call falling through to the lookup.

### Prompt-context embedded internal protocol notifications

**Symptom:** Claude Code auto-submits `<task-notification>…</task-notification>`
as a `UserPromptSubmit`; the hook reported it non-private, paid an Ollama
embedding (it is usually past `-min-prompt-len`), and injected "memory
relevant to what you just asked" for plumbing.
**Fix:** `privacy.IsInternalProtocolPayload`, ported from real claude-mem's
`isInternalProtocolPayload`. Its regex needs a backreference and a lookahead
RE2 cannot express, so it is plain string checks: exactly one open tag, a body
with no further occurrence of the tag name, the matching close tag. Checked
right after project exclusion, before privacy.
**How it was verified:** All 12 cases ported from `tag-stripping.test.ts`
(bare, empty, whitespace, multiline, attributed → true; unclosed, surrounded,
unrelated tags, over-256KB, adjacent or text-separated blocks → false). Live:
a real notification never reached a deliberately unreachable socket (no
connection-failure log line); break/restore showed it notifying and embedding.

### Prompt-context injected twice on a duplicate `UserPromptSubmit`

**Symptom:** Claude Code can fire `UserPromptSubmit` twice for one prompt
(real claude-mem issue #2515); each firing paid an embedding and injected its
own block.
**Fix:** `DEDUPE <session_id> <hash>` on the daemon socket; the daemon keeps
`{lastPromptHash, firstSeen}` per session and answers whether the hash was
seen within 10 seconds (`USER_PROMPT_DEDUPE_WINDOW_MS`), measured from the
original prompt so a genuine repeat after the window is new. Checked after
privacy, matching real claude-mem's order.
**How it was verified:** Wire round trip, the exchange through the real client
against a real socket (a later different hash supersedes; the stale hash is no
longer a duplicate), window expiry, stale-entry eviction. Live: the identical
payload twice — the first embedded, the second skipped; break/restore.

### Worktree sessions read and wrote different project names

**Symptom:** The project name was the cwd basename, so a subdirectory or a
git worktree collided or diverged from the repository.
**Fix:** `memory.ProjectContextFor` resolves the git root; a worktree writes
under its own name and `context` reads across the worktree and its parent,
fetching each at the full limit and trimming on merge.
**How it was verified:** `project_test.go` (repo root, generic subdirectory
names, a real worktree, outside git, a non-worktree `.git` file) and
`context_worktree_test.go`.

### The handler audit against real claude-mem

All seven files under real claude-mem's `src/cli/handlers/` (1,061 lines) were
read line-by-line against this port:

| handler | maps to | outcome |
|---|---|---|
| `summarize.ts` | `stop` | gap: `stop_hook_active` guard (fixed above) |
| `file-context.ts` | `file-context` | gaps: file-mtime staleness gate; per-session dedup with specificity scoring (both fixed) |
| `context.ts` | `context` | gap: git-root project resolution and worktree `allProjects` reads (fixed above) |
| `observation.ts` | `hook` | at parity; its missing-`cwd` throw is a transcript-path fallback here |
| `session-init.ts` | `prompt-context` | at parity or ahead (`>= 20` gate, internal-protocol skip, plus dedup and tag stripping) |
| `user-message.ts` | — | Cursor-only banner; out of scope |
| `file-edit.ts` | — | Cursor's `afterFileEdit`; `PostToolUse` `matcher: "*"` covers Edit/Write |

Deliberately unported: `FILE_READ_GATE_MIN_BYTES`, whose value is not defined
anywhere findable. Claude Code 2.1.238's 31 hook events were extracted from
the binary's own `hook_event_name` literals; see
[hooks.md](hooks.md#events-deliberately-not-wired).

### Hooks were verified live against real sessions

- **`context`** — a marker seeded into the database; a `claude -p` session
  with no tools, asked about its injected context, reported it verbatim.
- **`file-context`** — the same marker technique; a session asked to read that
  file reported the injected context.
- **`prompt-context`** — needed a `Prompt` field on the SDK's `HookInput`
  (v0.1.1, confirmed against a captured payload). Two observations seeded
  (Postgres/pgvector choice; log rotation); the session asked "what database
  technology did we choose for scaling similarity search over embeddings?"
  with no keyword overlap answered Postgres/pgvector/HNSW, attributing it to
  injected memory. Run against a throwaway `-db`, not the shared store.
- **`stop`** — run twice against one session; the second recognized the
  duplicate and did nothing.

### `hooks.json` could name a subcommand that did not exist

**Symptom:** Rename a subcommand and the build and tests pass while every hook
dies at runtime; an unknown subcommand prints usage and exits 2, so `context`
would emit usage on stdout where Claude Code expects JSON.
**Fix:** `TestHooksJSONInvokesRealSubcommands` reads `main.go`'s dispatch
switch; it refuses to run if the extraction finds implausibly few entries.

## Privacy

### No `<private>` tag redaction anywhere

**Symptom:** `grep -rn "private" --include=*.go` (excluding tests) returned
zero matches. A `<private>` block in tool output was captured, sent to the
observer, summarized, embedded, persisted, and served to any MCP caller.
**Fix:** The `privacy` package (`StripTags`/`StripMemoryTags`) with real
claude-mem's six tag names; one compiled pattern per tag because RE2 has no
backreferences — identical results for same-tag nesting
(`<private>a<private>b</private>c</private>` leaves `c</private>` either way).
Applied in the daemon before `transcript.Truncate` (a tag straddling the
1500-byte cut would be unclosed), and in `prompt-context`, which skips
embedding for a wholly private prompt. `stop` needs nothing: its input is
already-redacted rows.
**How it was verified:** Every case ported from `tests/utils/tag-stripping.test.ts`
(single/multiple/interleaved, multiline, ReDoS volume — 150 tags and a
10,000-character tag under one second — JSON-embedded, both
`system_instruction` spellings, a nested `claude-mem-context` inside an
injected CLAUDE.md dump). Live with break/restore on both sites: the observer
received neither markup nor secret; a wholly private prompt made no embedding
call while a partially private one embedded its remainder.

### A private prompt did not suppress its tool calls or summary

**Symptom:** Real claude-mem's `PrivacyCheckValidator` suppresses the whole
turn once its prompt strips to nothing; here only the prompt's embedding was
gated, so every tool call in the turn was observed and summarized.
**Fix:** `PRIVATE <session_id> <0|1>` and `ISPRIVATE <session_id>` on the
daemon socket. `prompt-context` sends the marker on **every** prompt, because a
sticky flag reproduces real claude-mem's issues #2794/#2795 (a session frozen
forever). `process()` and `stop` check it alongside project exclusion; an
absent flag or unreachable daemon means "not private". The hook is not
`async`, so ordering is enforced by the hook chain rather than a persisted row.
**How it was verified:** Wire round trips; the setter/query exchange over a
real socket (a later `false` supersedes `true`; no marker reads not private);
`process()` skipping before touching a nil session cache; stale-entry
eviction. Live: a private prompt followed by real `PostToolUse` and `Stop`
left zero rows while a control session persisted an observation and a
summary; break/restore (nil-pointer panic in the regression test).

### No way to exclude a project

**Symptom:** Real claude-mem's `CLAUDE_MEM_EXCLUDED_PROJECTS` had no
equivalent; the daemon and every recall hook tracked unconditionally, so
keeping a confidential project out meant not installing the plugin.
**Fix:** The `excludeproject` package and `-excluded-projects` on `worker`
(forwarded from `start`) and each of `context`/`file-context`/`prompt-context`/`stop`,
matching real claude-mem's per-handler checks; matched against path and
basename before any work, including before spawning an observer.
**How it was verified:** Real claude-mem's `isProjectExcluded`/`globToRegex`
run through Node on the same 19 cases gave byte-identical results, including
an empty path against `*` — `shouldTrackProject`'s `!cwd` guard, folded into
`IsExcluded`. Live: two projects on one daemon with identical `Read` calls; the
excluded one produced zero rows and no observer call.

## Search and MCP tools

### Unscoped search leaked across projects

**Symptom:** A Postgres test flaked because rows from unrelated projects
crowded a fixed `LIMIT`; the store is one database for every project on the
machine, so an unscoped search is a leak.
**Fix:** Scoping at the `memory.Backend` level for `Search`, `SemanticSearch`,
`ByIDs` (otherwise ids could be guessed), `Timeline` (scoped to the anchor's
project, not the caller's argument), and later `session_observations`; MCP
tools derive the project from the server's cwd, with `all_projects: true` or
`project: "..."` as the explicit way out.
**How it was verified:** A regression test seeding two projects with adjacent
ids and confirming `timeline` never crosses; live MCP calls.

### Paging needed a deterministic order

**Symptom:** `-offset` was originally skipped on a rationale that never
applied; once added, rank ties (identical term-frequency shape) could return a
row twice or skip one across pages.
**Fix:** Order by rank then `id` on both backends.
**How it was verified:** 5 matching rows seeded; pages of 2/2/1 are disjoint
and cover every row once, on both backends.

### Postgres placeholder numbering was hardcoded

**Symptom:** The project clause assumed `$3`, correct only while there was a
single optional filter.
**Fix:** Placeholders numbered dynamically as filters are appended.
**How it was verified:** A live-container test applying `project` and `type`
together.

### `search` lacked date range and sort order

**Symptom:** The README excused the missing `dateStart`/`dateEnd`/`orderBy`
with a claim that they needed a schema redesign; real claude-mem's
`SessionSearch.ts` uses plain `WHERE`/`ORDER BY` on `created_at_epoch`, which
this schema already indexes.
**Fix:** `dateStartMs`/`dateEndMs` (0 = unbounded) and `orderBy` —
`relevance` (default), `date_desc`, `date_asc`, anything else falling back to
`date_desc` as `buildOrderClause` does — on both backends; `store.ParseDateArg`
accepts RFC3339, `YYYY-MM-DD` (UTC midnight), or epoch milliseconds, shared by
the CLI and the MCP tool.
**How it was verified:** Rows with directly-set epochs on both backends;
`dateStart`/`dateEnd` alone and together; both orders and the fallback;
break/restore on the date clause. Live: a backdated row excluded by
`dateStart` and ordered first by `date_asc` through `mcp`.

### `search` could not filter on more than one type

**Symptom:** Real claude-mem's `obs_type` is comma-separated; this port matched
one type with `type = ?`.
**Fix:** `store.SplitCommaList`; one type stays an equality, more than one
becomes `type IN (...)`, slotting into the dynamic placeholder scheme.
**How it was verified:** Three types seeded; `"discovery,decision"` and
`"discovery, decision"` return exactly the union; break/restore. Live through
`search_observations`.

### `get_observations` failed on large id lists

**Symptom:** 100,000 ids failed with a raw "too many SQL variables" from the
SQLite driver (the hand-built `IN (?,?,...)` list had no cap).
**Fix:** `MaxIDsPerLookup` (100) in both backends — Postgres's `= ANY($1)`
does not hit the limit but gets the same bound for parity; unknown ids are
omitted, not an error.
**How it was verified:** The MCP boundary returns a clean `isError` naming the
limit; a live session fetched a seeded observation's full record by id.

### `timeline` crossed project boundaries and accepted negative depth

**Symptom:** `Timeline(..., -1, -1)` returned every row before the anchor on
SQLite (negative `LIMIT` is unlimited) and `LIMIT must not be negative` on
Postgres. The MCP caller substituted defaults, so it was unreachable live but
wrong as a public method.
**Fix:** Clamp to `[0, MaxTimelineDepth]` (100) in the store; order by `id`
(monotonic with insertion on both `rowid` and `BIGSERIAL`, confirmed).
**How it was verified:** Regression tests on both backends; live direct-anchor
and query-resolution calls returned exact neighbours with the anchor marked.

### `timeline` defaulted to depth 3 and ignored a caller mistake

**Symptom:** Real claude-mem's `SearchManager.timeline` defaults to 10 (its
own schema text says 3 — a doc/behaviour mismatch in claude-mem itself); this
port returned under a third of the context. Providing both `anchor` and
`query` silently preferred `anchor` where real claude-mem errors.
**Fix:** Default 10; both arguments is a tool error.
**How it was verified:** 21 seeded observations; an anchor-only call returns
all 21 (10 + anchor + 10), not 7; both arguments returns `isError`;
break/restore on each. Live through `mcp`.

### `add_observation` was invisible to semantic search

**Symptom:** The only write tool persisted but never embedded, so
`search_observations` found a manual observation and
`semantic_search_observations` did not.
**Fix:** Embed like automatic capture; idempotent via
`ContentHash(SessionID, "manual", title, narrative)` with a per-process
session id.
**How it was verified:** An Ollama-backed test (skips without Ollama); a live
plugin install found a manual observation by meaning.

### `add_observation` had no size bounds

**Symptom:** Every other input was bounded (`MaxPayloadBytes`,
`MaxIDsPerLookup`, `maxLimit`), but title/subtitle/narrative/facts/concepts
went from the wire into a row and an embedding request unchecked.
**Fix:** Title 500, subtitle 1000, narrative 10000 bytes; facts and concepts
50 items each, 1000 and 200 bytes per item; an oversized argument is an
`isError` naming the limit.
**How it was verified:** Real `tools/call` requests with a 501-byte title, a
10001-byte narrative, 51 facts, and a 1001-byte fact all rejected;
break/restore accepted every one; normal-sized fields still succeed.

### `recent_observations` did not match what SessionStart injects

**Symptom:** Its description claimed SessionStart's read path, but it returned
`[id] title (project, tool)` lines defaulting to 10, while `context` injects
prose with no ids defaulting to 5.
**Fix:** `session_start_context(limit?, project?)` over `RecentByProject`,
defaulting to 5, with `formatSessionStartContext` duplicating `formatContext`
byte for byte (a `package main` import would cycle).
**How it was verified:** Byte parity against a seeded observation (break/restore
on the header text); 7 seeded, exactly 5 returned; scoping and empty-project
tests. Live: the real `context` hook's `additionalContext` diffed against the
tool's output — identical.

### `observation_context` and `important_workflow`

**Symptom:** `prompt-context`'s semantic recall was the last hook-only read
with no on-demand equivalent; real claude-mem also ships a static
`important_workflow` teaching `search_observations` → `timeline` →
`get_observations`.
**Fix:** Both tools; `observation_context` returns the hook's exact injected
block (a duplicated formatter), `important_workflow` is registered first.
**How it was verified:** Byte-for-byte against a real embedded observation;
`add_observation` then `observation_context` end to end on the live Postgres
container through the compiled binary.

### MCP `serverInfo.version` was three releases stale

**Symptom:** A hardcoded `"0.1.0"` in the `initialize` response.
**Fix:** Derived from VCS build info like `version`/`doctor`.
**How it was verified:** The live `initialize` response matched
`git rev-parse HEAD`.

## Store: SQLite

### Concurrent writers hit "database is locked"; cascades never fired

**Symptom:** The daemon and every CLI command open their own connection to
one file; in rollback-journal mode a second writer failed immediately.
Separately, foreign keys are off by default, so `observation_vectors`'
`ON DELETE CASCADE` had never fired and `prune` left orphaned embeddings.
**Fix:** `_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on` as DSN
parameters, so every pooled connection gets them (a one-time `PRAGMA` reaches
only one).
**How it was verified:** `doctor` reports the settings in effect:
`journal_mode=wal foreign_keys=1 busy_timeout_ms=5000`.

### The FTS5 delete trigger was invalid

**Symptom:** The first real `DELETE` (when `prune` arrived) failed with a SQL
error through both the Go driver and the `sqlite3` CLI.
**Cause:** `observations_ad` used the fts5 "special command" delete syntax,
valid only for contentless/external-content tables.
**Fix:** SQLite migration 5 rewrites the trigger, so existing databases are
repaired, not just new ones.
**How it was verified:** A simulated already-migrated database with the broken
trigger is fixed on reopen and `prune` works; on Postgres, the generated
`search_vector` and `embedding` are confirmed gone after a prune.

### A corrupt backup imported "successfully"

**Symptom:** Postgres parses `CreatedAt` as RFC3339 (its column is
`TIMESTAMPTZ`); SQLite stored anything. A three-row backup with `not-a-date`
and an empty date reported `Imported 3 observation(s)`, and the later
SQLite→Postgres migration died on row 1 — half-restored, since `cmdImport` has
no transaction.
**Fix:** `store.ParseExportCreatedAt`, shared by both `ImportRow`s; SQLite
keeps the original string so well-formed rows round-trip byte-identically.
**How it was verified:** The bad backup now fails on row 1 with the same
message and writes zero rows; a good file imports, stores `created_at`
byte for byte, and re-imports as a no-op; six table-driven cases (malformed,
empty, impossible date, space-for-T, two valid forms) on both backends;
break/restore. Five other candidate divergences — negative `cost_usd`,
negative `created_at_epoch`, empty `session_id`/`project`/`content_hash`,
duplicate `content_hash` in one file, invalid `type` — were checked and match.

## Store: Postgres

### `CLAUDE_MEM_DB` did not exist, so an installed plugin could not reach Postgres

**Symptom:** `doctor` with no flag reported `~/.claude-mem-go/observations.db`;
`doctor -db postgres://…` reported Postgres. Every hook and MCP call took the
first branch, so an operator could stand up Postgres and have all memory go to
SQLite with no error.
**Cause:** `hooks.json` and `.mcp.json` pass no flags.
**Fix:** `CLAUDE_MEM_DB`, precedence `-db` > env > built-in; `doctor` names
which won.

### DSN passwords reached logs, and the first redactor leaked on malformed input

**Symptom:** `doctor`, hook logs, and `postgres.Open`'s error printed the DSN
in plaintext. The first `RedactDSN` used `net/url.Parse` and returned the
*unredacted* original when parsing failed — triggered by a well-formed
`user:password@` followed by a broken host.
**Fix:** A regex needing only the `scheme://user:password@` prefix.
**How it was verified:** The exact malformed DSN, and a live-container auth
failure.

### `hnsw.ef_search` was not range-checked by Postgres

**Symptom:** Identical code (`BEGIN; SET LOCAL hnsw.ef_search = 1001; COMMIT`)
errored on a connection that had touched the vector extension and silently
succeeded on a fresh one whose first query was that `SET LOCAL` — the GUC is
an unchecked placeholder until then.
**Fix:** Validate 1–1000 in Go at `Open`; apply the override with a
transaction-scoped `SET LOCAL`, since `database/sql` gives no control over
which pooled connection a call gets.
**How it was verified:** Out-of-range rejection, and no leakage past the call
on a pool forced to one connection; `SHOW hnsw.ef_search` confirms the value
takes effect on a pooled connection.

### No statement timeout

**Symptom:** Zero hits for `statement_timeout` in the repo; a hung query (lock
contention from `prune`/`reembed`, a pathological plan, a network stall) held
a connection forever, and a handful exhausted the 10-connection pool shared by
every hook and the daemon.
**Fix:** `statement_timeout` appended to the DSN (URL or keyword/value form —
`url.Parse` mangled the latter into `host=localhost%20user=x...`), default
30000ms via `CLAUDE_MEM_POSTGRES_STATEMENT_TIMEOUT_MS`; an explicit value in
the DSN wins; an unparseable URL passes through.
**How it was verified:** `statement_timeout=2000` against `SELECT pg_sleep(5)`
errored at ~2s with "canceling statement due to statement timeout"; a short
env value against `pg_sleep(10)` errored at ~1.5s; a `SELECT 1` afterward
succeeded immediately, proving the pool recovered; break/restore (the query ran
the full 10s).

### No per-attempt timeout on the initial ping

**Symptom:** `pingWithRetry`'s "~7.75s worst case" held only for fast
failures; every caller passes an undeadlined context, so a host that accepts
TCP and never answers could hang each of the 6 attempts for the OS's TCP
timeout.
**Fix:** `context.WithTimeout(ctx, connectionTimeout())` per attempt, default
5000ms via `CLAUDE_MEM_POSTGRES_CONNECTION_TIMEOUT_MS` (real claude-mem's
knob), composing with any tighter caller deadline.
**How it was verified:** A TCP listener that accepts and goes silent;
`pingWithRetry` returns in well under a second; break/restore (the test hung
past its own timeout).

### Pool size and idle timeout were hardcoded, and the idle timeout was 10x off

**Symptom:** `SetMaxOpenConns(10)` with no override, and
`SetConnMaxIdleTime(5 * time.Minute)` against real claude-mem's 30-second
default.
**Fix:** `CLAUDE_MEM_POSTGRES_POOL_MAX` (10) and
`CLAUDE_MEM_POSTGRES_IDLE_TIMEOUT_MS` (30000), same names as real claude-mem.
`SetMaxIdleConns` stays literal (no `pg.Pool` equivalent); `sslmode` needs no
port because pgx honours libpq conventions natively.
**How it was verified:** `CLAUDE_MEM_POSTGRES_POOL_MAX=3` yields
`MaxOpenConnections == 3` on the live container; break/restore (still 10).

### Open failed permanently if Postgres was still starting

**Symptom:** A daemon or hook starting a beat before the container's
healthcheck hard-failed on the first ping.
**Fix:** Retry on a backoff of 0/250ms/500ms/1s/2s/4s.
**How it was verified:** Container stopped, `Open` called, container restarted
~1.5s into the window; `Open` succeeded and measurably took over a second.

### Keyword search ignored `facts` and `concepts`

**Symptom:** SQLite's FTS5 covers five columns; Postgres's `search_vector`
covered three. On the 6,210-row dev container, **536 of 567 fact strings and
200 of 289 concept tags** could not be found by their own text. A byte-identical
row: SQLite `search "multi-agent system"` → found; Postgres → no matches
("system" appeared only in `concepts`, and `plainto_tsquery` ANDs terms).
**Fix:** Migration 3 drops and re-adds `search_vector` with `facts` and
`concepts` at weight `D` (`ADD COLUMN` backfills a generated column), mirrored
in `schemaSQL`.
**How it was verified:** `jsonb::text` is immutable enough for `STORED`, and
`'["multi-agent system"]'` tokenizes to `'agent':3 'multi':2 'multi-ag':1
'system':4`. The migration ran in about a second; `facts_unsearchable` 536 → 0
and `concepts_unsearchable` 200 → 0; `EXPLAIN` still shows
`Bitmap Index Scan on idx_observations_search_vector`; tests for facts-only,
concepts-only, and `D`-weight ranking, mirrored on SQLite; break/restore by
rebuilding the three-column column.

### Boolean operators worked on SQLite and broke on Postgres

**Symptom:** On three identical rows: `alpha OR beta` → SQLite
`[only-alpha only-beta both]`, Postgres `[both]`; `alpha NOT beta` → SQLite
`[only-alpha]`, Postgres `[both]` (exactly the excluded row). `OR`/`NOT` were
English stopwords to `plainto_tsquery`.
**Fix:** `websearch_to_tsquery` (what real claude-mem uses, and the only parser
Postgres documents as never raising on user input — confirmed with unbalanced
quotes, stray `&`/`|`/`!`, a bare `NOT`), with `NOT term` rewritten to `-term`
(a bare uppercase `NOT` is silently ANDed) and every non-operator token quoted
(lowercase `or`/`not` are operators to websearch but words to FTS5).
Stopword stripping remains a documented difference.
**How it was verified:** All five queries identical on both backends with the
compiled binary; hyphen, `key:value`, and `(parens)` still match;
break/restore with `plainto_tsquery` fails the OR and NOT subtests.

### The project filter made semantic search return zero rows

**Symptom:** 60,000 embedded rows in the queried project, 20,000 nearer rows
in another: `project="target"` → 0 rows; `"noise"` → 10; unscoped → 10.
`EXPLAIN`: `Index Scan using idx_observations_embedding_hnsw … Rows Removed by
Filter: 40 … actual rows=0`. Planner-dependent — correct at 4,000 rows, broken
at 60,000 — and `-hnsw-ef-search` does not rescue it.
**Cause:** The project predicate was a post-filter on the HNSW scan.
**Fix:** `hnsw.iterative_scan = strict_order` (~26ms on the reproduction);
a `MATERIALIZED` CTE pre-filter (2.7s) as the fallback for pgvector < 0.8,
detected once at `Open`, because an unsupported `hnsw.*` GUC errors on a
warmed connection.
**How it was verified:** The regression test asserts the routing decision, not
a row count (a row count passes at test scale regardless); break/restore; a
separate test exercises the CTE path.

### Hard-wired to 768-dimension embeddings

**Symptom:** All 14 `postgres.Open` call sites passed `embedDims = 0` → 768,
while `-embed-model` was documented on nine commands; `all-minilm` (384) or
`mxbai-embed-large` (1024) could not be used. `import` aborted half-populated;
the daemon logged and counted forever with zero embeddings. `reembed.go`
claimed a mismatch "can't actually occur" — false: `ObservationsNeedingEmbedding`
returned every row and `SaveEmbedding` rejected each.
**Fix:** `CLAUDE_MEM_POSTGRES_EMBED_DIMS` sizes a *new* column; `Open` reads
the real width from the catalog; `SaveEmbedding` names both numbers and the
remedy; `doctor` reports `embedding_column_dims`. Also fixed: `SaveEmbedding`
for a nonexistent observation returned `nil` on Postgres (zero-row `UPDATE`)
where SQLite raised a foreign-key violation — now a `RowsAffected` check.
**How it was verified:** A scratch store with `EMBED_DIMS=384` has
`vector(384)`, accepts 384 and rejects 768 with the actionable message;
`doctor` reports 384 there and 768 on the dev store; break/restore shows the
bare pgvector error. A cross-backend diff of `Timeline`, `ByIDs`,
`ObservationsForFile`, `CountByProject`, `RecentByProject`, `BySessionID`, and
`Prune` (ordering, tiebreaks, anchor rejection, depth/limit clamps at
`0`/`-1`/`-5`/`1000`, the `<` cutoff, no orphaned vectors) measured identical.

### `observations.type` was never validated

**Symptom:** Neither schema nor code checked `type` against the vocabulary
(`discovery`/`change`/`decision` from the observer, `summary` from Stop,
`manual` from `add_observation`); a drifting LLM tag or hand-edited import
would persist a row invisible to every type filter.
**Fix:** `store.ValidateObservationType` in the shared `insertRow` of both
backends; Postgres migration 2 adds a `CHECK` constraint inside a `DO` block
checking `pg_constraint` (no `ADD CONSTRAINT IF NOT EXISTS`).
**How it was verified:** 3000+ real rows in the dev container all fell inside
the vocabulary (the break/restore step briefly left two invalid rows, cleaned
up). Tests on both backends with break/restore; the constraint dropped by
hand, a raw bad `INSERT` succeeding, then restored and the same `INSERT`
failing with a constraint violation.

### Scale problems at 250,000 rows

A 250,000-row, 768-dimension corpus built through the real schema and queried
through the real Go code (~1.1GB with indexes, HNSW 531MB) surfaced three
defects, each now fixed; latencies are in [postgres.md](postgres.md#measurements).

- **`ObservationsForFile` had no usable index.** 47.7ms with `Rows Removed by
  Filter: 4687` — a post-filter over the project. Migration 4's GIN indexes:
  12.9ms, scaling with matches.
- **`maintenance_work_mem` far too small.** At 64MB: "hnsw graph no longer
  fits into maintenance_work_mem after 16759 tuples". 512MB moves the spill
  to 141,896 tuples and cuts the build from 73s to 40s.
- **Docker's 64MB `/dev/shm`.** Raising the above made the build die with
  `could not resize shared memory segment ... No space left on device`;
  `shm_size: 1gb`.

`EXPLAIN` confirmed the HNSW index is chosen at this size
(`Index Scan using idx_observations_embedding_hnsw`). Unscoped keyword search
at 70.9ms ranked ~50,000 matches by `ts_rank_cd`; the scoped path hooks use
was 12.3ms.

### A 95% recall figure was wrong and retracted

**Symptom:** The harness reported 95.0% recall@10 at 3,000 rows; enlarging the
corpus produced 84 → **100** → 81 → 83 → 85 across rising `ef_search`, a
physical impossibility.
**Cause:** The generator drew from 15 × 8 × 12 = 1,440 distinct sentences, so
vectors were mostly duplicates (1,252 distinct in 3,000), and recall was
counted as id overlap — measuring tie-breaking between two plans.
**Fix:** Unique `file:line` suffixes; recall counts ANN results at least as
close as the exact k-th distance; `measure` refuses a corpus under 99%
distinct and fails if recall drops as `ef_search` rises.
**How it was verified:** 20,000 distinct real embeddings: 80.0% at 40, 94.0%
at 200, 98.0% at 400, monotonic; the `SHOW hnsw.ef_search` check from the
original run had been correct all along. Details in
[bench/recall/README.md](../bench/recall/README.md).

## Operations

### `doctor` never checked the plugin was installed

**Symptom:** "All critical checks passed" over an install with no hooks wired
and a permanently empty store.
**Fix:** `plugincheck` reads `~/.claude/plugins/installed_plugins.json`,
matching the plugin-name half of `<name>@<marketplace>`; a missing manifest
means "not installed". Reported prominently but not critical, since
`--plugin-dir`, the CLI, and MCP work without an install.
**How it was verified:** Real state both ways — not installed, then installed
(`scope=local version=0.3.0`, key `claude-mem-go@claude-mem-go-local`), then
uninstalled with plugin state byte-identical (matching MD5s). Tests for
marketplace-independent matching, multi-scope, absent/corrupt manifests, and
`TestPluginNameMatchesManifest` against `.claude-plugin/plugin.json`;
break/restore on each.

### "Installed" did not mean "able to run"

**Symptom:** Removing the binary from the real install path left a plugin
Claude Code considered installed whose hooks could not run; `doctor` vouched
for it. `plugincheck.Install.InstallPath` was parsed and unused.
**Fix:** `plugincheck.BinaryStatus` requires an executable regular file and
**executes** `version` under a timeout; critical only when the plugin is
installed; a rebuilt-but-not-reinstalled tree is an informational skew note.
**How it was verified:** Working binary → `↳ binary OK`, exit 0; deleted →
`✘ plugin binary unusable`, exit 1; `chmod -x` → `is not executable (mode
-rw-r--r--)`, exit 1; restored → 0. Tests for a real build, missing,
non-executable, corrupt-but-executable, a directory, an empty path;
break/restore to stat-only failed the real-build and corrupt cases.

### `doctor` reported a dead daemon's stats as live

**Symptom:** "worker daemon not running", then from the leftover stats file its
pool saturation, build version, and a **critical** store mismatch, ending in
"Critical checks failed" over a process that did not exist.
**Fix:** Live-state findings gated on reachability; the activity line survives
as "last worker activity before it stopped".
**How it was verified:** A live daemon on another store fires all three
(exit 1); killed with the file left behind, all go quiet (exit 0); a test pins
that stale numbers are not presented as current.

### `doctor` could not see dimension drift or a missing index

**Symptom:** Switching embedding models made SQLite's cosine similarity return
-1 on every length mismatch, so old rows silently never matched; a dropped
HNSW index would degrade every Postgres search to a scan unnoticed.
**Fix:** `Backend.HealthDetails()` — SQLite's effective PRAGMAs; Postgres pool
utilization, `vector_extension`, `hnsw_index_exists`, `hnsw_ef_search`
(`-hnsw-ef-search` on `doctor` so it reflects the configured value);
`embedding_dims` histogram with `embedding_dims_consistent`; a live probe of
the configured model so a store embedded entirely under a replaced model is
caught too.
**How it was verified:** A mixed store showed `embedding_dims=384:1,768:1
embedding_dims_consistent=false`; Postgres rejected a wrong-dimension vector
outright; `vector_extension=0.8.6`, `hnsw_index_exists=true`;
`doctor -hnsw-ef-search 333` → `hnsw_ef_search=333`, unset → `200 (default)`.

### The read path had no instrumentation

**Symptom:** Every metric described writes; an empty recall raises no error
and looks like "nothing relevant" — the shape of two shipped bugs (the
cross-project leak; the zero-row post-filter).
**Fix:** The three hook read paths report their counts to the daemon; exposed
per source (`recall_prompt`, `recall_session`, `recall_file`) as Prometheus
counters, in the stats file, and in `doctor`, which judges only the session and
prompt paths (an empty file lookup is ordinary) and only after 10 recalls.
**How it was verified:** End to end found two defects unit tests missed:
`recordStats()` was only called by the capture path (counters reached
`/metrics` but never the stats file), and `handleConn` ran without panic
recovery.

### `prune` deleted nothing, ever

**Symptom:** `created_at_epoch` is milliseconds (`UnixMilli`, both backends);
`cmdPrune`'s cutoff was seconds — a ~1000x mismatch making
`created_at_epoch < cutoff` false for every row.
**Cause:** Every `Prune` test backdated rows by hand with unit-agnostic
numbers.
**Fix:** The cutoff in milliseconds; `TestPruneCutoffUnitsMatchInsertsRealTimestamp`
inserts through the real `Insert` path on both backends.
**How it was verified:** A real backdated row through the actual binary.

### Export dropped embeddings

**Symptom:** `ExportRow` omitted the embedding, so migrating to Postgres for
ANN search would arrive with nothing to search.
**Fix:** Carry the embedding.
**How it was verified:** 38 of 46 dev observations had embeddings; all 38
survived a round trip into a fresh SQLite file and into the live container.
Earlier, a 32-observation store exported, imported into Postgres, searched,
and re-imported with all 32 skipped as present.

### `reembed` and the stats file

**Symptom:** `doctor` could detect a stale embedding but nothing could fix it
short of re-ingesting; the daemon's only introspection was log lines.
**Fix:** `reembed` (rows with no embedding or a dimension mismatching a live
probe; dry run until `-yes`); `worker-stats.json` after every event, plus
`-metrics-addr` for Prometheus text.
**How it was verified:** One stale 384-dim and one unembedded row: `doctor`
flagged both, `reembed -yes` fixed both, `semantic-search` found the stale one
ranked correctly. A real event over the socket updated the stats file and
`doctor`; `curl /metrics` showed `claude_mem_go_worker_processed_total` go
0 → 1.

### `CLAUDE_MEM_LOG_LEVEL` worked but was undiscoverable

**Symptom:** Added, wired everywhere, recorded only in the changelog.
**Fix:** Documented in the README; `TestEveryEnvVarIsDocumented` fails for any
`CLAUDE_MEM*` name the source reads that the README lacks. One-directional
(code → README): the reverse was checked by hand first and flagged
`CLAUDE_MEM_EXCLUDED_PROJECTS`, which appears legitimately as real
claude-mem's variable.

### The usage message advertised 3 of 18 subcommands

**Symptom:** `worker|hook|ingest`, omitting `doctor` and `stats` — seen at the
exact moment a user mistyped something.
**Fix:** Usage grouped by intent; a test reads the dispatch switch and fails if
a subcommand is missing from it, refusing to run on implausibly few entries.

## Install and skills

### A git-installed plugin shipped no binary

**Symptom:** The binary is gitignored, so a git-source install had every hook
fail with `No such file or directory`, swallowed by Claude Code; `doctor` is
a subcommand of the missing binary.
**Fix:** The `Setup` hook builds it (~33s cold, ~0.7s warm, 300s timeout);
`run-hook.sh` announces a missing binary via `context`'s `additionalContext`
and `missing-binary.log`, because `Setup` was observed not firing under
`claude -p` or during `claude plugin install`. Fetching a release asset is
deliberately unwired. See [plugin-install.md](plugin-install.md#the-self-healing-binary).
**How it was verified:** `git archive HEAD` unpacked and run; the session said
"To fix claude-mem-go, run this and restart Claude Code: cd … && go build …";
after healing, a real observation landed through the wrapper.

### Self-heal could not repair a broken binary

**Symptom:** Go 1.26+ refuses `go build -o X` when X exists and is not an
object file, so `ensure-binary.sh` failed on the corrupt-binary case and
exited 0; `run-hook.sh` only checked `-x`, so a present-but-broken binary took
the healthy path and no diagnostic fired.
**Fix:** Build to a temp name and rename; probe `version`. CI runs Go 1.25 and
stable because the first bug was invisible on one pinned version. (PR #9.)

### Skills went stale

**Symptom:** `mem-search` said "eight tools" and omitted `observation_context`,
`important_workflow`, and `offset`; `mem-doctor` never mentioned
`-hnsw-ef-search`, and later described none of five new `doctor` checks
(three with zero mentions, including the store-mismatch finding that looks like
data loss and is not); `mem-timeline` enumerated another project with
`all_projects` but omitted it from `get_observations`, so the report was built
from titles alone.
**Fix:** Updated; two guards — every critical `✘` finding in `doctor.go` must
appear in `mem-doctor`, and every tool call in every skill must match the
server's tool table (30 calls across 6 skills were clean when first audited).
Both refuse to run on implausibly few extractions.
**How it was verified:** `--plugin-dir` sessions listing every tool name the
skill mentions (all ten came back) and every `doctor` check (`hnsw_ef_search`
among them); `/mem-search claude-mem installation`, `/mem-doctor`, and ten
`mem-timeline` `tools/call`s confirmed in the MCP server's log;
"clean up memories older than 1 day" ran the dry run and asked before `-yes`;
break/restore on both guards.

## Build, tests, and migrations

### `migrate.Run` did not sort and did not reject duplicates

**Symptom:** It documented "ascending Version order" but never sorted — a
slice listing 2 before 1 applied 2 first; two migrations sharing a version
silently skipped the second forever.
**Fix:** Sort explicitly; reject duplicate versions.
**How it was verified:** Both backends, including the live container in a
throwaway schema; a pre-framework SQLite file (no `content_hash`, no
`schema_migrations`) migrates forward on reopen; Postgres records a migration
once and does not re-apply.

### Concurrent first-open migrations raced

**Symptom:** `start` (via the daemon) and `context` both migrate a brand-new
SQLite file on a project's first session. Three different errors by timing:
`database is locked` (`busy_timeout` does not cover a losing DDL statement),
`UNIQUE constraint failed: schema_migrations.version`, and `duplicate column
name` (a check-then-act window in `ensureContentHashColumn`).
**Fix:** Retry `Run`'s whole check-and-apply on any failure; every migration is
idempotent and the "applied?" read is fresh each attempt.
**How it was verified:** A test widening the TOCTOU window failed 3/3 without
the fix and passed 8/8 with it, including under `-race`; the original
integration scenario re-run five more times with zero failures.

### Tests wrote into the real store

**Symptom:** `postgres_test.go` fell back to the README's own DSN, so `go test
./...` wrote real rows: **7,217 of 7,275 observations (99.2%)** in the dev store
were test debris across 3,968 `test-*` projects, 58 rows real; a `backend`
dispatch test left 2 rows under `backend-dispatch-test`.
**Fix:** `CLAUDE_MEM_GO_TEST_POSTGRES_DSN` is mandatory; the dispatch test uses
a closed port; near-miss variable names are reported.
**How it was verified:** Reinstating the fallback wrote 118 rows silently;
without it the run skips 52 tests, fails none, and leaves the row count
unchanged.

### Release binaries were stamped `-dirty`

**Symptom:** v0.4.0 assets reported `<commit>-dirty` from `version`.
**Cause:** goreleaser's untracked `dist/` counted as a modification for Go's
VCS stamp.
**Fix:** `dist/` ignored; the `before` hook runs `go mod tidy -diff` (verify,
never modify). (0.4.1.)
**How it was verified:** A snapshot build (`goreleaser release --snapshot
--clean --skip=publish`) compiled all four targets; the darwin/arm64 binary ran
`version` with a correct stamp and `doctor` against a temp database.

### CI history

Every run before the migration-framework commit failed at `startup_failure`
from an account-level billing lock unrelated to the repo; `act` and
`actionlint` were the verification at the time. The lock is resolved and
hosted runners are the source of truth (checked via `gh run list`). The
plugin-validate step's `command -v claude || echo skipping` escape hatch was
removed after confirming the install passes on a stock `node:20-slim`.
