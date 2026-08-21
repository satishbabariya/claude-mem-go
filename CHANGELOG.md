# Changelog

Grouped by rough milestone, not one entry per commit — `git log` is the
exact, granular history; this is the "what actually changed and why"
summary. Dates are when each milestone landed, not a formal release
process (this project doesn't cut tagged releases on a schedule).

## 0.3.0 — 2026-08-21

- **The worker daemon now exposes real per-session in-flight state** —
  the architectural follow-up the Stop hook's own doc comments had named
  as unresolved rather than attempted. A small plain-text `INFLIGHT
  <session_id>` query, answered synchronously over the same socket the
  fire-and-forget hook-forwarding protocol already uses (`hook.Forward`
  and the daemon's `handleConn` now branch on which kind of payload
  arrived), backed by a new `inflightTracker` that counts, per session,
  how many `PostToolUse` events are currently between "received" and
  "fully processed." `Stop` now queries this directly
  (`hook.QueryInFlight`) instead of only ever inferring from watching the
  observations table's row count.
- **This closes a real gap the previous (streak-based) Stop-hook fix
  still had**: its wait ceiling was a fixed ~45 seconds regardless of
  what was actually happening, because the row-count heuristic had no
  way to tell "still working" apart from "give up" beyond a fixed
  timeout. A live re-verification run of that fix hit a real single
  observation that took 104 seconds to process (an unusually large
  summarization call) — well past the old ceiling — and would have been
  cut off mid-flight, silently producing a summary missing that
  observation. The new query lets `Stop` extend its wait to five minutes
  once the worker confirms real activity for the session (an observation
  already persisted, or an event actively in flight), while a genuinely
  tool-call-free session still finishes in the original ~45s — the
  extension requires actual evidence of activity, not just "the worker
  answered." It also exits fast once the worker confirms nothing is left
  in flight, rather than waiting out the full row-count streak. The
  row-count streak can no longer independently declare "stable" while
  the worker is simultaneously reporting real ongoing work — an override
  this fix specifically needed, confirmed by briefly disabling it and
  watching a dedicated test fail exactly as it should (the old heuristic
  locking in early despite the worker's contrary signal).
- Verified with new unit tests (the fast-exit path, the ceiling
  extension past the old 45-attempt limit, and a non-regression test
  proving a genuinely empty session still pays only the original
  budget — not five minutes just because the worker happened to be
  reachable), each confirmed as a genuine regression test by briefly
  breaking the corresponding logic and watching the test fail before
  restoring it. Also verified with a dedicated real-socket test in the
  `worker` package (a real Unix socket, the actual `hook.Forward` and
  `hook.QueryInFlight` client functions, an `observer.Handle` that
  blocks on command) proving the query reports nonzero while an event
  is genuinely being processed and zero once it completes. Finally
  verified live end to end with the compiled binary: a real worker
  daemon, a real two-tool-call `claude` session, and `Stop`'s own log
  correctly reporting "from 2 observations" via the new query path.
- **New MCP tool: `observation_context`**, matching real claude-mem's own
  tool of the same name — the on-demand form of `prompt_context.go`'s
  `UserPromptSubmit` hook (semantic recall against the actual submitted
  prompt), the last read capability in this project that had no
  on-demand MCP equivalent; every other hook-only read — including
  `context.go`'s own `SessionStart` dump, via `RecentByProject` — already
  got a tool earlier this session
  (`recent_observations`/`session_observations`/`file_observations`).
  Unlike `semantic_search_observations` (closest in shape — same
  embed-the-query-then-`SemanticSearch` pipeline), this returns the exact
  same pre-formatted, ready-to-inject text block the hook produces
  automatically, not a list of results for a caller to interpret. The
  formatter is a deliberate duplicate of `prompt_context.go`'s
  `formatPromptContext`, not a shared import — `mcpserver` can't import
  `cmd/claude-mem-go`, which itself imports `mcpserver` for the `mcp`
  command, so importing it back would be a cycle. MCP tool count grows
  from 8 to 9. Verified with a real Ollama-backed test asserting the tool
  call's output matches the hook's exact format byte-for-byte for an
  observation found purely by meaning (no keyword overlap with the
  query) — confirmed as a genuine test, not a tautology, by briefly
  breaking the formatter's header text and watching it fail before
  restoring it — plus dedicated tests for the missing-query and
  no-embed-model-configured error cases. Also confirmed end to end
  against the live Postgres container through the compiled binary
  (`add_observation` then `observation_context` in one real session),
  not just the SQLite path the unit tests exercise.
- **New `-hnsw-ef-search` flag** (`semantic-search`, `prompt-context`,
  `mcp`) overrides pgvector's own `hnsw.ef_search` query-time recall/speed
  tradeoff — real ANN search "at scale" otherwise has no actual tuning
  knob once `observations` grows well past the row counts pgvector's
  built-in default (40) was tuned against. Building it surfaced a real,
  surprising Postgres/pgvector behavior: pgvector's documented 1..1000
  bound on this value is **not reliably enforced by Postgres itself** —
  `hnsw.ef_search` is a custom GUC the pgvector extension registers, and
  until something on a given backend connection has already touched the
  vector extension, Postgres treats the name as an unchecked placeholder.
  Reproduced directly: identical Go code (`BEGIN`, `SET LOCAL
  hnsw.ef_search = 1001`, `COMMIT`) correctly errored through a connection
  a prior real `Insert`/`SaveEmbedding` call had already warmed up, but
  silently accepted the exact same invalid value with no error at all on
  an otherwise-idle fresh connection whose first-ever query was that `SET
  LOCAL`. Fixed by validating the range in Go at `Open` time instead of
  ever trusting Postgres to catch it, so a misconfigured value fails the
  same way every time regardless of incidental connection state. The
  override itself is applied via a transaction-scoped `SET LOCAL`, not a
  plain `SET`, against the pooled connection every backend call shares —
  `database/sql` gives no control over which physical connection any one
  call gets, so a plain `SET` would silently persist onto whatever
  unrelated query the pool next hands that same connection. Verified
  against the real container: the out-of-range rejection (confirmed as
  a genuine check by temporarily disabling it and watching the test
  fail), and that a valid override doesn't leak past its own call
  (checked on a pool forced to a single connection, so this is
  deterministic rather than merely likely) — confirmed as a genuine
  regression test the same way, by briefly using a plain `SET` instead
  of `SET LOCAL` and watching the leak-check fail before restoring it.
- **New MCP tool: `important_workflow`**, matching real claude-mem's own
  zero-dependency, static-text tool of the same name and shape: it never
  touches the store, existing purely to teach a client the intended
  `search_observations` → `timeline` → `get_observations` pattern (narrow
  to a few IDs before paying for full detail, not the reverse). MCP tool
  count grows from 9 to 10. Registered first in the tool list, matching
  real claude-mem's own ordering. Verified with a unit test asserting the
  exact static text, confirmed as a genuine test by temporarily disabling
  the tool's dispatch case and watching it fail with "unknown tool"
  before restoring it, plus live end to end against the real Postgres
  container through the compiled binary.
- **Fixed a real, unrelated staleness bug found by hand while verifying
  the above live**: the `initialize` response's `serverInfo.version` had
  been hardcoded to the literal `"0.1.0"` since early in the project and
  never updated across several real version bumps since — three releases
  stale by the time this was noticed. Fixed by deriving it from Go's own
  VCS build info instead (`runtime/debug.ReadBuildInfo`, the same source
  `version`/`doctor` already use), so it can't go stale the same way
  again. Confirmed live: the compiled binary's `initialize` response now
  reports the real build commit, matching `git rev-parse HEAD` exactly.
- **`Backend.Search` gains `offset` pagination** (`search` CLI's
  `-offset`, `search_observations`'s `offset` argument), in both the
  SQLite and Postgres backends. Real claude-mem's own search tool has
  this; this project's README previously justified skipping it on a
  rationale ("doesn't map as directly onto an existing column") that
  never actually applied to offset specifically — that reasoning holds
  for the date-range filter and sort-order option this project genuinely
  doesn't port, but offset needs no column at all, just a plain
  `LIMIT`/`OFFSET` on the existing query, the same shape every other
  paginated read here already uses for `limit`. Fixing the doc's own
  inaccurate justification alongside implementing the feature it
  wrongly covered. Both backends' `Search` now order by rank
  (bm25/`ts_rank_cd`) THEN `id`, not rank alone — a rank tie between two
  rows is real, and pagination via `LIMIT`/`OFFSET` needs a fully
  deterministic order or two calls at different offsets could return the
  same row twice or skip one, depending on whatever arbitrary order the
  database happens to visit tied rows in. Verified against both real
  backends (including the live Postgres container) with a dedicated test
  seeding 5 matching rows and confirming 3 pages of size 2/2/1 are
  disjoint and together cover every seeded row exactly once, each
  confirmed as a genuine regression test by temporarily dropping the
  `OFFSET` clause and watching the test fail before restoring it. Also
  verified at the MCP protocol boundary (a dedicated test proving the
  `offset` argument actually reaches `Search`, not just that `Search`
  itself works) and live end to end against the shared Postgres
  container through the compiled binary, paging real accumulated data.
- **`worker.handleConn` now bounds wall-clock time, not just byte count**.
  Before this, a client that dials the daemon's socket and never writes
  or closes (a stalled process, or a bug in some future caller not going
  through `hook.Forward`) leaked that goroutine and its underlying file
  descriptor for as long as the daemon ran — meant to be days. More
  relevant now that the `INFLIGHT` query protocol shares this socket as a
  synchronous request/response exchange, not just the original one-way
  hook forward: the client side (`hook.QueryInFlight`) already set its
  own deadline, but the server side never did. Fixed with a
  `handleConnReadTimeout` (30s — generous enough that neither a normal
  hook forward nor a real `INFLIGHT` query, both near-instant on a local
  Unix socket, ever approach it). Verified with a real `net.Conn`: a test
  shrinks the timeout, connects a client that deliberately never writes
  or closes, and confirms `handleConn` actually returns once the deadline
  elapses — confirmed as a genuine regression test by temporarily
  removing the deadline call and watching the test time out before
  restoring it.
- **`skills/mem-search/SKILL.md` was stale** — it still said "eight
  tools" and never mentioned `observation_context`, `important_workflow`,
  or `search_observations`'s `offset` argument, all added earlier this
  session. The skill whose entire job is teaching Claude which tool to
  reach for is the one place this staleness actually mattered. Updated to
  document all ten tools and their current arguments. Verified live, not
  just that the markdown parses: `--plugin-dir` loaded this plugin into a
  real `claude -p` session and asked it to list every MCP tool name the
  skill mentions — all ten came back.
- **`doctor`/`HealthDetails` gain `hnsw_ef_search` visibility** — a real
  observability gap once `-hnsw-ef-search` existed: nothing anywhere
  confirmed whether an operator's override was actually configured.
  `doctor` gains the identical `-hnsw-ef-search` flag so it can be
  pointed at the same value used elsewhere (`mcp`/`semantic-search`/
  `prompt-context`) and reports it back verbatim, or `default (40)` when
  unset. Reports the `Store`'s *configured* value, not a live Postgres
  session setting — there isn't one to read, since `SemanticSearch`
  applies the override per call via a transaction-scoped `SET LOCAL`, not
  a persistent session GUC. Verified against the live Postgres container
  both ways (configured and default), each confirmed as a genuine
  regression test by temporarily breaking the field assignment and
  watching the test fail before restoring it.
- **`skills/mem-doctor/SKILL.md` was stale the same way `mem-search`'s
  was** — never mentioned `doctor`'s `-hnsw-ef-search` flag or the
  `hnsw_ef_search` health field, both added earlier the same session as
  this skill's own last edit. Updated its "What it checks" list.
  Verified live the same way: `--plugin-dir` loaded this plugin into a
  real `claude -p` session and asked it to list every distinct thing the
  skill says `doctor` checks — `hnsw_ef_search` came back among them.
- **A real concurrency bug: the idle-session reaper could close a
  subprocess mid-turn.** `worker.sessionCache`'s `evictIdle` tore down a
  cached session's `claude` subprocess purely on a stale `lastUsed`
  timestamp, with no awareness of `sessionEntry.mu` — but nothing bounded
  how long a single turn could run (a real single observation this
  project has measured took 104 seconds), and `lastUsed` was only ever
  refreshed when a turn *started*, never when one finished. A turn
  genuinely still in flight past the idle window could have its handle
  closed out from under it — a real use-after/during-close hazard on the
  subprocess's stdin/stdout, not just a wasted turn (confirmed one level
  deeper: `claude-agent-sdk-go`'s own `Session.Close`/`Send` have no
  synchronization between them either). Fixed with `entry.mu.TryLock()`
  before evicting — succeeding proves no turn is running and it's
  genuinely safe to close; failing means a turn is active, and the sweep
  simply defers to the next cycle a minute later. A second, related fix:
  `lastUsed` is now also refreshed when a turn *finishes* (`touch`,
  called from `process`), not just when one starts, so the idle clock
  reflects real last-activity time instead of only a turn's start.
  Verified with dedicated concurrency tests under `go test -race`
  (a handle whose `Observe` blocks on command, driving `process`'s own
  critical section directly, confirming eviction is skipped while
  genuinely in flight and still happens once the turn finishes), each
  confirmed genuine by reverting to the old unsafe logic and watching the
  test fail before restoring the fix. Also verified live against the
  compiled binary: temporarily shrunk the idle timeout to 3s and the
  sweep interval to 1s, then ran a real `claude` session — a genuine
  ~9-second real observation spanned several sweep cycles inside that
  window, and the daemon stayed alive with the observation correctly
  persisted, no panic, no crash.
- **The same gap existed on the shutdown path too.** `closeAll` (daemon
  shutdown) had the identical bug `evictIdle` just had: it called
  `evict` — no `entry.mu` awareness — on every session regardless of
  whether a turn was still genuinely in flight. The fix can't be
  identical, though: shutdown has no "next sweep" to defer an in-flight
  session to, so `closeAll` now *waits* (a real `Lock`, not a `TryLock`)
  for each turn to finish naturally, up to a `closeAllGracePeriod` (5s,
  matching this daemon's own metrics-server shutdown timeout), then
  force-closes anyway rather than blocking shutdown forever for a
  runaway turn. Every session's wait runs concurrently, so total
  shutdown time stays bounded regardless of session count. Deliberately
  scoped to just this mutex-safety gap, not a full graceful-shutdown
  redesign — a `handleConn`/`process` goroutine already dispatched
  before shutdown began could still race `getOrCreate` for a brand-new
  session, a broader concern documented rather than attempted here.
  Verified with dedicated concurrency tests under `go test -race` (one
  confirms `closeAll` waits for an in-flight turn before closing it,
  another confirms it force-closes after the grace period rather than
  hanging), both confirmed genuine by reverting to the old unsafe logic
  and watching them fail before restoring the fix. Also verified live: a
  real detached worker daemon, a real forwarded hook payload, a real
  `SIGTERM` sent while the observation was genuinely in flight, and a
  clean shutdown with no panic, no hang, no orphaned process left behind.
- **`observations.type` had zero validation anywhere, in either
  backend.** Nothing — not the schema, not the Go code — validated
  `type` against this project's own small, fixed vocabulary (`discovery`/
  `change`/`decision`/`summary`/`manual`). An LLM's `<type>` tag
  drifting to an unrecognized value, or a corrupted/hand-edited import
  file, would have silently persisted a row invisible to any `-type`/
  `type` filter with no error anywhere. Confirmed as a real, worth-fixing
  gap by checking actual production data first: queried the distinct
  `type` values across 3000+ real rows accumulated in this project's
  shared Postgres dev container, and every one already fell within the
  vocabulary. Fixed at both real ingestion boundaries (`Insert` and
  `ImportRow`, via their shared `insertRow`) with a new
  `store.ValidateObservationType`, in both backends. Also added a real
  Postgres `CHECK` constraint via a new migration — a schema-level
  guarantee regardless of which code path ever writes a row, wrapped in
  an idempotent `DO` block since Postgres has no
  `ADD CONSTRAINT IF NOT EXISTS`. Verified with dedicated tests in both
  backends (each confirmed genuine by temporarily disabling the
  validation and watching the test fail), and the Postgres `CHECK`
  constraint itself verified independently of the Go code: dropped it by
  hand, confirmed a raw SQL `INSERT` with a bad `type` succeeded, then
  restored it and confirmed the identical raw `INSERT` now fails with a
  real constraint-violation error.

## 0.2.0 — 2026-08-20

Enterprise-readiness pass: schema completeness, observability, backup, and
a run of real bugs found by building and testing real features rather
than assumed correct.

- **Schema migrations** — a real, versioned `schema_migrations` framework
  shared by both backends, replacing ad hoc per-column upgrade checks.
- **MCP tool surface** grew from 2 tools to 7: `recent_observations`,
  `session_observations`, `file_observations` alongside
  `search_observations`/`semantic_search_observations`, plus
  `add_observation` — the surface's only *write* tool, letting Claude
  explicitly persist something worth remembering that isn't the direct
  result of one tool call (closing a real gap real claude-mem's own
  `observation_add` tool covers). Its first version had a real gap of its
  own — it never embedded the new observation, so it was invisible to
  `semantic_search_observations` even though keyword search found it fine
  — fixed the same iteration, verified with a real Ollama-backed test.
  Then **`get_observations`** — fetch full details (narrative, facts,
  concepts, files) for specific IDs, closing the gap every other tool's
  deliberately-abbreviated list output left: no way to see an
  observation's full content without a separate CLI call outside the MCP
  surface entirely. New `Backend.ByIDs` method backs it in both storage
  backends (SQLite's hand-built `IN (?,?,...)`, Postgres's native
  `= ANY($1)` array support), scoped to the current project the same way
  `search_observations` is. Verified against a real `claude` CLI session,
  not just a unit test.
- **`PreToolUse` (file-context), `Stop` (session summary), and
  `UserPromptSubmit` (prompt-context) hooks wired**, closing three of the
  previously-unwired hooks. `UserPromptSubmit` is the sharpest of the
  three: it embeds the actual submitted prompt text and injects the
  observations semantically closest to *that specific question*, rather
  than `SessionStart`'s static "recent observations" dump — the same real
  gap real claude-mem's own `session-init` handler covers. Required a new
  `Prompt` field on `claude-agent-sdk-go`'s `HookInput` (confirmed against
  a real captured payload, tagged `v0.1.1`). Verified against a real,
  isolated Ollama call and a real `claude` CLI session: two topically
  distinct seeded observations, a prompt with no keyword overlap with
  either, and Claude's answer correctly identified the semantically
  relevant one and explicitly attributed it to injected memory. `Setup`
  remains unwired — real claude-mem uses it for Node/Bun version-checking,
  which has no equivalent for a single static Go binary.
- **`prune`** — retention command (dry-run by default), and **`export`/
  **`import`** — backup and the SQLite↔Postgres migration path. Both new;
  neither existed before.
- **Worker observability** — activity counters (`worker-stats.json`)
  surfaced through `doctor`, where before there was only raw log text.
  Also, optionally, a real Prometheus `/metrics` endpoint
  (`-metrics-addr`) serving the same counters for a real monitoring
  stack to scrape — opt-in, since it's the one thing about the worker
  that listens on more than a Unix socket.
- **`version` command** — build commit/time via Go's own VCS stamping.
- **`Backend.HealthDetails()`** — backend-specific facts `doctor` now
  prints: SQLite's real PRAGMA settings at runtime (confirming the
  WAL/foreign-keys fix actually took effect); Postgres's real connection
  pool utilization, pgvector extension version, and whether the HNSW
  index real ANN search depends on still exists.
