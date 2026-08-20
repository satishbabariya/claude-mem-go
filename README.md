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
- **ingest** — one-shot: read a real transcript file, observe N tool calls,
  persist them. Useful for backfilling or testing without wiring up hooks.
- **search** / **semantic-search** — keyword (FTS5) and meaning-based
  (local embeddings + cosine similarity) search over what's been persisted.
  `-project` scopes to one project; the default (empty) searches every
  project in the store, since these are ad-hoc CLI lookups run by a human
  who may genuinely want that.
- **mcp** — an MCP server (stdio, JSON-RPC 2.0) exposing eight tools any MCP
  client — including Claude Code itself — can call directly:
  `search_observations` and `semantic_search_observations` (keyword and
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
- **skills/mem-search** — a real Claude Code skill (`/mem-search`) teaching
  Claude when to reach for `search_observations` vs.
  `semantic_search_observations`. Validated with `claude plugin validate
  --strict`, and verified live: installed the plugin, ran `/mem-search
  claude-mem installation` in a real session, and confirmed via the MCP
  server's own log that a genuine `tools/call` fired — not a hallucinated
  answer.
- **skills/mem-doctor** — a second skill (`/mem-doctor`) surfacing the
  `doctor` health check *inside* a Claude Code session instead of only from
  a raw terminal — "is memory actually working" shouldn't require dropping
  out of the conversation to find out. Verified the same way as
  `mem-search`: installed the plugin at project scope in a throwaway
  directory, ran `/mem-doctor` in a real session, and got the real health
  check's own output back (worker/database/Ollama status), confirming
  `$CLAUDE_PLUGIN_ROOT` resolves correctly for a skill-invoked command, not
  just for hooks and the MCP server.
- **skills/mem-prune and skills/mem-export** — surface `prune` and
  `export`/`import` as `/mem-prune`/`/mem-export` the same way
  `mem-doctor` surfaces `doctor`. `mem-prune`'s instructions are written to
  treat this as the one genuinely destructive operation in the CLI: always
  run the dry run first, show the count, and get explicit confirmation
  before ever adding `-yes` — verified live, not just written and hoped
  for: asked a real session to "clean up memories older than 1 day" and
  confirmed it ran the dry run, reported the count, and asked whether to
  actually delete rather than doing so on its own.
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
