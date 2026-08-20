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
  search, brute-force cosine similarity for semantic search.
- **Postgres + pgvector** (`postgres://...` DSN) — real full-text search
  (`tsvector`/GIN, no hand-written query sanitizer needed — Postgres
  tokenizes punctuation like the hyphen in "claude-mem" sanely by default,
  unlike SQLite's FTS5) and a real ANN index (HNSW) for semantic search
  instead of a linear scan. `docker-compose.yml` brings up
  `pgvector/pgvector:pg16`; every claim above (the HNSW index actually gets
  used, not just created; the hyphen query that broke FTS5 works here
  without a workaround) was checked with `EXPLAIN` and real queries against
  that container, not assumed.

```sh
docker compose up -d
./claude-mem-go ingest -db "postgres://claudemem:claudemem@localhost:55432/claudemem?sslmode=disable"
```

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
  payloads through a bounded `pool` of observer sessions.
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
  the duplicate and did nothing.
- **ingest** — one-shot: read a real transcript file, observe N tool calls,
  persist them. Useful for backfilling or testing without wiring up hooks.
- **search** / **semantic-search** — keyword (FTS5) and meaning-based
  (local embeddings + cosine similarity) search over what's been persisted.
  `-project` scopes to one project; the default (empty) searches every
  project in the store, since these are ad-hoc CLI lookups run by a human
  who may genuinely want that.
- **mcp** — an MCP server (stdio, JSON-RPC 2.0) exposing `search_observations`
  and `semantic_search_observations` as tools any MCP client — including
  Claude Code itself — can call directly. Wire format confirmed against a
  real `claude` session, not assumed from the spec (see `mcpserver/`'s doc
  comment); end-to-end tool calls verified against the real CLI too.
  Scoped to the current project by default (derived from the server
  process's cwd) — this store is one shared database across every project
  ever recorded on the machine, so an unscoped search is a real
  cross-project leak, not just a ranking nuisance; found via a Postgres
  test flake (accumulated rows from unrelated projects crowded a fixed
  `LIMIT`), fixed at the `store.Backend` interface level so both backends
  and the CLI got it too. Pass `all_projects: true` to a tool call to
  search everything on purpose.
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
- **doctor** — an operational health check: is the `claude` CLI on `PATH`,
  is the worker daemon reachable, is the database reachable, is Ollama
  reachable with the configured model actually pulled. Distinguishes
  critical failures (exit 1 — nothing works without these) from
  informational ones (worker not running is fine, `start` launches it
  lazily; no Ollama just means no semantic search). Verified against real
  failures, not just the happy path: a genuinely unreachable Postgres DSN
  correctly exits 1, and an unpulled Ollama model correctly downgrades to
  a warning rather than a failure. Also surfaces the worker's own activity
  (see below) when available.
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
  persisted observation.

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

### Why the worker/hook split

An earlier version had the `PostToolUse` hook call the observer directly.
Testing that against a real Claude Code session showed it losing
observations: `"async": true` only means Claude Code doesn't wait for the
hook — it does **not** mean the hook's child process survives the
invoking `claude` process exiting. Both attempts died mid-observation. The
fix is what's here: a daemon started once and left running (detached via
`Setsid` so it survives its own launcher exiting too), with hooks doing
nothing but a fire-and-forget local socket write.

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
terminal, so these logs are the only way to see what they did).

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
- **`Setup` and `UserPromptSubmit` aren't wired** — real claude-mem uses
  these for version-checking and session-init respectively.
  `SessionStart`+`PreToolUse`+`PostToolUse`+`Stop` now cover recall (both
  per-project and per-file), capture, and summarization; these two remaining
  hooks are lower-value without claude-mem's modes/knowledge-graph system,
  which also has no analog here. (`PreToolUse` — per-file recall on `Read`
  — is wired as of the `file-context` subcommand above; it used to be in
  this list.)

## Testing

```sh
go test ./...          # all packages that don't need `claude`/Ollama/Docker
go test ./... -race

docker compose up -d   # then postgres/... runs against the real container
go test ./postgres/... -v
```

`.github/workflows/ci.yml` runs the same commands (plus a `pgvector/pgvector:pg16`
service container) on every push. It's validated with [`actionlint`](https://github.com/rhysd/actionlint)
and with [`act`](https://github.com/nektos/act) against a real Docker-backed
runner — both clean. GitHub's own hosted runners aren't currently executing
it (account-level, unrelated to this repo or its workflow file); `act` is
the actual verification this was tested against.

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