- **The `Stop` hook's session summary could silently skip a session, or
  silently omit its most recent action**, found by the same real
  full-stack integration test as the migration race above (two genuine
  concurrent `claude` sessions through real hooks): `Stop` fires the
  instant a session ends, but `PostToolUse`'s own observation for the
  session's LAST tool call is a fire-and-forget async LLM call the
  worker runs in the background — a single, immediate database read
  raced ahead of it. Reproduced directly: one real session's `Stop` hook
  found zero observations 4 seconds before the worker finished
  persisting the only one that session had. Fixed by polling until the
  count stops growing (up to ~12s, costing nothing perceptible since
  `Stop` already runs fire-and-forget) — but the first version of that
  fix had its own bug, caught live before shipping: treating two
  consecutive zero-reads as "confirmed empty" is wrong, since a
  one-tool-call session also reads zero until the observer call actually
  finishes. Fixed to require the count go positive before trusting
  stability. Verified with dedicated tests for each failure mode, and by
  re-running the original two real concurrent sessions — the first
  re-run still showed the flaw, the second correctly summarized both.
- **The `Stop` hook's session summary had a *third* real bug**, found by
  the Postgres backend's own full-stack test with a single session
  running two real tool calls back to back: even the corrected "two
  consecutive matching non-zero reads = stable" rule was still wrong,
  because one worker mutex serializes observation processing per session
  (`worker/sessions.go`) — the count can plateau at 1 for several real
  seconds while the second tool call's observation is still mid-flight,
  and two quick matching reads during that plateau falsely "confirm"
  stability. Reproduced live down to the timestamp: the first tool
  call's observation landed, the second started six seconds later on
  the same `session_id`, and `Stop` summarized "from 1 observations"
  instead of 2. Fixed by replacing the 2-check rule with a streak
  counter requiring 10 consecutive matching non-zero reads (~9s of
  confirmed no-growth, well above a single observation's own observed
  ~5-8s latency) before trusting the count, raising the overall wait
  budget from 12 to 45 polls to accommodate. Documented honestly as a
  heuristic, not a guarantee — a fully robust fix needs the worker to
  expose real per-session "turn in flight" state, a legitimate
  architectural follow-up this fix doesn't attempt. Verified with a new
  test reproducing the exact plateau shape, confirmed as a genuine
  regression test by temporarily reverting the streak threshold to 2
  and watching it fail, then restoring it.
