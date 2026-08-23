# Hooks and the worker daemon

How each Claude Code hook event is handled, what the worker daemon does, and
the controls that shape what gets captured. The wiring is in
`hooks/hooks.json`; every hook runs through `scripts/run-hook.sh`, which
`exec`s the plugin binary (see [plugin-install.md](plugin-install.md)).
Installing the plugin is what makes these fire; nothing here runs otherwise.

## Contents

- [Hook reference](#hook-reference)
  - [SessionStart](#sessionstart)
  - [UserPromptSubmit](#userpromptsubmit)
  - [PreToolUse](#pretooluse)
  - [PostToolUse](#posttooluse)
  - [Stop](#stop)
  - [Setup](#setup)
  - [Events deliberately not wired](#events-deliberately-not-wired)
- [The worker daemon](#the-worker-daemon)
- [Privacy](#privacy)
  - [`<private>` tags](#private-tags)
  - [Excluded projects](#excluded-projects)
  - [Persisting prompts (opt-in)](#persisting-prompts-opt-in)
- [Why the worker/hook split](#why-the-workerhook-split)
- [Running the worker as a supervised service](#running-the-worker-as-a-supervised-service)

## Hook reference

Every hook shares these properties:

- **Budget.** Each recall hook bounds its own work a little under the
  `timeout` in `hooks.json`, so a stuck store query yields a clean, empty
  reply rather than Claude Code killing the process mid-write
  (`cmd/claude-mem-go/ctx.go`; `TestHookBudgetsFitInsideHooksJSONTimeouts`
  pins them against the manifest).
- **Failure behaviour.** A hook never surfaces a failure to Claude Code. It
  logs to its own file in `~/.claude-mem-go/` and exits 0; a missing daemon,
  unreachable store, or unreachable Ollama degrades to "no memory this turn".
- **Store selection.** `-db` > `$CLAUDE_MEM_DB` > `~/.claude-mem-go/observations.db`.
  Hooks receive no flags from the plugin, so `CLAUDE_MEM_DB` is the only way
  to point them at another store.
- **Project resolution.** A project is the git repository root's basename,
  not the cwd's; a git worktree resolves to its own name for writes and to
  both itself and its parent repository for reads.

### SessionStart

Matcher `startup|clear|compact`. Two commands run in order.

**`start`** (timeout 15s, log `start.log`) — ensures a worker daemon is
running. If one is reachable it is left alone, except in two cases: it is
running an older build than this binary (the daemon is the one long-lived
process, so after an upgrade it would keep applying old rules indefinitely),
or it writes to the built-in store while this session is configured for
another via `$CLAUDE_MEM_DB` (the daemon predates that configuration). In
both, `start` stops it with SIGTERM and spawns a replacement. Two *explicit*
different stores are warned about and left alone, because restarting on every
mismatch would thrash the daemon between two shells' stores. A spawn lock
(`worker.sock.lock`) stops concurrent sessions from spawning duplicates; the
worker is detached with `Setsid` so it outlives its launcher. Flags:
`-db`, `-socket`, `-stats`, `-model`, `-embed-model`, `-max-concurrent`,
`-metrics-addr`, `-excluded-projects` — all forwarded to the spawned worker.

**`context`** (timeout 10s, budget 8s, log `context.log`) — injects memory
before the first prompt. It reads the project's newest observations
(`-limit`, default 5, max 100) across the worktree and its parent repository,
leads with an "Unfinished from the last session" block taken from the most
recent session summary's `next_steps` (only the most recent: stale intentions
presented as current are worse than none), and returns them as
`additionalContext`. It tells the daemon how many rows it found so the read
path is observable (best-effort). Skipped for an excluded project.

### UserPromptSubmit

**`prompt-context`** (timeout 30s, budget 25s, log `prompt-context.log`) —
embeds the submitted prompt and injects the semantically closest observations
(`-limit`, default 5), so recall responds to what is being asked rather than
to what happened most recently. Not `async`: it blocks the turn, which is what
guarantees its privacy marker reaches the daemon before any `PostToolUse`
from the same turn. Guards run in this order, each logged as `skip:`:

1. Excluded project.
2. Internal protocol payload — a `<task-notification>…</task-notification>`
   block Claude Code auto-submits is not user text.
3. `<private>` tags are stripped; the daemon is told whether the prompt was
   wholly private (`PRIVATE <session_id> <0|1>`, sent on *every* prompt so a
   later non-private prompt clears the flag). A wholly private prompt stops
   here.
4. Nothing to do: embedding disabled (`-embed-model ""`) and prompt storage
   off.
5. Duplicate firing — the daemon keeps the last prompt hash per session for
   10 seconds (`DEDUPE <session_id> <hash>`), measured from the original
   prompt, so a doubled `UserPromptSubmit` neither re-embeds nor re-injects.
6. Optional prompt persistence (see [Persisting prompts](#persisting-prompts-opt-in)).
7. Prompts shorter than `-min-prompt-len` (default 20) are not embedded.

Flags: `-db`, `-socket`, `-embed-model`, `-hnsw-ef-search`, `-limit`,
`-min-prompt-len`, `-excluded-projects`, `-store-prompts`.

### PreToolUse

**`file-context`** (matcher `Read`; timeout 10s, budget 8s, log
`file-context.log`) — before a file is read, injects prior observations whose
`files_read`/`files_modified` mention it (`-limit`, default 5). Skipped inside
a subagent (`agent_id`/`agent_type` present in the payload), for an excluded
project, and for any tool other than `Read`. Two gates on the result: if the
file's mtime is newer than the newest observation about it, the memory is
stale and nothing is injected; otherwise candidates are narrowed to one per
session, most specific first. Flags: `-db`, `-socket`, `-limit`,
`-excluded-projects`.

### PostToolUse

**`hook`** (matcher `*`; `async`, timeout 120s, log `hook.log`) — reads the
payload from stdin and forwards it to the daemon's socket (`-socket`), then
exits. It does no observation work itself. A payload over
`hook.MaxPayloadBytes` (8MB, the same cap as an MCP JSON-RPC line) is
rejected whole rather than truncated, since a truncated JSON payload is
corrupt, not short. The dial timeout is 500ms. A missing daemon is logged
(`is claude-mem-go worker running?`) and the hook exits 0.

### Stop

**`stop`** (`async`, timeout 120s, budget 110s, log `stop.log`) — writes one
`type=summary` observation for the session from the observations already
persisted, never from the transcript. `run-hook.sh` detaches it after reading
stdin, because Claude Code tears hook processes down when a session ends and
`claude -p` sessions end the moment the answer prints — a Stop hook that was
merely `exec`ed produced no summary at all.

Guards, in order: no `session_id`; excluded project; subagent context;
`stop_hook_active` (the turn is being retried because another Stop hook
blocked it — a repeat Stop on an already-summarized session measured 17.3s
and a billed observer call before the content hash rejected the insert, so
returning success here is what Claude Code asks for and loses nothing); the
session marked private (`ISPRIVATE` query to the daemon; an unreachable
daemon means "not private"); a summary already exists for this session.

Waiting for in-flight observations: the last tool call's observation is still
being produced by the daemon when Stop fires, so the hook polls the session's
row count every second (`internal/cli/sessionwait.go`). It queries the daemon
(`INFLIGHT <session_id>`) and returns as soon as the daemon reports nothing in
flight for 2 consecutive checks with at least one row present. Without a
daemon answer it falls back to the row count: 10 consecutive matching
non-zero reads (~9s of confirmed no-growth, above a single observation's
usual 5–8s) within 45 polls. A daemon that reports real in-flight work
extends the ceiling to 300 polls (five minutes), since a single observation
has been measured at 104s under load. A session with no tool calls pays the
full 45s and writes nothing.

Windowing: `BySessionID` returns oldest-first, so a plain `LIMIT` would keep
the start of a session and drop its conclusions (measured on a 150-observation
session: the summary described early edits and missed the decision recorded
in the tail). The hook re-reads up to 2000 rows and summarizes
`-limit` (default 50, max 100) of them as head + tail with the middle elided.
The summary's idempotency key is the session id alone, so a Stop that fires
twice produces one summary; the summary is embedded too (`-embed-model`).
Flags: `-db`, `-socket`, `-model`, `-embed-model`, `-limit`,
`-excluded-projects`.

### Setup

**`scripts/ensure-binary.sh`** (matcher `*`, timeout 300s) — builds the
plugin binary from the shipped source when it is missing or not runnable.
Described in [plugin-install.md](plugin-install.md#the-self-healing-binary).

### Events deliberately not wired

Claude Code 2.1.238 defines 31 hook events. Real claude-mem wires six
(`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`,
`Setup`) and so does this port. `PreCompact` looks natural for a memory
system but is unnecessary: `stop` builds its summary from observations
already persisted per tool call, so compaction destroys nothing it needs.
Real claude-mem's `user-message.ts` (a Cursor banner) and `file-edit.ts`
(Cursor's `afterFileEdit`; `PostToolUse` with `matcher: "*"` already sees
Edit/Write) have no Claude Code analogue. Its `FILE_READ_GATE_MIN_BYTES` size
gate is not ported because its value is not defined anywhere findable.

## The worker daemon

`worker` is the one long-lived process. It listens on a Unix socket
(`~/.claude-mem-go/worker.sock`; the path is validated against the OS limit,
104 bytes on macOS), opens its `memory.Backend` once for its lifetime, and
processes each `PostToolUse` payload in its own goroutine with panic recovery.

**Processing one event.** Skip if there is no `tool_name`, the project is
excluded, or the session is marked private. `<private>` and the other memory
tags are stripped from `tool_input`/`tool_response` *before*
`transcript.Truncate` caps each field at 1500 bytes (a tag straddling the cut
would otherwise be left unclosed and unmatchable). The observer turn runs on
a cached `claude` session for that `session_id`, retrying once on a fresh
session for a transient or rate-limit failure (`classify` distinguishes those
from auth/quota errors that would fail identically). The observation is
inserted, deduped on `ContentHash` (a duplicate is logged, not an error), then
embedded; embedding failure is additive-only — the row is keyword-searchable —
and `embed.Client.Embed` itself retries once after 250ms on a network error or
5xx, never on a 4xx or an empty vector.

**Session cache and `-max-concurrent`.** Each cached session is one idle
`claude` subprocess holding one observer slot; turns on a session are
serialized by a per-session mutex. `-max-concurrent` (default 4) is the
number of slots. A session arriving past it evicts the least-recently-used
idle cached session — never one mid-turn — and retries every 250ms for up to
2 minutes, logging at `WARN` that `-max-concurrent` should be raised if it
cannot get a slot. The default is 4 rather than real claude-mem's 2 because
three real concurrent Claude Code windows at 2 evicted and respawned an
observer (~1–2s) on every third tool call. A negative or zero value is clamped
to 1. Idle sessions are closed after 10 minutes by a sweep that uses
`TryLock` so it never closes a subprocess mid-turn; `lastUsed` is refreshed
when a turn finishes, not only when it starts.

**Shutdown.** On SIGTERM the daemon waits for every dispatched turn to finish
(a `WaitGroup` covers turns for sessions not yet in the cache), then waits up
to 5s per cached session for its in-flight turn before force-closing, then
closes the store. Daemon exit is logged at `ERROR`.

**Socket protocol.** Besides the fire-and-forget hook forward, the socket
answers plain-text queries used by the other hooks: `INFLIGHT <session_id>`,
`PRIVATE <session_id> <0|1>`, `ISPRIVATE <session_id>`, `DEDUPE <session_id>
<hash>`, and recall-outcome reports from the three read hooks. Reads have a
30s server-side deadline so a client that connects and never writes cannot
leak a goroutine, and the same 8MB payload cap as the client.

**Stats and metrics.** After every event the daemon writes
`~/.claude-mem-go/worker-stats.json`: `processed`, `duplicates`,
`observer_errors`, `insert_errors`, `embed_errors`, per-source recall
counters (`recall_prompt`, `recall_session`, `recall_file`, each
`searches`/`empty`), `cached_sessions`, `pool_in_flight`, `pool_capacity`,
`last_activity_at`, plus its `store`, `version`, and `pid` — which is how
`start` and `doctor` detect a stale daemon or a store mismatch. `doctor`
gates those live-state findings on the daemon being reachable, so a dead
daemon's leftover file is reported as "last worker activity before it
stopped" rather than as current. `-metrics-addr 127.0.0.1:9090` serves the
same data as Prometheus text at `/metrics`; it is the only TCP listener in
the project and is off by default.

**Stale-daemon replacement and store mismatch** are `start`'s job; see
[SessionStart](#sessionstart). `doctor` reports a daemon on a different store
than the command as critical, and an older build as informational.

## Privacy

### `<private>` tags

Wrap anything in `<private>...</private>` to keep it out of memory — the
convention real claude-mem teaches in its own `UserPromptSubmit` banner. The
`privacy` package strips the same six tag names as real claude-mem's
`stripMemoryTags`: `private`, `claude-mem-context`, `system_instruction`,
`system-instruction`, `persisted-output`, `system-reminder`. Go's RE2 regexp
has no backreferences, so it is one compiled pattern per tag rather than a
single combined one; results are identical for same-tag nesting, and a
difference could only show in a per-tag breakdown this package does not
expose. Applied at both capture boundaries:

- **Tool calls** — the daemon strips `tool_input`/`tool_response` before
  truncation.
- **Prompts** — `prompt-context` strips the prompt before embedding. A prompt
  that strips to nothing marks the whole turn private: the daemon then skips
  every `PostToolUse` for that session and `stop` skips the summary. The flag
  is re-asserted on every prompt rather than set once, because a sticky flag
  would freeze a session forever after one private prompt (real claude-mem's
  issues #2794/#2795). An absent flag means "not private"; an unreachable
  daemon means the same.

The unit tests are ported from real claude-mem's `tag-stripping.test.ts`,
including the ReDoS-volume cases (150 tags and a 10,000-character tag, each
under one second) and a tool result carrying a nested `claude-mem-context`
block. `stop` needs no stripping of its own: its input is already-redacted
observations.

### Excluded projects

Real claude-mem's `CLAUDE_MEM_EXCLUDED_PROJECTS` opts projects out of
automatic tracking. This port takes the same comma-separated glob patterns
(`*`, `**`, `?`, and a leading `~` for the home directory) through the
`-excluded-projects` flag on `worker` (forwarded from `start`) and on each of
`context`, `file-context`, `prompt-context`, and `stop`, matching real
claude-mem's design of an independent check at the top of every automatic
handler. A pattern is matched against both the project's full path and its
directory basename; a match is logged as `skip: project excluded (cwd=...)`
before any work — for the daemon, before an observer subprocess is spawned
or reused. The glob semantics were verified byte-for-byte against real
claude-mem's `isProjectExcluded`/`globToRegex` on the same 19 cases,
including the `!cwd` guard that `shouldTrackProject` applies first.
Excluded projects still work with the MCP server and CLI; only automatic
capture and recall are off.

### Persisting prompts (opt-in)

Real claude-mem stores every prompt in a `user_prompts` table; by default
this port keeps only a 10-second SHA-256 in daemon memory for deduplication.
The table exists on both backends (SQLite migration 8 with an FTS5 index,
Postgres migration 6 with a `tsvector`/GIN index) with the reference's
columns: `session_id`, `project`, `prompt_text`, `prompt_number`,
`created_at`, `created_at_epoch`.

Writing to it is off because it stores the user's verbatim words rather than
a model's summary. Enable with `-store-prompts` on `prompt-context` or
`CLAUDE_MEM_STORE_PROMPTS=1` in the hook's environment. The write runs after
every privacy gate above — excluded project, internal protocol payload,
`<private>` stripping (only the stripped text is stored; a wholly private
prompt is not stored at all), and the duplicate-firing guard — and before the
`-min-prompt-len` check, so "yes" is stored even though it is not embedded.
A storage error is logged at `WARN`; the hook still prints `{}`.

Reading it back: the `search_prompts` (keyword, or enumerate newest-first
with no query) and `session_prompts` (one session in `prompt_number` order)
MCP tools, project-scoped like their observation counterparts. `stats` prints
a `prompts` line once any exist; `prune -older-than-days` deletes prompts in
the same scope as observations (its reported count stays observations-only);
`export` writes prompt rows after observations tagged `"kind":"prompt"` and
`import` dispatches on that tag, so older export files still import.

## Why the worker/hook split

An earlier design had the `PostToolUse` hook call the observer directly and
lost observations: `"async": true` means Claude Code does not wait for the
hook, not that the hook's process survives the `claude` process exiting. A
daemon started once and detached via `Setsid`, with hooks doing nothing but a
local socket write, is the fix. The daemon also opens its store once rather
than per event (a daemon meant to run for days would otherwise pay a TCP
handshake to Postgres per tool call) and, with the bounded Postgres pool, is
what keeps a burst of tool calls from exhausting a shared server's
`max_connections`.

## Running the worker as a supervised service

By default the daemon starts lazily from `SessionStart`, so a crash or a
reboot with no active session leaves capture dead until the next session
starts. `deploy/systemd/claude-mem-go-worker.service` (Linux, user-level
systemd) and `deploy/launchd/com.claude-mem-go.worker.plist` (macOS launchd
user agent) supervise it independently; install steps are in each file's
header. `start` checks for a reachable worker before spawning, so a supervised
daemon and the lazy launch coexist. The systemd unit passes
`systemd-analyze verify`; the launchd agent was loaded on a real machine,
answered `doctor`, and was relaunched by `KeepAlive` within seconds of being
killed.
