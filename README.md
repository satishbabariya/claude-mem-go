# claude-mem-go

A Go reimplementation of claude-mem's core loop — capture what Claude Code
did, compress it into a structured observation, persist it, make it
searchable — built on [claude-agent-sdk-go](../claude-agent-sdk-go) instead
of the closed-source `@anthropic-ai/claude-agent-sdk` npm package.

This exists because every step of it was tested against the real thing
first: the SDK layer is a subprocess wrapper around the `claude` CLI's
documented `stream-json` protocol (verified by reading the npm package's
`.d.ts` files and exercising the protocol directly), and the worker/hook
split below exists because a simpler "hook does the work inline" design was
tried, found to lose data, and fixed — not because it seemed like a good
idea in the abstract.

## Requirements

- The `claude` CLI on `PATH` (used as the actual model backend — no API key
  management here beyond what `claude` itself already handles).
- [Ollama](https://ollama.com) running locally, with an embedding model
  pulled (`ollama pull nomic-embed-text`), if you want semantic search.
  Keyword search (`search`) works without it.
- Go 1.24+.

## Architecture

```
transcript / hook payload  →  observer (claude-agent-sdk-go Session)  →  classify
                                                                            │
                                                       store.Backend (SQLite or Postgres)
```

`store.Backend` is one interface with two implementations, selected by what
the `-db` flag looks like:

- **SQLite** (default, zero dependencies) — a file path. FTS5 keyword
  search, brute-force cosine similarity for semantic search. Opened with
  WAL journal mode, a 5s busy-timeout, and foreign keys on — all three
  fixed real, reproduced problems: this project's actual shape is the
  worker daemon and every CLI subcommand each opening their own connection
  to the same file, and the default rollback-journal mode's exclusive
  write lock made a second concurrent writer fail immediately with
  "database is locked." Separately, SQLite's foreign-key enforcement
  defaults to off regardless of what the schema declares, which meant
  `observation_vectors`' `ON DELETE CASCADE` had never actually fired —
  `prune` was silently leaving orphaned embedding rows behind. Both
  applied via DSN params (`_journal_mode`, `_busy_timeout`,
  `_foreign_keys`), not a one-time `PRAGMA` `Exec` call, since the latter
  only reaches whichever single pooled connection happens to run it.
- **Postgres + pgvector** (`postgres://...` DSN) — real full-text search
  (`tsvector`/GIN, no hand-written query sanitizer needed — Postgres
  tokenizes punctuation like the hyphen in "claude-mem" sanely by default,
  unlike SQLite's FTS5) and a real ANN index (HNSW) for semantic search
  instead of a linear scan. `docker-compose.yml` brings up
  `pgvector/pgvector:pg16`; every claim above (the HNSW index actually gets
  used, not just created; the hyphen query that broke FTS5 works here
  without a workaround) was checked with `EXPLAIN` and real queries against
  that container, not assumed. The container has a persistent named
  volume (data survives `docker compose down`) and `restart:
  unless-stopped`, so it comes back after a Docker Desktop/daemon restart
  without manual intervention — confirmed the policy actually applies via
  `docker inspect`, and confirmed Docker's real distinction between "you
  explicitly stopped it" (an explicit `docker stop`/`docker kill` — this
  policy correctly does NOT override that; verified directly) and a crash,
  which it would restart from.

```sh
docker compose up -d
./claude-mem-go ingest -db "postgres://claudemem:claudemem@localhost:55432/claudemem?sslmode=disable"
```

  `sslmode=disable` above is for the local Docker Compose container only
  (it's on `localhost`, nothing else can see that traffic). Every DSN
  example in this repo uses it for the same reason — pgx's `stdlib` driver
  already honors the standard libpq `sslmode` parameter, so no code change
  is needed to use TLS, just don't copy `sslmode=disable` into a DSN that
  crosses a real network: use `sslmode=require` (encrypted, no certificate
  verification) or `sslmode=verify-full` (encrypted and verified,
  recommended for anything production) against a real Postgres instance.
  A real Postgres DSN carries its password in plaintext — every place a
  `-db` value could reach a log line or stdout (`doctor`'s output, the
  `context`/`stop`/`file-context` hook logs, the error `postgres.Open`
  returns and every caller further up the stack that logs it) now goes
  through `store.RedactDSN` first, replacing the password with
  `REDACTED`. This was a genuine leak before, not a hypothetical one, and
  the first version of the redaction function itself had a real bug: it
  used `net/url.Parse`-then-reserialize and fell back to returning the
  *unredacted* original whenever parsing failed — exactly backwards for a
  redaction function — which a deliberately malformed DSN (a well-formed
  `user:password@` prefix followed by a broken host) triggered
  immediately. Fixed with a regex-based approach that only needs the
  `scheme://user:password@` prefix to be well-formed, independent of
  whatever comes after — verified against the exact malformed-DSN case
  that leaked, and against a real live-container auth failure.

  `-hnsw-ef-search` (on `semantic-search`, `prompt-context`, and `mcp`)
  overrides pgvector's own `hnsw.ef_search` query-time recall/speed
  tradeoff — the mandate's own "real ANN vector search... at scale"
  otherwise has no actual knob once `observations` grows well past the
  row counts pgvector's built-in default (40) was tuned against. Building
  it surfaced a genuinely surprising real behavior: pgvector's documented
  1..1000 bound on this value is **not reliably enforced by Postgres
  itself**. `hnsw.ef_search` is a custom GUC pgvector's extension
  registers, and until something on a given backend connection has
  already touched the vector extension, Postgres treats the name as an
  unchecked placeholder — reproduced directly, identical Go code (`BEGIN`,
  `SET LOCAL hnsw.ef_search = 1001`, `COMMIT`) correctly errored through a
  connection a prior real `Insert`/`SaveEmbedding` call had already warmed
  up, but silently accepted the exact same invalid value with no error at
  all on an otherwise-idle fresh connection whose first-ever query was
  that `SET LOCAL`. A caller with a typo'd or misconfigured value would
  have no reliable way to notice, since whether Postgres rejects it
  depends on incidental connection warm-up state, not the value itself.
  Fixed by validating the range in Go at `Open` time instead of ever
  trusting Postgres to catch it, so a bad value fails the same way every
  time regardless of connection state. The override itself is applied via
  a transaction-scoped `SET LOCAL`, not a plain `SET`, against the pooled
  connection every backend call shares — `database/sql` gives no control
  over which physical connection any one call gets, so a plain `SET`
  would silently persist onto whatever unrelated query the pool next
  hands that same connection. Verified against the real container: the
  out-of-range rejection, and that a valid override doesn't leak past its
  own call (checked on a pool forced to a single connection, so this is
  deterministic rather than merely likely).

- **Schema migrations** (`migrate/`) — a real, versioned schema-migration
  framework shared by both backends: a `schema_migrations` table records
  which numbered, idempotent migrations have run, so the next schema change
  has somewhere to go instead of either re-editing a `CREATE TABLE` that
  production databases already ran once, or repeating the bespoke
  "check `PRAGMA table_info`, `ALTER` if missing" pattern SQLite's
  `content_hash` column used before this existed. Both backends' existing
  schema steps (SQLite: the initial table, `content_hash`, the FTS5 index,
  the vector table; Postgres: the initial table + indexes) are now
  registered migrations rather than unconditional statements re-run on
  every `Open`. Verified against a real upgrade path, not just unit tests:
  a simulated pre-migration-framework SQLite database (missing both the
  `content_hash` column and the `schema_migrations` table itself) correctly
  migrates forward on reopen, and a live Postgres container correctly
  records its migration once and doesn't re-apply it on a second `Open`.
  `migrate.Run` itself had two real bugs, found by hand rather than
  assumed correct just because every migration so far happened to avoid
  triggering them: it documented applying migrations in "ascending
  Version order" but never actually sorted them — a migrations slice
  listing version 2 before version 1 applied version 2 *first*, silently
  violating the one guarantee this whole package exists to provide. Worse,
  two migrations accidentally sharing a Version number didn't error at
  all: once the first one got recorded as applied, the "already applied?"
  check silently skipped the second one forever, indistinguishable from
  having run correctly. Fixed by sorting explicitly before applying and
  rejecting duplicate version numbers outright, verified against both real
  backends (including the live Postgres container, in a throwaway schema
  cleaned up afterward, confirming migrations still apply in the correct
  order there too).

  A third, more serious bug in the same package was found by an actual
  full-stack integration smoke test — a real project wired with hooks
  pointing at explicit, isolated `-db`/`-socket` paths, run through a
  genuine `claude` session — rather than any of the targeted unit/e2e
  tests above: `SessionStart`'s `start` (which spawns the worker daemon,
  which itself calls `Open`) and `context` (which also calls `Open`
  directly) can both race to migrate the SAME brand-new SQLite file on a
  project's very first session. Reproduced deterministically: several
  independent connections to the same fresh file, migrated concurrently,
  surfaced three *different* real errors depending on timing —
  `"database is locked"` (SQLite's `busy_timeout` doesn't cover a losing
  DDL statement the way it covers a losing row lock), `"UNIQUE constraint
  failed: schema_migrations.version"` (two connections both saw a
  migration as unapplied and both tried to record it), and `"duplicate
  column name"` (a migration's own idempotency check — see
  `ensureContentHashColumn` — racing against an identical concurrent
  check, a classic check-then-act TOCTOU window). Fixed not by trying to
  prevent the race at the SQL level, but by retrying `Run`'s entire
  check-and-apply sequence on any failure: every `Migration.Apply` is
  already required to be idempotent, and the "already applied?" read
  happens fresh on each attempt, so a retry after a race-induced failure
  simply sees whatever the other process already committed and skips it
  — correct regardless of which specific error a given race happened to
  surface as. Verified by deliberately widening the exact TOCTOU window
  in a dedicated concurrency test (proven to reliably fail 3/3 times
  without the fix and pass 8/8 with it, including under `-race`), and by
  re-running the original full integration scenario that found this five
  more times with zero failures.

- **worker** — a persistent daemon, meant to be started once (see `start`)
  and left running. Listens on a Unix socket, processes PostToolUse
  payloads through a bounded `pool` of observer sessions. Opens its
  `store.Backend` exactly once for the daemon's whole lifetime (fixed from
  opening and closing a fresh one on every single event) — real connection
  churn otherwise, since a daemon meant to run for days would pay a fresh
  connection (a real TCP handshake against Postgres) per tool call
  indefinitely; verified end-to-end by restarting a live daemon and
  sending it multiple real events in sequence through the one handle.
  Combined with the Postgres backend's now-bounded connection pool
  (`SetMaxOpenConns`/`SetMaxIdleConns`, previously left at
  `database/sql`'s default of unlimited) — the two together are what keep
  a burst of concurrent tool calls from being the only thing standing
  between this daemon and a shared Postgres server's `max_connections`.
  `postgres.Open` also now retries its initial ping on a short backoff
  (~7.75s worst case across 6 attempts) instead of failing permanently on
  the very first attempt — every real caller (this daemon, every CLI hook,
  the MCP server) passes a context with no deadline of its own, so without
  an internal bound a startup race (this daemon, or a hook, starting a
  beat before Postgres's own container finishes its healthcheck) would
  otherwise hard-fail every single time it happened to lose that race.
  Verified against the real docker-compose container, not simulated: the
  container was stopped, `Open` was called, the container was restarted
  ~1.5s into the retry window, and `Open` returned successfully instead of
  failing on its first ping — confirmed by measuring that it actually took
  over a second, not that it merely didn't error.
- **hook** — the thin client Claude Code's `PostToolUse` hook actually
  invokes: forward stdin to the worker's socket, exit. Deliberately does
  *no* observation work itself — see "Why the worker/hook split" below.
- **start** — idempotent daemon launcher for `SessionStart`: spawns a
  detached worker if one isn't already running, using a `worker-spawn-gate.ts`-style
  lockfile so concurrent sessions starting at once don't spawn duplicates.
- **context** — the other `SessionStart` hook, and the piece that makes
  this project actually function as *memory* rather than an on-demand
  search tool: it looks up the current project's most recent observations
  and injects them as context Claude sees automatically, before anyone asks
  for anything. Verified against a real session, not just unit-tested: a
  distinctive marker was seeded directly into the database, and a real
  `claude -p` session — with no tools, asked only about its own injected
  context — correctly reported it back verbatim.
- **prompt-context** — the `UserPromptSubmit` hook: embeds the actual
  submitted prompt text and injects the semantically closest observations
  — sharper than `context`'s static recent-observations dump, since it
  responds to what's actually being asked rather than just "what happened
  lately." Real claude-mem's own gap of the identical shape (`session-init`
  does the same "embed the prompt, semantic-search, inject" against its
  own server-backed store). Skips prompts under 20 characters (too short
  to embed meaningfully) and disables entirely with `-embed-model ""`, the
  same convention `mcp`/`stop` use. See "`UserPromptSubmit`" below for how
  this was verified against a real Ollama call and a real `claude` session.
- **file-context** — the `PreToolUse` hook (matcher `Read`): real
  claude-mem's own per-file recall, distinct from `context`'s per-project
  recall. Looks up prior observations that mention the specific file about
  to be read (via `files_read`/`files_modified`) and injects them before
  the read happens. Verified against a real session the same way `context`
  was: a distinctive marker seeded directly into the database, and a real
  `claude` session — asked to read that exact file — correctly reported the
  injected context back.
- **stop** — the `Stop` hook: synthesizes everything recorded during one
  session into a single `type=summary` observation (real claude-mem's
  "summarize" step). Idempotent the same way ingestion is — the key is the
  session_id alone, so a session that ends more than once (or a Stop that
  fires twice) still gets exactly one summary, verified by running it twice
  against the same real session and confirming the second call recognized
  the duplicate and did nothing. Now embeds the summary too (a real,
  previously-undiscovered gap: this hook had no `-embed-model` flag at all
  and never called `SaveEmbedding`, so a session summary — arguably the
  single most information-dense observation this project ever
  produces — had been invisible to `semantic_search_observations` from the
  day this hook was written, findable only by keyword search or listing.
  Found by pattern-matching against the identical gap `add_observation`
  had). Verified against a real session: seeded observations, ran `stop`
  with the real embed model, and confirmed the resulting summary was
  found by `semantic-search` using a query with zero keyword overlap.

  A real, more consequential race found by an actual full-stack
  integration test (two genuine concurrent `claude` sessions, run
  through real hooks pointed at explicit isolated paths — not a
  synthetic unit test): `Stop` fires the instant a session ends, but
  `PostToolUse`'s own observation for the session's LAST tool call is a
  real, fire-and-forget async LLM call the worker daemon runs in the
  background, taking several real seconds. A single, immediate database
  read raced ahead of it — reproduced directly, one real session's `Stop`
  hook found *zero* observations 4 seconds before the worker finished
  persisting the one observation that session actually had, silently
  skipping the summary entirely. In a longer session, the identical race
  would instead silently produce a summary missing just its most recent,
  often most contextually important, action. Fixed by polling until the
  count stops growing across consecutive checks (up to ~12s, costing
  nothing perceptible since `Stop` already runs fire-and-forget) rather
  than trusting a single read — but the *first* version of that fix had
  its own real bug, caught live before it shipped: treating two
  consecutive zero-reads as "confirmed empty" is wrong, since a session
  with exactly one real tool call also reads zero on every check until
  the observer call actually finishes, which routinely took longer than
  one poll interval. Only a count that has gone *positive* and then
  stops growing is real evidence of stability; a genuinely tool-call-free
  session now correctly pays the full wait budget instead of exiting
  early on a false stabilization. Verified with dedicated tests proving
  each specific failure mode (false-positive stabilization at zero, a
  perpetually-growing count that must still terminate, a genuinely-empty
  session paying the full budget) and by re-running the original two
  real concurrent sessions twice more — the first re-run still found
  the flaw, the second, after fixing it, correctly summarized both.

  A *third* real bug, found the same way but against the Postgres
  backend's own full-stack test with a single session running TWO real
  tool calls back to back: even the corrected "two consecutive matching
  non-zero reads = stable" rule was still wrong, because observations
  for one session are processed *sequentially* — one worker mutex
  serializes turns per session (see `worker/sessions.go`) — so the count
  can sit at 1 for several real seconds while the second tool call's
  observation is still mid-flight, and two quick matching reads during
  that plateau falsely "confirm" stability. Reproduced live down to the
  timestamp: the first tool call's observation landed, then the second
  started six seconds later on the same `session_id`, and `Stop` fired
  its summary in between, producing "from 1 observations" instead of 2.
  Fixed by replacing the 2-check rule with a streak counter requiring 10
  consecutive matching non-zero reads (~9s of confirmed no-growth,
  chosen well above a single observation's own observed ~5-8s latency)
  before trusting the count, with the overall wait budget raised from
  12 to 45 polls to accommodate. This narrows the race further but is
  still explicitly a heuristic, not a guarantee — there is no way for
  this hook to know for certain that *every* `PostToolUse` event for a
  session has finished, only to infer it from the count holding steady
  long enough; a fully robust fix would need the worker to expose real
  per-session "is a turn still in flight" state for this hook to query
  directly, which is a legitimate architectural follow-up, not something
  this fix attempts. Verified with a new dedicated test reproducing the
  exact plateau shape (a count that holds at a stale, lower value for
  several checks — fewer than the required streak — before the real
  next observation lands), confirmed as a genuine regression test by
  temporarily reverting the streak threshold back to 2 and watching it
  fail, then restoring it.

  That "legitimate architectural follow-up" is now built: the worker
  daemon exposes real per-session in-flight state over its own socket
  (a small plain-text `INFLIGHT <session_id>` query alongside the
  existing fire-and-forget hook-forwarding protocol, answered
  synchronously — see `hook.QueryInFlight`/`worker`'s `inflightTracker`),
  and `Stop` queries it directly instead of only ever inferring from a
  row count. This closes the gap two ways: it exits fast once the worker
  confirms nothing is left in flight (no more waiting out a long
  row-count streak once the real answer is already known), and — the
  half that actually matters for correctness — it *extends* the wait
  ceiling from ~45 seconds to five minutes once the worker confirms real
  activity, but only then, so a genuinely tool-call-free session still
  finishes in the original ~45s. That extension is not cosmetic: a live
  re-verification run of the previous fix hit a real single observation
  that took 104 seconds — longer than the old ceiling — which would have
  been cut off mid-flight, summarizing an incomplete session, without a
  live signal justifying the wait. A row-count streak reaching its own
  "stable" threshold no longer overrides the worker's own "still busy"
  answer, either — an override this fix specifically needed, verified by
  briefly disabling it and confirming a test failure it exists to catch
  (the old heuristic locking in early despite the worker reporting
  ongoing work). Verified live end to end with the actual compiled
  binary: a real worker daemon, a real two-tool-call `claude` session,
  and `Stop`'s own log correctly reporting "from 2 observations" via the
  new query path, not the row-count fallback.
- **ingest** — one-shot: read a real transcript file, observe N tool calls,
  persist them. Useful for backfilling or testing without wiring up hooks.
- **search** / **semantic-search** — keyword (FTS5) and meaning-based
  (local embeddings + cosine similarity) search over what's been persisted.
  `-project` scopes to one project; the default (empty) searches every
  project in the store, since these are ad-hoc CLI lookups run by a human
  who may genuinely want that. `search` also takes `-type` to filter to
  one observation type (`discovery`/`change`/`decision`/`summary`/
  `manual` — the actual, small, fixed vocabulary the observer itself ever
  writes) and `-offset`, to page past a prior call's `-limit` — the same
  `obs_type`/`offset` filters real claude-mem's own search tool has.
  `-offset` was skipped in an earlier pass on a rationale that never
  actually applied to it: real claude-mem also has a date-range filter and
  a sort-order option this project genuinely doesn't port since neither
  maps onto an existing column without a real schema/query redesign, but
  offset needed no column at all — a plain `LIMIT`/`OFFSET` on the
  existing query, the same shape `RecentByProject` and every other
  paginated read here already use for `limit`. Both backends' `Search`
  now order by rank (bm25/`ts_rank_cd`) THEN `id`, not rank alone: a rank
  tie between two rows is real (identical term-frequency shape scores
  identically), and pagination via `LIMIT`/`OFFSET` needs a fully
  deterministic order or two calls at different offsets could return the
  same row twice or skip one, depending on whatever arbitrary order the
  database happens to visit tied rows in. Verified against both real
  backends (including the live Postgres container) with a dedicated test
  seeding 5 matching rows and confirming 3 pages of size 2/2/1 are
  disjoint and together cover every seeded row exactly once — not just
  "offset changes the result," which a non-deterministic order could also
  produce by accident.
  Threading `-type` through both backends (separately from offset, added
  earlier) found a real latent footgun in the
  Postgres implementation's placeholder numbering: `project`'s own scope
  clause hardcoded `$3`, assuming it was always the third argument when
  present — correct only because there was never a second optional
  filter to disturb that assumption. Rewritten to build placeholder
  numbers dynamically as each optional filter is appended, verified with
  a dedicated test against the live container that applies `project` and
  `type` together (the exact combination the old scheme couldn't have
  handled safely if it silently drifted).
- **mcp** — an MCP server (stdio, JSON-RPC 2.0) exposing ten tools any MCP
  client — including Claude Code itself — can call directly:
  `search_observations` (now also takes `type` and `offset` — see the CLI
  `search` entry above) and `semantic_search_observations` (keyword and
  meaning-based search), `recent_observations`, `session_observations`,
  and `file_observations` — the same `RecentByProject`/`BySessionID`/
  `ObservationsForFile` reads `SessionStart`, `Stop`, and the `PreToolUse`
  file-context hook already push automatically, now reachable on demand
  instead of only ever happening for you — and `add_observation`, this
  server's only *write* tool: everything else only ever surfaces what
  `PostToolUse` already captured automatically from a tool call; this lets
  Claude explicitly persist something worth remembering that isn't the
  direct result of one (a decision, a stated preference) — the same real
  gap real claude-mem's own `observation_add` tool closes. Idempotent the
  same way automatic capture is: `ContentHash(SessionID, "manual", title,
  narrative)` means calling it twice with the same title/narrative in the
  same session is a no-op, not a duplicate. `SessionID` here is generated
  once per MCP server process (Claude Code spawns one per session, so this
  is the natural per-session scope an MCP tool call has no other way to
  carry). Also embeds the new observation the same way automatic capture
  does (when an embed model is configured) — the first version of this
  tool didn't, a real gap that made a manually-added observation invisible
  to `semantic_search_observations` even though `search_observations`
  found it fine; fixed and locked in with a real Ollama-backed test (skips
  cleanly when Ollama isn't reachable, the same pattern `postgres_test.go`
  uses for a missing container). Wire format confirmed against a real
  `claude` session, not assumed from the spec (see `mcpserver/`'s doc
  comment); every tool's end-to-end call verified against the real CLI,
  not just unit-tested — including a live plugin install where a manually
  added observation was found afterward through `semantic_search_observations`
  by meaning, not keyword overlap.
  Scoped to the current project by default (derived from the server
  process's cwd) — this store is one shared database across every project
  ever recorded on the machine, so an unscoped search is a real
  cross-project leak, not just a ranking nuisance; found via a Postgres
  test flake (accumulated rows from unrelated projects crowded a fixed
  `LIMIT`), fixed at the `store.Backend` interface level so both backends
  and the CLI got it too. Pass `all_projects: true` to a search tool call
  to search everything on purpose, or `project: "..."` to `recent_observations`/
  `file_observations` to look at a different single project.
  Also **`get_observations`** — fetch full details (narrative, facts,
  concepts, files) for specific observation IDs, e.g. the `[id]` shown in
  any list-shaped tool's abbreviated output. Every other tool's list format
  deliberately shows only title/subtitle to keep results short (compared
  against real claude-mem's own `get_observations`, which exists for the
  identical reason: theirs is "step 3, fetch full details for filtered
  IDs" after their `search`/`timeline` steps). Backed by a new
  `Backend.ByIDs` method in both storage backends — SQLite via a
  hand-built `IN (?,?,...)` placeholder list (`database/sql` gives it no
  way to bind a whole slice as one placeholder), Postgres via pgx's native
  `= ANY($1)` support for a plain `[]int64` argument, verified against the
  live Docker container. Unknown IDs are silently omitted, not an error.
  Scoped to the current project the same way `search_observations` is —
  without that, a caller could read another project's observations just by
  guessing or iterating IDs, the same cross-project leak class fixed for
  `Search`/`SemanticSearch` earlier. Verified end to end against a real
  `claude` CLI session (a live MCP tool call, not just a unit test) that
  fetched a seeded observation's full narrative/facts/concepts by ID.
  `ByIDs` is also capped at `store.MaxIDsPerLookup` (100) per call, found
  the hard way rather than anticipated: a real test against this project's
  own SQLite driver showed 100,000 IDs failing outright with a raw
  "too many SQL variables" driver error, since the hand-built
  `IN (?,?,...)` placeholder list has no cap of its own. Enforced in both
  backends (Postgres's `= ANY($1)` doesn't hit the same driver limit, but
  gets the identical bound anyway, for parity), and confirmed at the MCP
  protocol boundary that an oversized request comes back as a clean
  `isError` tool result mentioning the limit, not a raw driver error
  leaking through.
  Also **`timeline`** — chronological context AROUND one observation
  (`depth_before`/`depth_after` observations immediately surrounding an
  anchor), mirroring real claude-mem's own `timeline` tool ("step 2: get
  context around results"). Answers a different question than
  `recent_observations`/`session_observations`: not "what's recent" or
  "what happened this session," but "what surrounds this ONE specific
  observation" — the read path a search result in isolation can't answer
  on its own. Give it an `anchor` (an observation ID) directly, or a
  `query` to resolve one automatically via a single-result keyword search,
  the same convenience real claude-mem's version offers. New
  `Backend.Timeline` method in both backends, ordered by `id` (not
  `created_at_epoch` like every other query) since id increases
  monotonically with insertion order in both SQLite's rowid and Postgres's
  `BIGSERIAL` — confirmed true in both, not assumed. Always scoped to the
  anchor's own project rather than trusting the caller's project argument
  at face value, closing off the same class of cross-project leak fixed
  for `Search`/`ByIDs` earlier — verified with a dedicated regression test
  seeding two projects with numerically-adjacent IDs and confirming the
  timeline never crosses the boundary. Verified end to end against a real
  `claude` CLI session for both the direct-anchor and query-resolution
  paths: seeded a real chronological sequence, asked for context around
  one specific item, and got back exactly its true neighbors with the
  anchor correctly marked.
  `Backend.Timeline` also now clamps `depthBefore`/`depthAfter` to
  `[0, MaxTimelineDepth]` at the store layer itself, not just in
  `mcpserver.go`'s own caller — found the hard way that a negative depth
  isn't just "no results": SQLite's `LIMIT` treats a negative value as
  *unlimited*, so `Timeline(..., -1, -1)` returned every row before the
  anchor instead of zero. Postgres fails differently for the identical
  root cause (`LIMIT must not be negative`, a real driver error) rather
  than silently returning everything, but both are wrong outcomes for a
  method whose whole contract is "a bounded window around one row." The
  live `timeline` MCP tool was never actually exposed to this (its own
  caller already treats `<=0` as "not specified" and substitutes the
  default of 3), so this is a defense-in-depth fix for `Backend.Timeline`
  as a public API — any future caller that skips that substitution and
  passes a negative depth straight through would otherwise hit this.
  Verified against both real backends with a dedicated regression test.
  Also **`observation_context`** — the on-demand form of `prompt_context.go`'s
  `UserPromptSubmit` hook, matching real claude-mem's own tool of the same
  name. That hook's semantic recall against the actual submitted prompt
  was, before this, the last read capability in this project with no
  on-demand MCP equivalent — every other hook-only read (including
  `context.go`'s own `SessionStart` dump, via `RecentByProject`) already
  got a tool this session (`recent_observations`/`session_observations`/
  `file_observations`). Unlike `semantic_search_observations`, which is
  closest in shape (same
  embed-the-query-then-`SemanticSearch` pipeline) but returns a list of
  results for a caller to interpret, this returns the exact same
  pre-formatted, ready-to-inject text block the hook produces
  automatically — a duplicated formatter, not a shared import (`mcpserver`
  can't import `cmd/claude-mem-go`, which itself imports `mcpserver` for
  the `mcp` command; importing it back would be a cycle), verified
  byte-for-byte against a real embedded observation found purely by
  meaning. Also confirmed end to end against the live Postgres container
  through the compiled binary itself (`add_observation` then
  `observation_context` in the same real session), not just the SQLite
  path `mcpserver_test.go` exercises.
  Also **`important_workflow`** — matches real claude-mem's own
  zero-dependency, static-text tool of the same name and shape: it never
  touches the store at all, existing purely to teach an MCP client the
  intended `search_observations` → `timeline` → `get_observations`
  pattern (narrow to a few IDs before ever paying for full detail, not
  the other way around — real token cost this project's whole
  abbreviated-list convention exists to protect). Registered first in the
  tool list, same as real claude-mem's, so its terse description is the
  first thing a client sees even before calling anything. Verified end to
  end against the live Postgres container through the compiled binary,
  not just the unit test.
  Building this also caught a real, unrelated staleness bug found by hand
  while checking the live `initialize` response: `serverInfo.version` had
  been hardcoded to the literal `"0.1.0"` since early in the project and
  never updated across several real version bumps since — three releases
  stale by the time this was noticed. Fixed by deriving it from Go's own
  VCS build info instead (the same source `version`/`doctor` already
  use), so it can't go stale again the same way. Confirmed live: the
  compiled binary's `initialize` response now reports the real build
  commit, matching `git rev-parse HEAD` exactly.
- **skills/mem-search** — a real Claude Code skill (`/mem-search`) teaching
  Claude when to reach for `search_observations` vs.
  `semantic_search_observations`. Validated with `claude plugin validate
  --strict`, and verified live: installed the plugin, ran `/mem-search
  claude-mem installation` in a real session, and confirmed via the MCP
  server's own log that a genuine `tools/call` fired — not a hallucinated
  answer.
  Went stale as the MCP surface grew past when this skill was first
  written — found by hand rather than assumed current: it still said
  "eight tools" and never mentioned `observation_context`,
  `important_workflow`, or `search_observations`'s `offset` argument, all
  added later the same session. The skill whose entire job is teaching
  which tool to reach for is the one place staleness here actually
  matters. Updated to document all ten, verified live the same way as the
  original: `--plugin-dir` loading this plugin into a real `claude -p`
  session and asking it to list every MCP tool name the skill mentions —
  all ten came back, confirming the updated file is what a real session
  actually reads, not just that the markdown parses.
- **skills/mem-doctor** — a second skill (`/mem-doctor`) surfacing the
  `doctor` health check *inside* a Claude Code session instead of only from
  a raw terminal — "is memory actually working" shouldn't require dropping
  out of the conversation to find out. Verified the same way as
  `mem-search`: installed the plugin at project scope in a throwaway
  directory, ran `/mem-doctor` in a real session, and got the real health
  check's own output back (worker/database/Ollama status), confirming
  `$CLAUDE_PLUGIN_ROOT` resolves correctly for a skill-invoked command, not
  just for hooks and the MCP server.
  Went stale the same way `mem-search` did, caught in the same pass:
  never mentioned `doctor`'s `-hnsw-ef-search` flag or the
  `hnsw_ef_search` health field it reports, both added earlier the same
  session as this skill's own last edit. Updated, and verified the same
  live way: `--plugin-dir` loading this plugin into a real `claude -p`
  session and asking it to list every distinct thing the skill says
  `doctor` checks — `hnsw_ef_search` came back among them.
- **skills/mem-prune, skills/mem-export, and skills/mem-reembed** —
  surface `prune`, `export`/`import`, and `reembed` as
  `/mem-prune`/`/mem-export`/`/mem-reembed` the same way `mem-doctor`
  surfaces `doctor`. `mem-prune`'s instructions are written to treat this
  as the one genuinely destructive operation in the CLI: always run the
  dry run first, show the count, and get explicit confirmation before
  ever adding `-yes` — verified live, not just written and hoped for:
  asked a real session to "clean up memories older than 1 day" and
  confirmed it ran the dry run, reported the count, and asked whether to
  actually delete rather than doing so on its own. `mem-reembed`'s
  instructions apply the identical dry-run-first discipline for a
  different reason: not destructive, but a real Ollama API cost per row.
- **doctor** — an operational health check: is the `claude` CLI on `PATH`,
  is the worker daemon reachable, is the database reachable, is Ollama
  reachable with the configured model actually pulled. Distinguishes
  critical failures (exit 1 — nothing works without these) from
  informational ones (worker not running is fine, `start` launches it
  lazily; no Ollama just means no semantic search). Verified against real
  failures, not just the happy path: a genuinely unreachable Postgres DSN
  correctly exits 1, and an unpulled Ollama model correctly downgrades to
  a warning rather than a failure. Also surfaces the worker's own activity
  (see below) when available, and each backend's own operational details
  via a new `Backend.HealthDetails()` method: SQLite reports the PRAGMA
  settings actually in effect on the connection (`journal_mode`,
  `foreign_keys`, `busy_timeout_ms`) — confirming the WAL/FK fix genuinely
  took effect, not just that it was requested in the DSN. Postgres reports
  real connection-pool utilization (confirming the bounded pool actually
  applies), the pgvector extension's installed version, and whether the
  HNSW index `SemanticSearch` depends on for real ANN search still
  exists — a schema drift would otherwise silently degrade every semantic
  search to a full table scan with nothing here ever saying so. Verified
  against both real backends, not fabricated values: SQLite showed
  `journal_mode=wal foreign_keys=1 busy_timeout_ms=5000`; the live Postgres
  container showed a real `vector_extension=0.8.6` and
  `hnsw_index_exists=true`.
  `HealthDetails` also now reports `embedding_dims`/`embedding_dims_consistent`
  — surfacing a real, previously-silent failure mode: if the configured
  Ollama embedding model ever changes (different dimension count),
  `SemanticSearch`'s cosine similarity returns -1 (its theoretical
  minimum) on any length mismatch rather than erroring, so the *old*
  embeddings just quietly stop ever matching a new-model query, forever,
  with nothing anywhere saying so — a `doctor` run is now the one place
  that actually surfaces it. This is SQLite-specific in practice: Postgres
  reports the identical key for parity, but its fixed `vector(N)` column
  type makes a real mismatch structurally impossible (confirmed directly —
  saving a wrong-dimension vector there fails loudly with a real Postgres
  error instead). Verified both ways: a real `doctor` run against a
  deliberately mixed-dimension SQLite database showed
  `embedding_dims=384:1,768:1 embedding_dims_consistent=false`, and a real
  attempt to save a mismatched vector into the live Postgres container was
  rejected outright.
  `HealthDetails` also now reports `hnsw_ef_search` — a real observability
  gap once `-hnsw-ef-search` existed with nowhere confirming it was
  actually configured. `doctor` gained the identical `-hnsw-ef-search`
  flag so it can be pointed at the same override an operator set on
  `mcp`/`semantic-search`/`prompt-context`, and reports it back verbatim
  (or `default (40)` when unset). Reports this `Store`'s *configured*
  value, not a live Postgres session setting — there isn't one to read,
  since `SemanticSearch` applies it per call via a transaction-scoped
  `SET LOCAL`, not a persistent session GUC. Verified against the live
  container: a real `doctor -hnsw-ef-search 333` run showed
  `hnsw_ef_search=333`, and the unset default showed
  `hnsw_ef_search=default (40)`.
- **version** — prints the exact commit and build time via Go's own
  `runtime/debug.ReadBuildInfo()` (VCS stamping is on by default since Go
  1.18 — no ldflags wiring, no version file to keep in sync, no CI change
  needed), including a `-dirty` marker if the tree had uncommitted changes
  at build time. There was no way to answer "what build is this" at all
  before this — no version flag, no way to correlate a bug report with an
  exact build. `doctor`'s header prints the same string. This is also
  `cmd/claude-mem-go`'s first test file — every other package already had
  coverage; this one didn't.
- **export** / **import** — the store's only backup and migration story;
  there was no way to get data out of this store at all before this, and
  no way to move data between the SQLite and Postgres backends. `export`
  writes every observation as JSON Lines (paginated internally via
  `Backend.ExportAll`, so a very large store doesn't need to fit in memory
  at once); `import` reads that file back through `Backend.ImportRow`,
  preserving each row's original `content_hash` (the same idempotent-dedup
  guarantee `Insert` provides — importing the same file twice, or restoring
  on top of data that's already there, skips rows already present instead
  of duplicating them) and its original timestamp (a restore reflects when
  things actually happened, not when they were re-imported). Since both
  backends implement the same `Backend` interface, `export` from one and
  `import` into the other is the SQLite<->Postgres migration path — verified
  with real data, not just unit tests: exported this project's own real
  32-observation dev database, imported it into the live Postgres
  container, confirmed the rows searchable there, then re-ran the same
  import and confirmed it correctly skipped all 32 as already present.
  `ExportRow` also carries each row's embedding, if it had one — the first
  version didn't, a real gap the same "does every write path do what the
  others do" check that caught the `add_observation`/`Stop` embedding
  bugs also caught here: without it, migrating to Postgres for real ANN
  search at scale would have arrived with nothing left to search. Fixed
  and verified against this project's own real dev database (38 of 46
  observations carried an embedding; all 38 survived a round trip into a
  completely fresh SQLite file) and against the live Postgres container
  with a real embedding value, not a fabricated one.
- **prune** — deletes observations older than a cutoff; there was no
  retention story at all before this, meaning the store only ever grows.
  Dry-run by default (`-older-than-days N` alone just reports a count);
  `-yes` is required to actually delete, and `-project` scopes it to one
  project instead of every project in the store. Finding this gap surfaced
  a real, previously-latent bug: the FTS5 `observations_ad` trigger used
  the fts5 "special command" delete syntax, which is only valid for
  contentless/external-content tables — this table is neither, and the
  trigger had silently never been exercised because nothing had ever
  deleted a row before `prune` existed. The first real `DELETE` hit it
  immediately with a genuine SQL error, reproduced through both the Go
  driver and the plain `sqlite3` CLI. Fixed via a new numbered migration
  (not just fixing the schema definition, which would only help brand-new
  databases) — verified against the exact upgrade scenario: a simulated
  already-migrated database with the original broken trigger correctly
  gets fixed on reopen, and `prune` then works against it. Also tested
  against a live Postgres container (no shadow table there, but still
  verified the generated `search_vector` and `embedding` columns are
  genuinely gone after a prune, not just the row). A second, more serious
  bug shipped in the same original commit and was caught the next
  iteration: `created_at_epoch` is stamped in **milliseconds**
  (`now.UnixMilli()`, both backends), but `cmdPrune`'s cutoff was computed
  in **seconds** (`time.Now().AddDate(...).Unix()`) — a ~1000x mismatch
  that made `created_at_epoch < cutoff` false for every row that ever
  existed, so `prune` silently deleted nothing, ever, for any real
  `-older-than-days` value. Every existing `Prune` unit test had backdated
  rows by hand to small, unit-agnostic numbers, so none of them could have
  caught a caller using the wrong unit; the regression test that closes
  this (`TestPruneCutoffUnitsMatchInsertsRealTimestamp`, both backends)
  inserts through the real `Insert` path instead, and the CLI fix was
  re-verified against a real backdated row through the actual binary.
- **reembed** — the remediation half of `doctor`'s
  `embedding_dims_consistent` finding: detecting a stale/missing embedding
  was one thing, but there was no way to actually fix it short of
  re-ingesting from scratch. Finds every observation with no embedding at
  all, or one whose stored dimension doesn't match the currently
  configured model's real dimension (learned via a probe embed call —
  nothing here maintains a model-name-to-dimension lookup table), and
  re-embeds it. Dry-run by default like `prune` (real Ollama API cost per
  row, even though nothing is ever deleted), `-yes` to actually do it,
  `-project` to scope it. `doctor` itself now does the same probe-and-
  check: `embedding_dims_consistent=true` alone only catches internal
  disagreement between *stored* embeddings — a store embedded entirely
  under a since-replaced model would report "consistent" while every
  single embedding is silently unsearchable under the model that's
  actually live right now, which the plain histogram check can't see.
  Verified end to end with real Ollama calls, not mocked: seeded one
  observation with a stale 384-dim embedding and one never embedded at
  all, confirmed `doctor` flagged both the internal-consistency case AND
  (separately) the live-model mismatch, ran `reembed -yes`, confirmed
  both fixed, and confirmed via `semantic-search` that the previously
  stale observation is now actually findable and correctly ranked.
- **Observability** — the worker daemon's only introspection used to be
  raw log lines (`worker.log`, and the per-hook logs). It now also writes
  a small `~/.claude-mem-go/worker-stats.json` snapshot after every
  processed event: counts of observations persisted / deduped / failed at
  each stage (observer, insert, embedding), plus live pool utilization
  (`pool.InFlight()`/`Capacity()`) and cached-session count. `doctor` reads
  and prints it when present. Verified against a real running daemon, not
  just unit tests: restarted the worker with the instrumented binary, sent
  it a real `PostToolUse` payload over its actual Unix socket, and
  confirmed both the stats file and `doctor`'s output reflected the real
  persisted observation. The worker also optionally serves the same data
  in Prometheus text exposition format — `-metrics-addr 127.0.0.1:9090`
  (passed to `start` too, which forwards it to the worker it spawns)
  starts an HTTP listener at `http://<addr>/metrics`; empty (the default)
  disables it entirely, since this is the one thing about this daemon that
  listens on more than a Unix socket. Verified against a real running
  daemon: started one with `-metrics-addr` set, curled `/metrics` before
  and after sending it a real event over its actual socket, and confirmed
  `claude_mem_go_worker_processed_total` went from 0 to 1.
- **Embedding retries once on a transient failure** — `classify`/`worker.go`
  already retry the main observer call once on a transient/rate-limit
  failure, but `embed.Client.Embed` had no equivalent: a single network
  hiccup against Ollama (briefly unavailable, momentarily overloaded)
  failed that observation's embedding permanently. Not catastrophic
  (`worker.process` already treats embedding failure as additive-only —
  keyword search on the row still works), but it meant semantic search
  silently and permanently missed observations on any brief Ollama blip a
  retry would have recovered from. Now retries once on a network-level
  error or a 5xx status; does *not* retry a 4xx or a successful-but-empty
  embedding (the model genuinely isn't pulled, and won't be moments
  later) — verified against a real Ollama call in addition to the new
  unit tests.

## Installing as a Claude Code plugin

`.claude-plugin/plugin.json` + `.claude-plugin/marketplace.json` +
`hooks/hooks.json` + `.mcp.json` make this a real, installable Claude Code
plugin — not just something wired by hand-editing `.claude/settings.json`.
Validated with the real CLI, and installed/exercised end to end at
**project scope** (never user/machine-wide — that would affect every other
Claude Code session on the box, not just a test):

```sh
go build -o claude-mem-go ./cmd/claude-mem-go
claude plugin validate .                                          # manifest sanity check

# From inside a project you want claude-mem-go active in:
claude plugin marketplace add /path/to/claude-mem-go --scope project
claude plugin install claude-mem-go@claude-mem-go-local --scope project
```

`SessionStart`, `PostToolUse`, and the MCP server were confirmed live
through this exact mechanism (not `--mcp-config`/manual settings):
`SessionStart` starts the worker and injects context, `PostToolUse` reaches
the worker and persists a real observation, and `search_observations`
returns real rows through the plugin-bundled `.mcp.json` — all in one
project-scoped install/uninstall cycle, cleaned up afterward. `Stop` (session
summarization) is wired into the same `hooks/hooks.json` and was verified
directly against real, already-persisted session observations rather than
re-run through a full plugin install cycle.

### Releases

Installing used to mean `go build` locally, every time, with no other
option. `.goreleaser.yaml` + `.github/workflows/release.yml` close that gap:
pushing a `vX.Y.Z` tag cross-compiles `claude-mem-go` for
linux/darwin × amd64/arm64 (`CGO_ENABLED=0` — a real constraint, not a
default left in place: both `modernc.org/sqlite` and `pgx/v5` are pure Go,
this project's whole reason for choosing them, so nothing here needs a C
toolchain per target) and attaches the archives plus a `checksums.txt` to a
real GitHub Release. Release notes are `CHANGELOG.md` itself, not an
auto-generated commit dump — this project already maintains one by hand
for exactly this reason.

Verified locally with a real snapshot build (`goreleaser release
--snapshot --clean --skip=publish`, no tag or publish needed) — all four
targets actually compiled, and the darwin/arm64 archive's binary was
extracted and run for real: `version` printed a correct commit/build-time
string (Go's own VCS stamping — see `version.go` — needs no ldflags
wiring, cross-compiled or not), and `doctor` ran its full real checklist
against a fresh temp database. The workflow YAML passed `actionlint`
before being committed, the same discipline `ci.yml` was checked with.

Deliberately **not yet wired into `hooks/hooks.json`** — that still points
at a locally-built `$CLAUDE_PLUGIN_ROOT/claude-mem-go`, and switching it to
fetch a matching release asset automatically (a `Setup`-hook
download-if-missing step, the same shape real claude-mem's own `Setup`
hook covers for its version-check) is a real, separate change that
deserves its own verification pass rather than riding along with "does the
release pipeline produce working binaries at all."

### Why the worker/hook split

An earlier version had the `PostToolUse` hook call the observer directly.
Testing that against a real Claude Code session showed it losing
observations: `"async": true` only means Claude Code doesn't wait for the
hook — it does **not** mean the hook's child process survives the
invoking `claude` process exiting. Both attempts died mid-observation. The
fix is what's here: a daemon started once and left running (detached via
`Setsid` so it survives its own launcher exiting too), with hooks doing
nothing but a fire-and-forget local socket write.

Both sides of that socket write are now bounded (`hook.MaxPayloadBytes`,
8MB — matching the MCP server's own JSON-RPC line cap): `hook.Forward`
rejects an oversized payload outright rather than sending it, and the
worker daemon's own socket read enforces the identical bound as
defense-in-depth. Without this, an abnormally large `tool_response` (a
`Bash` command that cats a multi-gigabyte file, a `Read` of a huge log)
had no upper bound at all — a real risk specifically because the worker
is one long-lived process every project on the machine shares, so a
single pathological tool call could balloon its memory for every other
session using it too, and would otherwise get stringified verbatim into
an observer prompt at a real per-token API cost. An oversized payload is
rejected whole, not truncated — a truncated JSON hook payload is corrupt,
not just short, so there's no safe partial-forward here. Verified against
a real running (isolated, throwaway) daemon: sent an 8MB+100-byte payload
over its actual socket, confirmed the daemon logged a rejection and
stayed alive, then sent a normal-sized payload through the same daemon
and confirmed it processed normally afterward.

Bounded in bytes, but not in wall-clock time until a later pass found the
gap: `handleConn`'s read had no deadline at all, only the byte cap above —
a client that dials the socket and never writes or closes (a stalled
process, or a bug in some future caller not going through `hook.Forward`)
leaked that goroutine and its underlying file descriptor for as long as
the daemon ran, which is meant to be days. More relevant once the
`INFLIGHT` query protocol (see the Stop-hook section above) started
sharing this same socket as a synchronous request/response exchange, not
just the original one-way hook forward — the client side already set its
own deadline, but the server side never did. Fixed with a
`handleConnReadTimeout` (30s, generous enough that neither a normal hook
forward nor a real `INFLIGHT` query — both near-instant on a local Unix
socket — ever come close to it). Verified with a real `net.Conn` (not
mocked): a test shrinks the timeout, connects a client that deliberately
never writes or closes, and confirms `handleConn` actually returns once
the deadline elapses rather than hanging — confirmed as a genuine
regression test by temporarily removing the deadline call and watching
the test time out before restoring it.

### Truncation used to be able to corrupt real tool output mid-character

`transcript.Truncate` (and `Parse`'s internal `truncate`) caps every
`tool_input`/`tool_response` field at `FieldCap` (1500 bytes) before it
ever reaches an observer prompt — on the real, live hot path
`worker.Daemon.process` runs for every single tool call. The cut was a
plain `s[:max]` byte-offset slice, with no regard for where a multi-byte
UTF-8 rune actually starts or ends. Confirmed directly: any non-ASCII
character (an accented file path, an emoji, box-drawing characters from
`tree`/`ls` output, non-English text) that happens to straddle byte 1500
gets sliced in half, producing **invalid UTF-8** in the truncated
result — over the volume of real tool calls a long-running daemon
processes, "happens to straddle" is an eventual certainty, not a rare
edge case. Fixed by walking back to the nearest real rune-start byte
before cutting (at most 3 extra bytes trimmed, since the longest UTF-8
encoding is 4 bytes) — verified with a unit test confirming the
truncated result is always valid UTF-8 even when the exact cutoff is
engineered to land mid-character, and a real end-to-end run through an
isolated worker daemon: sent a real tool_response with multi-byte
characters straddling the exact 1500-byte boundary over the daemon's
actual socket, and confirmed the full real pipeline (truncate → observer
prompt → real `claude` subprocess → persisted observation) completed
cleanly with no encoding error anywhere.

### A negative limit, and the bigger gap it exposed: no panic recovery anywhere

Auditing every `Backend` method that takes a `limit` (prompted by
Timeline's own negative-depth fix, above) found the identical bug in
`Search`, `RecentByProject`, `BySessionID`, `ObservationsForFile`,
`ExportAll`, and `ObservationsNeedingEmbedding`: SQLite's `LIMIT` treats a
negative value as *unlimited*, confirmed by hand against a real seeded
database returning every row for `limit=-1` instead of zero. `SemanticSearch`
had a worse version of the same bug — it slices its own results in Go
(`all[:limit]`) rather than relying on SQL's `LIMIT`, so a negative limit
didn't return "everything," it **panicked outright** with a real "slice
bounds out of range" runtime error.

That panic mattered more than it might look: neither the worker daemon
nor the MCP server had *any* panic recovery anywhere. `SemanticSearch`
runs inside the worker's per-event goroutine (`handleConn`'s
`go d.process(...)`) and the MCP server's synchronous request handler —
an unrecovered panic in either one crashes the **entire process**, not
just the one call: the worker daemon serving every project on the
machine, or the whole `claude` session's MCP connection. Neither one was
actually reachable through the live MCP tool surface today (its own
caller already substitutes a default before calling any of these, the
same story as Timeline's fix), but "not reachable today" and "safe" are
different claims for a public `Backend` method and the two long-lived
processes built on it.

Fixed both layers: every affected method now clamps a negative limit to
0 in both backends (`clampNegativeLimit`, `LIMIT 0` already behaves
correctly — only negative values needed guarding), and — the more
consequential half — `worker.Daemon.process` and `mcpserver.Server.handle`
now both recover from a panic instead of letting it crash the process, an
independent backstop against *any* future bug of this shape, not just
this one. Verified thoroughly: a dedicated regression test per method
against both real backends (SQLite confirms zero rows instead of
unlimited; Postgres confirms a clean clamp instead of surfacing its own
real "LIMIT must not be negative" driver error), a fault-injection test
in each of `worker`/`mcpserver` (a fake `Backend` that panics on every
call, exercised through the real request-handling path, confirming the
process survives and a real caller gets a clean error instead of a dead
connection), and a live `claude` CLI session confirming the MCP server
stays alive and keeps serving requests correctly afterward.

Auditing every other caller-supplied integer while already in this
territory found one more, in `pool.New` this time: Go's own
`make(chan T, n)` panics with `"makechan: size out of range"` for a
negative `n`, so `worker -max-concurrent -1` (a mistyped or computed
flag) crashed the daemon at **startup**, before it ever bound its
socket — confirmed by hand against a real running worker process. Zero
has a quieter but equally real failure mode: a zero-capacity semaphore
can never be acquired, so every `Acquire` call would block forever
instead of crashing. `pool.New` now clamps anything below 1 to 1. Also
fixed a smaller, related observability bug this surfaced: the startup
log line printed the *raw* `-max-concurrent` flag value, not the pool's
actual (possibly clamped) capacity — `doctor`'s own `pool=X/Y` stats
already used the real `pool.Capacity()` and were never wrong, but the
one log line calling itself `max_concurrent=-1` while the pool was
really running at capacity 1 was a real, if cosmetic, inconsistency.
Verified against a real running worker: started one with
`-max-concurrent -1`, confirmed it stayed alive (rather than crashing)
and its own startup log correctly reported `max_concurrent=1`.

### The idle-session reaper could close a subprocess mid-turn

`worker.sessionCache` reuses one `claude` subprocess per session across
tool calls (cost and latency, see the package's own doc comment), torn
down after `sessionIdleTimeout` (10 minutes) of disuse by a background
sweep — a resource bound for a daemon meant to run for days. `lastUsed`
was refreshed when a turn *started* (`getOrCreate`), but nothing bounded
how long a single turn could *run*, and nothing refreshed `lastUsed` again
until the *next* turn started. This project has measured a real single
observation taking 104 seconds under normal load — comfortably enough
margin, on a busy daemon or a slow model response, for a turn to still be
genuinely in flight when the idle sweep looked at a now-stale timestamp
and decided the session was safe to tear down. The original `evictIdle`
had no awareness of `sessionEntry.mu` at all: it would call `Close()` on
the handle regardless of whether `worker.Daemon.process` was still
blocked inside `Observe`, reading that exact subprocess's stdout — a real
use-after/during-close hazard on the underlying pipes, not just a wasted
turn. Traced one level deeper into `claude-agent-sdk-go`'s own
`Session.Close`/`Send`: no synchronization between them either, so this
really would have raced two goroutines over one subprocess.

Fixed with `entry.mu.TryLock()` (not a blocking `Lock`) before evicting:
succeeding *proves* no turn is currently running, so it's genuinely safe
to close; failing means a turn is active right now, and the sweep simply
skips that session — safe to defer regardless of how long the turn takes,
since a session with a turn actively in flight isn't meaningfully idle no
matter what its timestamp claims. It's reconsidered on the next sweep a
minute later. A second, related fix closes the timestamp's other gap:
`lastUsed` is now also refreshed when a turn *finishes* (`touch`, called
from `process` after a successful `Observe`), not just when one starts —
without it, a session's idle clock was measured from turn-start, not
real last-activity time, which is exactly what let a long-but-legitimate
turn look artificially close to the idle window in the first place.

Verified with dedicated concurrency tests, not just logical review: one
drives `worker.Daemon.process`'s own critical section directly (acquiring
`entry.mu` and calling a handle whose `Observe` blocks on command) while
back-dating `lastUsed` to look idle, confirms `evictIdle` leaves the
in-flight session alone and its handle unclosed, then releases the
simulated turn and confirms a *later* sweep does evict it — proving the
fix defers eviction rather than leaking the session forever. Each new
test passes cleanly under `go test -race`, and each was confirmed as a
genuine regression test by temporarily reverting to the old unsafe logic
and watching it fail before restoring the fix. Also verified live against
the real compiled binary: temporarily shrunk `sessionIdleTimeout` to 3
seconds and the sweep interval to 1 second, then ran a real `claude`
session with real Ollama/observer calls — a genuine ~9-second real
observation spanned several sweep cycles inside the shrunk idle window,
and the daemon stayed alive with the observation correctly persisted, no
panic, no crash — the exact race this fix closes, exercised under real
production-shaped timing rather than only a synthetic unit test.

### The same gap existed on the shutdown path too

`closeAll` (used on daemon shutdown, `defer d.sessions.closeAll()` in
`Run`) had the identical bug `evictIdle` just had, found by hand on a
fresh look at the surrounding code: it called `evict` — no `entry.mu`
awareness at all — on every cached session regardless of whether a turn
was still genuinely in flight. Same use-after/during-close hazard,
different trigger (a real `systemd`/`launchd` restart or manual `kill`
mid-observation, instead of an idle timer).

The fix can't be identical, though: `evictIdle` can always defer an
in-flight session to the *next* sweep, but shutdown has no next sweep —
skipping it the same way would leak the subprocess as an orphan with
nothing left to ever clean it up. `closeAll` now *waits* (a real
`entry.mu.Lock()`, not a `TryLock`) for each in-flight turn to finish
naturally, up to a `closeAllGracePeriod` (5s — the same shape as this
daemon's own metrics HTTP server shutdown, `Run`'s
`metricsSrv.Shutdown` with an identical bounded `context.WithTimeout`);
if a turn hasn't finished within that window, it force-closes anyway
rather than blocking shutdown forever for a runaway turn — the one
accepted exception to "never close while a turn might be in flight,"
since the daemon is exiting either way. Every session's wait runs
concurrently, so total shutdown time stays bounded by the grace period
regardless of how many sessions are cached, not multiplied by each one.

Deliberately scoped to just this mutex-safety gap, not a full graceful-
shutdown redesign: a `handleConn`/`process` goroutine already dispatched
from `Run`'s `Accept` loop before shutdown began could still be racing
`getOrCreate` for a brand-new session concurrently with `closeAll`'s own
snapshot — a real, broader "drain in-flight requests before closing
anything" concern this fix doesn't attempt, documented honestly rather
than silently ignored or overclaimed as solved.

Verified with dedicated concurrency tests under `go test -race`: one
confirms `closeAll` doesn't close a handle until *after* a simulated
in-flight turn's own `Unlock()`, proving it genuinely waited; another
confirms a turn that outlives a (shrunk, for the test) grace period gets
force-closed anyway rather than hanging shutdown indefinitely. Both
confirmed as genuine regression tests by reverting to the old unsafe
logic and watching them fail before restoring the fix. Also verified
live against the real compiled binary: started a real detached worker,
forwarded a real hook payload, sent it a real `SIGTERM` while the
observation was genuinely in flight (confirmed via the worker's own log
timestamps), and confirmed the daemon shut down cleanly — no panic, no
hang, no orphaned process left behind afterward.

### `observations.type` had zero validation anywhere, in either backend

Nothing — not the schema, not the Go code — validated `type` against
this project's own small, fixed vocabulary (`discovery`/`change`/
`decision` from the real observer prompt, `summary` from the Stop hook's
session-summary prompt, `manual` hardcoded in `add_observation`). An
LLM's `<type>` tag drifting to an unrecognized value, or a corrupted/
hand-edited import file, would have silently persisted a row invisible
to any `-type`/`type` filter, with no error anywhere. Found by hand as a
real schema-completeness gap, not a demonstrated live bug — worth
checking before assuming it's purely theoretical: queried the actual
distinct `type` values across 3000+ real rows this project's own testing
has accumulated in its shared Postgres dev container, and every single
one already fell within the vocabulary. (While verifying the fix below —
a real, if minor, reminder that "checked once" isn't "stays true": the
break/restore verification step for the new tests briefly left two
rows with an invalid `type` in that same shared container, caught and
cleaned up immediately, not organic drift.)

Fixed at both real ingestion boundaries — `Insert` and `ImportRow`,
via their shared `insertRow` — with `store.ValidateObservationType`,
returning a clear error before either backend's own driver ever sees an
unrecognized value. For the Postgres backend specifically (the
production-scale backend this project's "schema completeness" mandate is
really about), also added a real `CHECK` constraint via a new migration
— a schema-level guarantee that holds regardless of which code path
ever writes a row, not just the ones that go through this Go package.
Since Postgres has no `ADD CONSTRAINT IF NOT EXISTS`, the migration wraps
it in a `DO` block checking `pg_constraint` first, so it stays idempotent
the way every migration here must be.

Verified thoroughly: dedicated tests in both backends confirming
`Insert`/`ImportRow` reject an unrecognized type and persist nothing,
each confirmed as a genuine regression test by temporarily disabling the
validation call and watching the test fail before restoring it. The
Postgres `CHECK` constraint itself was verified independently of the Go
code — dropped it by hand, confirmed a raw SQL `INSERT` with a bad
`type` succeeded (proving the drop genuinely took effect and Go-side
validation alone wasn't masking the test), then restored it and
confirmed the identical raw `INSERT` now fails with a real Postgres
constraint-violation error — the actual schema-level guarantee, not just
the application-level one.

### Excluding a project from automatic capture — a real feature gap this port had until now

Real claude-mem lets a user opt specific projects out of automatic
tracking entirely (`CLAUDE_MEM_EXCLUDED_PROJECTS`, a comma-separated
glob-pattern list checked at the top of every automatic hook handler —
`shouldTrackProject`/`isProjectExcluded` in
`src/shared/should-track-project.ts`/`src/utils/project-filter.ts`).
claude-mem-go had no equivalent anywhere: `worker.Daemon.process`
(automatic capture) and the `context`/`file-context`/`prompt-context`/
`stop` hooks (automatic recall/summary) all tracked unconditionally.
The only way to keep a sensitive, client-confidential, or scratch
project out of a shared memory database was to not install the plugin
at all — all-or-nothing, unlike real claude-mem's per-project opt-out.

Closed with a new `excludeproject` package and a `-excluded-projects`
flag on `worker` (forwarded from `start`, the same way `-model`/
`-embed-model`/`-max-concurrent` already are) and on each of `context`/
`file-context`/`prompt-context`/`stop` individually — matching real
claude-mem's own design of checking this at the top of every automatic
handler independently, not one central gate. A comma-separated list of
glob patterns (`*`, `**`, `?`, and a leading `~` for the home directory)
matched against both the full path and the directory's basename — a
project whose path or name matches is skipped entirely, logged as
`skip: project excluded (cwd=...)`, before any real work happens (for
`worker`, deliberately before ever spawning or reusing an observer
subprocess, so an excluded project never pays for an LLM call it's about
to throw away).

Deliberately ported to match real claude-mem's *exact* glob semantics
rather than inventing a new dialect — a user migrating an existing
`CLAUDE_MEM_EXCLUDED_PROJECTS` value should get identical matching
behavior here. Verified directly against the real TypeScript source, not
just re-derived from reading it: ran real claude-mem's own
`isProjectExcluded`/`globToRegex` functions through Node against the
exact same 19 test cases this port's own unit tests use, confirming
byte-for-byte identical results on every one — including the one
initially-surprising case (an empty path against a bare `*` pattern)
that turned out to be `shouldTrackProject`'s own separate `!cwd` guard
short-circuiting before `isProjectExcluded` is ever consulted, not a
divergence in the glob logic itself; this port's `IsExcluded` correctly
folds that same guard in, since it's the one function standing in for
both real claude-mem functions combined.

Verified live end to end, not just unit-tested: two throwaway projects
sharing one real running worker daemon, one with a directory name
matching an exclusion pattern and one without, each driven by a real
`claude` CLI session performing an identical `Read` tool call. The
matching project's `PostToolUse` event was skipped before any observer
call — confirmed via the worker's own log and, more directly, by
querying the real resulting database afterward: exactly one observation
existed, from the unmatched project, with the excluded project's
directory producing zero rows.

### `add_observation` had no size bound on any of its inputs

Every other external-input surface in this codebase is bounded somewhere:
`hook.MaxPayloadBytes` caps the hook socket, `store.MaxIDsPerLookup` caps
`get_observations`' id list, `maxLimit` caps every list tool's `limit`
argument. `add_observation`'s title/subtitle/narrative/facts/concepts
were the one exception — they arrive straight from an MCP tool call's
JSON arguments with no length check at all, and unlike `PostToolUse`'s
captured fields (already bounded by `transcript.FieldCap` before an
observation is even built), a manually-added observation's fields went
straight from the wire into a database row and, when semantic search is
enabled, an Ollama embedding request. A caller (accidentally or
otherwise) handing it a multi-megabyte "title" would have been accepted
exactly like a real one — stored forever and printed in full by every
list tool's formatter.

Fixed with per-field byte caps (`maxObservationTitleBytes` = 500,
`maxObservationSubtitleBytes` = 1000, `maxObservationNarrativeBytes` =
10000) sized several times larger than any real value ever needs —
matching what `observer.go`'s own prompt already asks a well-behaved
caller for (title a "short title", subtitle a "one-line detail",
narrative "one paragraph") — plus count-and-per-item caps on `facts`/
`concepts` (`maxObservationFactsCount`/`maxObservationConceptsCount` =
50 items, `maxObservationFactBytes` = 1000, `maxObservationConceptBytes`
= 200) following the same "no legitimate caller needs more than a page"
reasoning already governing `store.MaxIDsPerLookup` and
`store.MaxTimelineDepth` — facts and concepts are meant to be a handful
of discrete, short items, not an unbounded list. An oversized argument
gets a clean `isError` tool result naming the limit that was exceeded,
the same shape `get_observations` already uses for too many ids, rather
than a silently-accepted row or a raw driver/network error.

Verified with dedicated tests sending real MCP `tools/call` requests
with oversized title/narrative/facts arguments against a real SQLite
backend, each confirmed as a genuine regression test by temporarily
removing the validation call and watching every one of them fail
(a 501-byte title, a 10001-byte narrative, 51 facts, and a 1001-byte
single fact all got accepted and stored instead of rejected) before
restoring the fix and confirming they pass again — plus a non-regression
test confirming normal-sized fields still succeed, so this is a real
bound and not an accidental block on legitimate calls.

### File-context injection and session summaries leaked into subagent tool calls

Claude Code's hook payloads carry `agent_id`/`agent_type` only when a
hook fires from inside a Task-tool subagent invocation rather than the
main session — absent otherwise. Real claude-mem's own adapter
(`src/cli/adapters/claude-code.ts`'s `normalizeInput`) reads these same
two fields off the same payload, and two of its handlers gate real
behavior on them: `file-context.ts` skips injecting file memory
entirely when `input.agentId` is set, and `summarize.ts` skips the
end-of-session summary the same way — a subagent isn't the end user's
actual session, so it shouldn't see automatically-injected memory or
trigger a session-level summary of its own.

`claude-agent-sdk-go`'s `HookInput` never modeled either field, so
neither check was even possible here: `file-context` (this port's
`PreToolUse`/`Read` hook) unconditionally injected whatever memory
existed about a file, including into a subagent's own `Read` calls, and
`stop` had no equivalent guard. Fixed by adding `AgentID`/`AgentType` to
`HookInput` (`claude-agent-sdk-go` v0.1.2) and wiring the identical skip
into both `file-context` and `stop`, in the same order real claude-mem
checks them: project-exclusion first, then the subagent check.

Verified live end to end rather than just by reading the TS source: a
real `claude` session launched with `--plugin-dir`, one turn doing a
direct `Read` of a seeded file (no `agent_id` present — `file-context`
proceeds to its normal lookup) and a second turn dispatching a `Task`
subagent to `Read` the same file (`agent_id` set, `agent_type` =
`general-purpose` — `file-context.log` shows the new skip line).
Confirmed as a genuine fix, not a tautological check, by temporarily
removing the guard, rebuilding, and rerunning the identical
subagent-`Read` scenario: without the guard the exact same call fell
through to the normal "no prior observations" lookup path instead of
being skipped, before the guard was restored and re-verified.

### No `<private>` tag redaction anywhere — this port's biggest privacy gap

Real claude-mem trains users on a specific convention, stated directly in
its own `UserPromptSubmit` banner: wrap anything in
`<private>...</private>` to keep it out of memory. `src/utils/
tag-stripping.ts`'s `stripMemoryTags` strips six tag names — `private`,
`claude-mem-context`, `system_instruction`, `system-instruction`,
`persisted-output`, `system-reminder` — and real claude-mem applies it at
both of its actual capture boundaries: the `PostToolUse` ingestion path
(`tool_input`/`tool_response`, stripped in `shared.ts` before an
observation is even queued) and prompt handling (`SessionRoutes.ts`
skips session-init/injection entirely — `reason: 'private'` — when a
prompt is wholly wrapped in a tag; `summarize.ts` strips the Stop hook's
summarized text the same way).

This port had none of it. `grep -rn "private" --include=*.go` (excluding
tests) across the whole repository returned zero matches — a
`<private>` block in a tool's output would be captured, sent to the
observer LLM prompt verbatim, summarized, embedded, and persisted
exactly like any other content, then surfaced through
`search_observations`/`semantic_search_observations` to any MCP caller.

Fixed with a new `privacy` package (`StripTags`/`StripMemoryTags`)
porting the exact tag set. It can't be tag-stripping.ts's single combined
regex with a `\1` backreference tying each open tag to its matching
close tag by name — Go's `regexp` package (RE2) has no backreference
support at all — so it's one compiled literal pattern per tag name
instead. Verified this produces identical results to the TS version for
same-tag nesting (the case tag-stripping.ts's own tests exercise:
`<private>a<private>b</private>c</private>` strips down to the same
dangling-`c</private>`-remains-as-literal-text result either way); the
two approaches can differ only in which tag's internal counter gets
credited when two *different* tag types are nested inside each other,
which this package doesn't expose a per-tag breakdown for, so it isn't
observable.

Wired in at the two places that matter for this port's architecture:
`worker.go`'s `PostToolUse` capture strips `tool_input`/`tool_response`
*before* `transcript.Truncate`, not after — a `<private>` block that
happened to straddle the 1500-byte truncation cutoff would otherwise be
left with a dangling, unclosed tag this package's regex could never
match, defeating the whole point — and `prompt-context`'s
`UserPromptSubmit` handling skips the embedding call entirely for a
wholly-private prompt, matching real claude-mem's `reason: 'private'`
skip. `stop.go` needed no separate change: its session summary is built
purely from already-persisted (by then already-redacted) observations,
never from re-reading the raw transcript.

Every unit test case was ported directly from real claude-mem's own
`tests/utils/tag-stripping.test.ts` — read and ported from the actual
test file, not re-derived from prose — covering basic single/multiple/
interleaved tag removal, multiline tag content, ReDoS-volume timing (150
tags and a 10,000-character single tag, both under a 1-second budget),
tags embedded inside JSON strings, both `system_instruction` spellings,
and `system-reminder` including the realistic case real claude-mem's own
test exists for: a tool result carrying an injected CLAUDE.md dump that
itself contains a nested `claude-mem-context` block.

Verified live past the unit tests too, on both call sites, each confirmed
as a genuine fix (not a tautological check) by temporarily removing the
strip call, rebuilding, and rerunning the identical scenario before
restoring it: a real `worker.process()` call with a `<private>` block in
`tool_response` confirmed the transcript.ToolCall actually handed to the
observer contains neither the tag markup nor the secret text inside it
(the disabled version leaked both straight through); and the real,
compiled `prompt-context` binary against a real Ollama model confirmed a
wholly-private prompt is skipped before any embedding call is ever made,
while a partially-private prompt still gets its non-private remainder
embedded and searched normally (the disabled version sent the literal
`<private>...</private>` markup and the secret inside it straight to a
real embedding call).

### A `<private>` prompt only redacted the prompt text — its tool calls and Stop summary weren't suppressed

The tag-stripping fix above only ever gated the prompt's own semantic
embedding. Real claude-mem's privacy guarantee goes further:
`PrivacyCheckValidator.checkUserPromptPrivacy` is consulted at both its
actual capture boundaries — before queueing a `PostToolUse` observation
(`shared.ts`) and before generating a Stop-time summary
(`SessionRoutes.ts`) — and suppresses the *entire turn*, not just the
prompt text, once that turn's persisted prompt stripped to nothing. This
port had no equivalent: a user who wrapped a prompt in
`<private>...</private>` still had every tool call from that turn fully
observed by the LLM, persisted, and included in the Stop summary — the
privacy promise silently stopped at the prompt itself.

Fixed by extending the worker daemon's existing plain-text socket
protocol — already used for the `INFLIGHT <session_id>` query — with two
more message kinds: a fire-and-forget `PRIVATE <session_id> <0|1>`
marker and a request/response `ISPRIVATE <session_id>` query
(`hook.SetSessionPrivate`/`hook.QueryPrivate`, parsed by
`hook.ParsePrivacyMarker`/`hook.ParsePrivacyQuery`). `prompt-context`
sends the marker on **every** `UserPromptSubmit`, private or not — not
only when private. That matters: real claude-mem's own
`PrivacyCheckValidator` doc comment documents a real bug it exists to
avoid (issues #2794/#2795) — a session whose `user_prompts` row is
absent (session-init hadn't run yet) must never be treated as private,
or every observation for that session would be silently frozen forever.
A sticky "mark private and never clear it" flag would reproduce exactly
that failure mode the moment a later, non-private prompt superseded an
earlier private one, so the flag has to be actively re-asserted (to
`false`) on every non-private prompt too, not just set once and left.

The worker's `process()` (`PostToolUse`) and `stop` (`Stop`) both check
the flag — `sessionCache.isPrivate`, defaulting to `false` for a session
with no flag ever recorded, matching real claude-mem's own
absent-signal-defaults-to-allow behavior — right alongside the existing
project-exclusion check, before any real work (spawning an observer
subprocess, opening the store) happens. `stop` runs as its own separate
process with no direct access to the worker's in-memory state, so it
queries `ISPRIVATE` over the socket the same way it already queries
`INFLIGHT`; a query failure (daemon unreachable) deliberately falls
through to summarizing normally rather than skipping, since an unknown
signal must never be treated as "private" either.

One structural difference worth calling out: real claude-mem's
`UserPromptSubmit`-equivalent and its later observation-ingestion calls
run on separate async paths that can genuinely race (hence #2794/#2795
in the first place). This port's `UserPromptSubmit` hook
(`prompt-context`) is **not** registered `"async": true` in
`hooks.json` — it blocks Claude Code's own turn until it returns — so
the privacy marker is guaranteed to reach the worker before Claude Code
even begins the tool-calling turn that could fire a `PostToolUse` event
in response to that same prompt. This port's version is race-free for a
reason real claude-mem's own can't be: the hook chain itself enforces
the ordering, not a lookup against a persisted row.

Verified with new unit tests in the `worker` package — wire-format round
trips for both new message kinds, the full setter/query exchange driven
through the real client functions against a real worker over a real Unix
socket (confirming a later `false` marker actually supersedes an earlier
`true` one, and that a session with no marker ever sent reads back as
not private), `process()` skipping before it ever touches the
nil-backed session cache that would otherwise panic (mirroring the
project-exclusion regression test's own technique), and a stale-entry
eviction test for the map bounding this state's lifetime. Verified live
past the unit tests too: a real worker daemon, a `<private>` prompt
followed by real `PostToolUse` and `Stop` payloads for one session
(confirmed zero rows in the real database across both), against a
control session with a normal prompt (a real observation AND a real
session summary, both persisted) — confirmed as a genuine fix by
temporarily removing the `process()` gate and watching the regression
test fail with a real nil-pointer panic before restoring it.

### `prompt-context` couldn't tell a real prompt apart from an internal Claude Code protocol notification

Claude Code can auto-submit a `<task-notification>...</task-notification>`-
wrapped payload as a `UserPromptSubmit` "prompt" — a background/subagent
task-completion notification, not real user text. Real claude-mem's
`isInternalProtocolPayload` (`src/utils/tag-stripping.ts`) is checked at
both of its `UserPromptSubmit` capture boundaries — the CLI handler
(`session-init.ts`, immediately after its project-exclusion check, before
any privacy check or embedding) and the HTTP session-init route — and
bails out immediately when it fires.

This port had no equivalent anywhere. `prompt-context` would strip
privacy tags from a `<task-notification>` payload (a no-op, since it
isn't a privacy tag), report a non-private state to the worker, and —
since such payloads are often well past the `-min-prompt-len` floor —
pay a real Ollama embedding call and inject "Memory relevant to what you
just asked" context in response to internal plumbing, not anything a
user actually asked.

Fixed with `privacy.IsInternalProtocolPayload`, ported from
`tag-stripping.ts`'s own version. That TS regex combines a `\1`
backreference with a negative lookahead keyed off that same backreference
(`(?:(?!<\1\b|</\1\b)[\s\S])*`) — Go's RE2 engine can express neither
half of it, so this is plain string operations instead: verify the whole
(trimmed) string is exactly one open tag, a body containing no further
occurrence of the tag name, and the matching close tag — the same shape
the backreference restricts the TS version to. Wired into
`prompt-context` in real claude-mem's own check order: right after the
project-exclusion check, before privacy stripping or the
`hook.SetSessionPrivate` notification to the worker.

All 12 test cases were ported directly from real claude-mem's own
`tests/utils/tag-stripping.test.ts` `isInternalProtocolPayload` suite —
read and ported from the actual test file, not re-derived from prose:
bare, empty-body, whitespace-surrounded, multiline, and attributed
blocks (all `true`); an unclosed tag, user text surrounding the block,
unrelated tags, an over-256KB payload, and two adjacent or
text-separated blocks (all `false` — the last deliberately, since the
check is a deny-list per single well-formed block, not concatenations of
them). Every case passes against this port's plain-string
implementation. Verified live against the real compiled binary too: a
real `<task-notification>` payload correctly skipped before ever
attempting to reach the worker socket (confirmed by pointing at a
deliberately unreachable socket path and seeing no connection-failure
log line at all), confirmed as a genuine fix by temporarily removing the
guard and watching the identical payload instead attempt a real worker
notification (which then logged the expected connection failure) and
proceed to a real Ollama embedding call, before restoring it.

### `prompt-context` had no protection against a duplicate `UserPromptSubmit` firing

Claude Code can fire `UserPromptSubmit` more than once for the same
prompt — a real, previously-shipped bug on the TS side, not a
theoretical one: real claude-mem's own issue #2515 tracked exactly this,
and its fix (`findRecentDuplicateUserPrompt`,
`src/services/sqlite/prompts/get.ts`) checks a session's tag-stripped
prompt text against `USER_PROMPT_DEDUPE_WINDOW_MS`
(`src/shared/user-prompts.ts`, 10 seconds) right after its privacy check
and before saving the prompt or doing semantic injection
(`SessionRoutes.ts`). Without it, a duplicate firing pays its own
embedding call and injects its own duplicate "memory relevant to what
you just asked" block in the same turn.

This port had no equivalent anywhere — no `user_prompts` table to check
against, and no dedup logic at all. `cmdPromptContext` embedded and
injected on every single invocation unconditionally, so a duplicate
`UserPromptSubmit` would pay a second real Ollama round-trip and inject
the recall block twice in one turn.

Fixed by extending the worker daemon's plain-text socket protocol again
(the same pattern `PRIVATE`/`ISPRIVATE` and `INFLIGHT` already use) with
a `DEDUPE <session_id> <hash>` request/response call
(`hook.CheckDuplicatePrompt`/`hook.ParseDedupeQuery`). The worker keeps a
per-session `{lastPromptHash, firstSeen}`
(`sessionCache.checkAndRecordPrompt`, alongside the existing
privacy-flag state, for the same reason: it must work before any
`sessionEntry` exists) and reports whether an identical hash was already
recorded within the window. The window is deliberately measured from
the *original* prompt's own timestamp, never extended just because
someone asks again — matching real claude-mem's own semantics precisely:
`findRecentDuplicateUserPrompt` checks against the original saved row's
`created_at`, not against whichever check happened most recently, so a
prompt genuinely repeated by the user after the window elapses is
correctly treated as new rather than silently swallowed. Wired into
`prompt-context` right after the privacy check, matching real
claude-mem's own ordering (privacy first, then duplicate detection).

Verified with new worker-package tests: a wire-format round trip, the
full check-and-record exchange driven through the real client function
against a real socket (including the case that matters most — a
different, later prompt hash supersedes the earlier one, and re-checking
that now-stale earlier hash correctly reads as NOT a duplicate anymore),
an explicit window-expiry test, and stale-entry eviction. Verified live
end to end too: a real worker daemon, the identical `UserPromptSubmit`
payload sent twice for one session against a real Ollama model — the
first call reached a real embedding/semantic-search attempt, the second
was skipped as a duplicate — confirmed as a genuine fix by temporarily
disabling the check and watching both calls independently reach a real
embedding call before restoring it.

## Quick start

```sh
go build -o claude-mem-go ./cmd/claude-mem-go

# One-shot, no hooks needed:
./claude-mem-go ingest -limit 3
./claude-mem-go search "some keyword"
./claude-mem-go semantic-search "a question phrased differently"

# Wired into a project via .claude/settings.json (see .claude/settings.json.example):
./claude-mem-go start   # idempotent — safe to call from every SessionStart
./claude-mem-go hook     # what PostToolUse actually invokes

# As an MCP server (see .mcp.json.example):
./claude-mem-go mcp
```

Data lives in `~/.claude-mem-go/` — `observations.db`, `worker.sock`,
and `worker.log` / `start.log` / `hook.log` (hooks run detached from any
terminal, so these logs are the only way to see what they did). Every log
file rotates at 5MB, keeping one prior generation (`name.log.1`) — there
was no cap at all before this, and `worker.log` in particular gets a new
line on every `PostToolUse` event for as long as the daemon runs, which is
meant to be months. Verified against a real running daemon, not just unit
tests: grew a real `worker.log` past the cap by hand, restarted the
daemon, and confirmed it rotated the oversized file to `worker.log.1` and
started a fresh one on its very first log line.

## Running the worker as a supervised service (optional)

By default, the worker daemon only ever starts lazily: `start` (invoked
from `SessionStart`) spawns it if it isn't already running, guarded by a
spawn lock so concurrent sessions starting at once don't race. That's
fine for normal use, but it means a crashed daemon, or a machine reboot
with no Claude Code session active to trigger the next `SessionStart`,
leaves memory capture silently dead until something starts a session
again.

`deploy/systemd/claude-mem-go-worker.service` (Linux, user-level
systemd) and `deploy/launchd/com.claude-mem-go.worker.plist` (macOS
launchd user agent) are optional templates for supervising it
independently — install either one and the daemon restarts itself on
crash and comes back after a reboot, with no conflict with `start`'s own
lazy-launch logic (`start` checks whether a worker is already reachable
before spawning one, so having a supervisor keep it running just means
that check always finds one already there). See the comments at the top
of each file for install steps.

Verified for real, not just written and assumed: the systemd unit was
checked with `systemd-analyze verify` in a real systemd container (Docker
— its `%h` specifier resolves correctly, and the file parses clean); the
launchd plist was actually loaded on a real macOS machine, confirmed the
supervised process came up and answered `doctor` on its own socket, then
killed the process directly and confirmed `KeepAlive` relaunched it
within seconds — genuine crash recovery, not assumed from the plist's own
claimed behavior.

## Known limitations

- **Semantic search is brute-force cosine similarity only on the SQLite
  backend** — fine at the scale one project's observations realistically
  reach, won't scale to millions of rows. The Postgres backend has a real
  HNSW ANN index instead; use it once scale is an actual concern.
- **The schema is a narrower subset** of claude-mem's real `observations`
  table (40+ migrations' worth of sync/origin-device bookkeeping and an
  FTS5 shadow table are not replicated here) — this persists what an
  observation actually *contains* plus a content-hash dedup key, not
  claude-mem's full multi-device sync machinery. What's no longer a gap:
  a real versioned migration path for whatever gets added next (see
  "Schema migrations" above) — neither backend had one before.
- **`Setup` isn't wired** — real claude-mem uses it for version-checking a
  Node/Bun install; there's no equivalent check this single static Go
  binary needs (no runtime to verify, no interpreter version to detect).
  `UserPromptSubmit` **is** now wired (see below) — the last of the two
  hooks this section used to list as missing.

### `UserPromptSubmit` — semantic context injection on the actual prompt

`SessionStart`'s context injection (`context` subcommand) is a static
"most recent observations" dump, decided before Claude has any idea what
the user is about to ask. `prompt-context` closes the sharper gap real
claude-mem's own `session-init` handler covers: it fires on the actual
submitted prompt text, embeds it via the same local Ollama call
`semantic-search` uses, and injects the observations semantically closest
to *that specific question* — recall that responds to what's actually
being asked, not just "what happened lately."

Needed a new field on `claude-agent-sdk-go`'s `HookInput`
(`Prompt string`, confirmed against a real captured `UserPromptSubmit`
payload — same technique this package's other fields were verified with —
tagged `v0.1.1`) since nothing had ever needed the submitted prompt text
before. Guards mirror real claude-mem's own: prompts under 20 characters
are skipped rather than embedded (too short to be a meaningful semantic
anchor, and would waste an Ollama round-trip on every single message for
no benefit), and `-embed-model ""` disables the hook entirely — the same
on/off convention `mcp`/`stop` already use.

Verified against a real, isolated Ollama-backed setup, not a mock: two
topically distinct observations seeded and embedded (one about choosing
Postgres/pgvector for ANN search, one about log rotation), a real
`claude -p` session asked "what database technology did we choose for
scaling similarity search over embeddings?" — no keyword overlap with the
seeded title's exact wording — and Claude's answer came back correctly
identifying Postgres/pgvector/HNSW, explicitly attributing it to injected
memory rather than a codebase search. Deliberately run against a
throwaway `-db` file via `.claude/settings.json` rather than a full
plugin install, to avoid writing test rows into this machine's real,
shared production database (the same discipline applied throughout this
project's live-verification history).

## Security

See [`SECURITY.md`](SECURITY.md) for the trust model (no auth on the MCP
server or worker socket — the boundary is the local OS user, same as any
MCP server; the one real network surface is the opt-in Prometheus
endpoint, which should stay bound to localhost), what's actually
hardened and why (DSN redaction, bounded inputs, panic recovery), and
what's explicitly out of scope (encryption at rest, rate limiting) —
plus how to report a vulnerability.

## Testing

```sh
go test ./...          # all packages that don't need `claude`/Ollama/Docker
go test ./... -race

docker compose up -d   # then postgres/... runs against the real container
go test ./postgres/... -v
```

`.github/workflows/ci.yml` runs the same commands (plus a `pgvector/pgvector:pg16`
service container) on every push, and genuinely passes there — checked via
`gh run list`, not assumed: every run since "Add a real versioned
schema-migration framework for both backends" has completed successfully
on GitHub's own hosted runners. That wasn't always true earlier in this
project's history: every run before that failed at `startup_failure`
before ever reaching a single step, caused by an account-level GitHub
billing lock unrelated to this repo or its workflow file — that's what
`act` (a real Docker-backed local runner) and `actionlint` were verifying
against at the time, since the hosted runners weren't reachable at all.
The billing lock has since been resolved; hosted-runner CI is the current
source of truth again.

`postgres/`'s tests skip cleanly (not fail) when nothing is listening at
`localhost:55432` — start `docker compose up -d` first if you want them to
actually run. They're real integration tests against a live container, not
mocks: dedup, hyphenated-query full-text search, and vector-similarity
ranking (with a control vector orthogonal to the query, confirming rank
order rather than just "no error") all execute real SQL.

Packages with pure logic (`classify`, `pool`, `store`, `transcript`,
`observer`'s retry policy, `worker`'s spawn-lock) have real unit tests.
`claude-agent-sdk-go`'s `Session`/`Query` and this project's end-to-end
worker/hook flow are validated by hand against the real `claude` CLI and
Claude Code hooks (see the project history) rather than mocked — mocking
the subprocess protocol would test the mock, not the thing that actually
breaks.