- **A real, full-stack integration smoke test found a third,
  more serious `migrate` bug** the targeted unit/e2e tests didn't catch:
  `SessionStart`'s `start` (spawns the worker daemon, which itself calls
  `Open`) and `context` (also calls `Open` directly) can both race to
  migrate the SAME brand-new SQLite file on a project's very first
  session. Reproduced deterministically: concurrent connections to the
  same fresh file surfaced three *different* real errors depending on
  timing — `"database is locked"`, `"UNIQUE constraint failed:
  schema_migrations.version"`, and `"duplicate column name"` (a
  migration's own idempotency check racing against an identical
  concurrent check). Fixed by retrying `Run`'s entire check-and-apply
  sequence on any failure rather than trying to prevent the race at the
  SQL level — safe because every `Migration.Apply` is already required
  to be idempotent. Verified with a dedicated concurrency test
  (deliberately widened race window, proven to fail 3/3 without the fix
  and pass 8/8 with it under `-race`) and by re-running the original
  full integration scenario five more times with zero failures.
- **Field truncation could corrupt real tool output mid-character.**
  `transcript.Truncate`, on the live hot path for every single tool call
  the worker daemon processes, cut fields at a plain byte-offset slice
  with no regard for UTF-8 rune boundaries — any non-ASCII character
  (accented paths, emoji, box-drawing characters, non-English text)
  straddling the 1500-byte cutoff got sliced in half, producing invalid
  UTF-8. Fixed by walking back to the nearest real rune-start byte before
  cutting. Verified with a unit test forcing the cutoff to land
  mid-character, and a real end-to-end run through an isolated worker
  daemon with an engineered multi-byte payload sent over its actual
  socket, confirming the full real pipeline completes cleanly.
- **`migrate.Run` had two real bugs in the framework both backends'
  schema changes depend on**, found by hand rather than assumed correct:
  it documented "ascending Version order" but never actually sorted
  migrations — a slice listing version 2 before version 1 applied version
  2 *first*. Worse, two migrations accidentally sharing a Version number
  didn't error: the second one was silently skipped forever once the
  first got recorded as applied, indistinguishable from having run
  correctly. Fixed by sorting explicitly and rejecting duplicate versions
  outright. Every real migration so far happened to be listed in order
  with unique versions, which is exactly why this went unnoticed until
  audited directly. Verified against both real backends, including the
  live Postgres container in a throwaway schema.
- **`search`/`search_observations` gained a `type` filter** — the small,
  fixed observation-type vocabulary this project's observer actually
  writes (`discovery`/`change`/`decision`/`summary`/`manual`), the same
  real gap real claude-mem's own search tool covers with its `obs_type`
  parameter. Threading it through the Postgres backend found a real
  latent footgun: the existing `project` scope clause hardcoded its
  placeholder number (`$3`), correct only because there was never a
  second optional filter to disturb that assumption. Rewritten to build
  placeholder numbers dynamically as each optional filter is appended.
  Verified against both real backends (including a dedicated Postgres
  test applying `project` and `type` together — the exact combination
  the old hardcoded scheme couldn't have handled safely) and a live
  `claude` CLI session confirming the MCP tool's own `type` argument
  correctly narrows results.
- **`SECURITY.md`** — the trust model this project didn't have written
  down anywhere: no auth on the MCP server or worker socket (the
  boundary is the local OS user, same as any MCP server), the one real
  network surface (the opt-in Prometheus endpoint) should stay bound to
  localhost, DSN credentials are never logged in the clear, every
  caller-supplied size/count is bounded, and both long-lived processes
  now recover from panics instead of crashing — plus what's explicitly
  out of scope (encryption at rest, rate limiting) and how to report a
  vulnerability (GitHub's private vulnerability reporting).
- **`pool.New` clamps a non-positive `maxConcurrent` instead of crashing
  the worker daemon at startup.** Go's own `make(chan T, n)` panics with
  "makechan: size out of range" for a negative `n`, so
  `worker -max-concurrent -1` (a mistyped or computed flag) crashed
  before the daemon ever bound its socket — confirmed against a real
  running process. Zero has a quieter but equally real failure mode
  (every `Acquire` blocks forever instead of crashing). Now clamps
  anything below 1 to 1. Also fixed a smaller bug this surfaced: the
  startup log printed the *raw* flag value, not the pool's real
  (possibly clamped) capacity — `doctor`'s own stats already used the
  real value and were never wrong, but the log line claiming
  `max_concurrent=-1` while the pool actually ran at capacity 1 was a
  real inconsistency. Verified against a real running worker.
- **Every `Backend` method taking a `limit` now clamps a negative value,
  and — the more consequential fix — the worker daemon and MCP server now
  recover from a panic instead of crashing the whole process.** Auditing
  every `limit`-taking method (prompted by Timeline's own negative-depth
  fix) found the identical "SQLite's LIMIT treats negative as unlimited"
  bug in `Search`, `RecentByProject`, `BySessionID`,
  `ObservationsForFile`, `ExportAll`, and `ObservationsNeedingEmbedding`.
  `SemanticSearch` had a *worse* version: it slices its own results in Go
  (`all[:limit]`), so a negative limit didn't return everything — it
  **panicked** with a real "slice bounds out of range" error. Since
  neither the worker daemon (`SemanticSearch` runs in its per-event
  goroutine) nor the MCP server (synchronous request handler) had *any*
  panic recovery anywhere, that panic would have crashed the entire
  shared process, not just failed one call. Not reachable through the
  live MCP tool surface today (its own caller already substitutes a
  default before calling any of these), so this is defense-in-depth for
  the `Backend` contract and the two long-lived processes built on it —
  the same reasoning as Timeline's fix, but this time paired with an
  actual process-level safety net. Verified thoroughly: a regression test
  per affected method against both real backends, a fault-injection test
  in each of `worker`/`mcpserver` (a fake `Backend` that panics on every
  call, proving the process survives and a caller gets a clean error
  instead of a dead connection), and a live `claude` CLI session
  confirming the server stays alive and keeps working afterward.
- **`Backend.Timeline` clamps `depthBefore`/`depthAfter` to
  `[0, MaxTimelineDepth]` at the store layer**, not just in the MCP tool's
  own caller. Found the hard way: a negative depth isn't "no results" —
  SQLite's `LIMIT` treats a negative value as *unlimited*
  (`Timeline(..., -1, -1)` returned every row before the anchor), and
  Postgres fails differently for the identical root cause (a real "LIMIT
  must not be negative" driver error) rather than silently returning
  everything. The live `timeline` MCP tool was never actually reachable
  through this (its caller already substitutes the default of 3 for any
  `<=0` value before calling `Timeline`), so this is a defense-in-depth
  fix for the `Backend.Timeline` method's own contract, verified against
  both real backends with a dedicated regression test.
- **`reembed`** — the remediation half of `doctor`'s
  `embedding_dims_consistent` finding: finds every observation with no
  embedding or a stale dimension and re-embeds it with the current model
  (dry-run by default, `-yes` to actually do it, same discipline as
  `prune` — a real Ollama API cost per row even though nothing is ever
  deleted). `doctor` also now cross-checks against the *live* model's
  real dimension (via a probe embed call), not just internal consistency
  among stored embeddings — a store embedded entirely under a
  since-replaced model would otherwise report "consistent" while every
  embedding is silently unsearchable under the model that's actually
  active. New `skills/mem-reembed` surfaces it as `/mem-reembed`. Verified
  end to end with real Ollama calls: seeded a stale-dims row and a
  never-embedded row, confirmed `doctor` flagged both cases, ran
  `reembed -yes`, confirmed both fixed, and confirmed via
  `semantic-search` that the previously stale observation is now
  findable and correctly ranked.
- **`HealthDetails` now catches a real, previously-silent failure mode:
  inconsistent embedding dimensions.** If the configured Ollama embedding
  model ever changes, `SemanticSearch`'s cosine similarity returns -1 (its
  theoretical minimum) on any length mismatch rather than erroring — old
  embeddings just quietly stop ever matching a new-model query, forever,
  with nothing anywhere saying so. New `embedding_dims`/
  `embedding_dims_consistent` keys surface this in `doctor`. SQLite-
  specific in practice: Postgres reports the identical keys for parity,
  but its fixed `vector(N)` column type makes a real mismatch structurally
  impossible — confirmed directly (a mismatched save there fails loudly
  with a real Postgres error instead). Verified both ways: a real
  `doctor` run against a deliberately mixed-dimension SQLite database
  showed the inconsistency; a real save attempt against the live Postgres
  container was rejected outright.
- **`timeline`** — MCP tool #8: chronological context AROUND one
  observation (`depth_before`/`depth_after` observations surrounding an
  anchor), mirroring real claude-mem's own `timeline` tool ("step 2: get
  context around results"). Give it an `anchor` directly or a `query` to
  resolve one automatically via a single-result keyword search. New
  `Backend.Timeline` method in both backends, ordered by `id` rather than
  `created_at_epoch` (confirmed in both SQLite's rowid and Postgres's
  `BIGSERIAL` that id increases monotonically with insertion order,
  rather than assumed). Always scoped to the anchor's own project, not
  the caller's project argument at face value — the same cross-project
  leak class fixed for `Search`/`ByIDs` earlier, with its own dedicated
  regression test. Verified end to end against a real `claude` CLI
  session for both the direct-anchor and query-resolution paths.
- **`get_observations`/`Backend.ByIDs` capped at 100 IDs per call**
  (`store.MaxIDsPerLookup`). Found the hard way: a real test against this
  project's own SQLite driver showed 100,000 IDs failing outright with a
  raw "too many SQL variables" error, since the hand-built
  `IN (?,?,...)` placeholder list has no cap of its own. Enforced in both
  backends for parity (Postgres's `= ANY($1)` doesn't hit the same limit,
  but gets the identical bound anyway), and confirmed at the MCP protocol
  boundary that an oversized request comes back as a clean tool error
  mentioning the limit, not a raw driver error.
- **Hook payloads are now size-bounded** (`hook.MaxPayloadBytes`, 8MB,
  matching the MCP server's own JSON-RPC line cap) on both ends of the
  worker socket. Before this, an abnormally large `tool_response` (a
  `Bash` command catting a multi-gigabyte file) had no upper bound at
  all — a real risk specifically because the worker daemon is one
  long-lived process every project on the machine shares, so a single
  pathological tool call could balloon its memory for every other
  session using it too. Rejected whole, not truncated (a truncated JSON
  hook payload is corrupt, not just short). Verified against a real
  running daemon: sent an oversized payload over its actual socket,
  confirmed it logged a rejection and stayed alive, then confirmed a
  normal-sized payload right after processed correctly.
- **Skill docs re-synced with the actual tool surface** — `mem-search`
  still said "six tools" and never mentioned `get_observations` at all
  (added a whole iteration earlier and simply never back-filled into the
  skill instructions), and `mem-doctor`'s "what it checks" list predated
  both the worker activity counters and `Backend.HealthDetails()`, so it
  described a `doctor` that no longer matched what the binary actually
  prints. Confirmed the fix against the real tool count (`grep` against
  `mcpserver.go`'s actual tool definitions: 7, matching the corrected
  doc) rather than just editing prose.
- **`postgres.Open` retries its initial connection** on a short backoff
  (~7.75s worst case across 6 attempts) instead of failing permanently on
  the very first ping. Every real caller (the worker daemon, every CLI
  hook, the MCP server) passes a context with no deadline of its own, so a
  common startup race — this daemon, or a hook, starting a beat before
  Postgres's own container finishes its healthcheck — would otherwise
  hard-fail every time it happened to lose that race; a real, sustained
  outage is still the process supervisor's job (systemd/launchd's
  `Restart=on-failure`), not this retry loop's. Verified against the real
  docker-compose container: stopped it, called `Open`, restarted it
  mid-retry, confirmed `Open` recovered instead of failing on the first
  attempt.
- **Real binary releases** — `.goreleaser.yaml` + a tag-triggered
  `.github/workflows/release.yml` cross-compile `claude-mem-go` for
  linux/darwin × amd64/arm64 and attach them (plus a `checksums.txt`) to a
  real GitHub Release on every `vX.Y.Z` tag push. Before this, installing
  meant `go build` locally with no other option. Verified with a real
  snapshot build (no tag needed): all four targets compiled, and the
  darwin/arm64 binary was extracted and actually run — `version` and
  `doctor` both worked correctly.
- **`doctor`** extended; **`mem-search`**, **`mem-doctor`**,
  **`mem-prune`**, and **`mem-export`** Claude Code skills added —
  `mem-prune`'s instructions treat `prune` as the one genuinely
  destructive CLI operation, always dry-running and confirming before
  ever passing `-yes`.
- **Log rotation** (5MB cap, one prior generation) — logs had no cap
  before and could grow unbounded.
- **Optional systemd/launchd templates** (`deploy/`) for supervising the
  worker daemon independent of any single Claude Code session.
- **Real bugs found and fixed**, each caught by testing against something
  real rather than assumed correct:
  - A cross-project memory leak — `Search`/`SemanticSearch` had no project
    scoping anywhere in the stack.
  - A years-latent FTS5 trigger bug (`observations_ad` used
    external-content-only delete syntax on a self-contained table) that
    `prune` uncovered on its first real `DELETE`.
  - SQLite's default journal mode locking under real concurrent writers
    (fixed with WAL + a busy-timeout), and `foreign_keys` defaulting off
    (so `ON DELETE CASCADE` had never actually fired).
  - `prune`'s own cutoff computed in the wrong unit (seconds vs. the
    milliseconds `created_at_epoch` is actually stamped in) — silently
    deleting nothing at all until the next iteration caught it.
  - A Postgres DSN's password reaching logs and stdout on any connection
    failure — plus a bug in the first fix for it (a malformed DSN made an
    early `net/url`-based redaction fall back to the *unredacted*
    original).
  - The worker daemon opening a fresh database connection per event
    instead of once per process lifetime — real connection churn against
    a shared Postgres server.
  - `embed.Client.Embed` never retrying a transient Ollama failure, unlike
    the main observer call path.
  - Neither the new `add_observation` MCP tool nor the `Stop` hook's
    session-summary observation ever got embedded — both persisted a real
    observation through a different code path than the worker's own
    `process()`, and both forgot the embedding step. The `Stop` case in
    particular meant a session summary — arguably the single most
    information-dense observation this project produces — had been
    invisible to semantic search since the day that hook was written, not
    just since this pass began.
  - `export`/`import` silently dropped every observation's embedding —
    `ExportRow` carried no field for it at all. A "migrate to Postgres for
    real ANN search at scale" would have arrived with nothing left to
    search. Fixed (`Backend.ExportAll` now LEFT JOINs the embedding,
    `ImportRow` restores it) and verified against this project's own real
    dev database and the live Postgres container with a real embedding
    value.
- Also: bounded the Postgres connection pool (previously
  `database/sql`'s default of unlimited), CLI `-limit` flags clamped to
  match the MCP server's own bound, and `docker-compose.yml`'s Postgres
  container now has `restart: unless-stopped`.

See `git log` for the exact commit-by-commit history — every entry above
corresponds to one or more real commits with a full rationale in the
commit message.

## 0.1.0 — initial

The core loop: capture (via a `claude-agent-sdk-go` Observer session,
hardened against tool access), compress into a structured observation,
persist (SQLite FTS5 + brute-force cosine, or Postgres+pgvector for real
ANN + full-text search at scale), and recall (`SessionStart` context
injection, `search`/`semantic-search`). The worker/hook daemon split
(fixing a real "async hook dies with its parent" failure mode), an MCP
server exposing `search_observations`/`semantic_search_observations`,
content-hash dedup, and CI.
