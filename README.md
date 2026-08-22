# claude-mem-go

A Go reimplementation of claude-mem's core loop — capture what Claude Code
did, compress it into a structured observation, persist it, make it
searchable — built on [claude-agent-sdk-go](https://github.com/satishbabariya/claude-agent-sdk-go) instead
of the closed-source `@anthropic-ai/claude-agent-sdk` npm package.

This exists because every step of it was tested against the real thing
first: the SDK layer is a subprocess wrapper around the `claude` CLI's
documented `stream-json` protocol (verified by reading the npm package's
`.d.ts` files and exercising the protocol directly), and the worker/hook
split ([docs/hooks.md](docs/hooks.md)) exists because a simpler "hook does the work inline" design was
tried, found to lose data, and fixed — not because it seemed like a good
idea in the abstract.

## Contents

- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [Operating it](#operating-it)
- [Backends](#backends)
- [Known limitations](#known-limitations)
- [Security](#security)
- [Testing](#testing)
- [Documentation](#documentation)
- [License](#license)

## Requirements

- The `claude` CLI on `PATH` (used as the actual model backend — no API key
  management here beyond what `claude` itself already handles).
- [Ollama](https://ollama.com) running locally, with an embedding model
  pulled (`ollama pull nomic-embed-text`), if you want semantic search.
  Keyword search (`search`) works without it.
- Go 1.25+ (what go.mod requires; CI also tests the latest stable).

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

Install it as a Claude Code plugin (project scope — never user/machine-wide
while trying it out, that would affect every other session on the box):

```sh
go build -o claude-mem-go ./cmd/claude-mem-go
claude plugin validate .                                          # manifest sanity check

# From inside a project you want claude-mem-go active in:
claude plugin marketplace add /path/to/claude-mem-go --scope project
claude plugin install claude-mem-go@claude-mem-go-local --scope project
```

Then run `./claude-mem-go doctor` to confirm the worker, database, and Ollama
are reachable. See [docs/plugin-install.md](docs/plugin-install.md) for what
the install does, how a git-installed plugin builds its own binary, and the
bundled `/mem-*` skills.

## Architecture

```
transcript / hook payload  →  observer (claude-agent-sdk-go Session)  →  classify
                                                                            │
                                                      memory.Backend (SQLite or Postgres)
```

Package layout (everything under `internal/` is private to this module;
`cmd/claude-mem-go` is the one binary):

```
cmd/claude-mem-go/        one thin file per subcommand; dispatch in main.go
internal/
  memory/                 storage-neutral domain: Backend interface, Observation,
                          SearchResult, ExportRow, ContentHash, path/project helpers
  memory/sqlite/          the SQLite implementation (zero-dependency default)
  memory/postgres/        the Postgres + pgvector implementation
  memory/backend/         DSN → implementation dispatcher (backend.Open)
  migrate/  pool/         shared infrastructure
  worker/                 the daemon: socket server, session cache, spawn/stop control
  observer/ classify/     the LLM observer session and its error classification
  hook/ transcript/       the hook-side client protocol and transcript parsing
  privacy/ excludeproject/ contextfmt/ logging/ plugincheck/ cli/
  mcpserver/              the MCP server (transport, tool table, handlers, formatters)
bench/recall/             ANN recall benchmark harness (not part of the binary)
scripts/  deploy/  hooks/  skills/  .claude-plugin/   plugin surface and ops files
```

`memory.Backend` is one interface with two implementations, selected by what
the `-db` flag looks like:

- **SQLite** (default, zero dependencies) — a file path.
- **Postgres + pgvector** — a `postgres://...` DSN.

See [Backends](#backends) below and [docs/postgres.md](docs/postgres.md).

## Configuration

### Environment variables

Hooks and the MCP server are invoked by Claude Code with no flags, so these
variables are the only configuration that reaches them. Every `CLAUDE_MEM_*`
variable the code reads is listed here (a test, `TestEveryEnvVarIsDocumented`,
enforces that).

| Variable | What it does |
|---|---|
| `CLAUDE_MEM_DB` | SQLite file path or `postgres://` DSN. Without it an installed plugin cannot reach Postgres at all — see [docs/postgres.md](docs/postgres.md#setup). |
| `CLAUDE_MEM_LOG_LEVEL` | `DEBUG`, `INFO` (default), `WARN`, `ERROR`, or `SILENT` for the hooks and the worker daemon. |
| `CLAUDE_MEM_OLLAMA_BASE_URL` | Where the embedding server is; default `http://localhost:11434`. |
| `CLAUDE_MEM_POSTGRES_STATEMENT_TIMEOUT_MS` | Per-statement timeout added to the Postgres DSN so one hung query cannot wedge the pool (default 30000). |
| `CLAUDE_MEM_POSTGRES_CONNECTION_TIMEOUT_MS` | Bound on each initial-connection ping attempt (default 5000). |
| `CLAUDE_MEM_POSTGRES_POOL_MAX` | Postgres connection-pool size (default 10). |
| `CLAUDE_MEM_POSTGRES_IDLE_TIMEOUT_MS` | How long an idle pooled Postgres connection is kept (default 30000, matching real claude-mem). |
| `CLAUDE_MEM_POSTGRES_EMBED_DIMS` | Vector column dimension for the Postgres backend (default 768, must match the embedding model). |
| `CLAUDE_MEM_STORE_PROMPTS` | `1` makes the `UserPromptSubmit` hook persist each prompt's text (after `<private>` stripping); off by default because it stores the user's verbatim words. Same as `-store-prompts`. See [docs/hooks.md](docs/hooks.md#persisting-prompts-opt-in). |
| `CLAUDE_MEM_EXCLUDED_PROJECTS` | Real claude-mem's variable; this port takes the same patterns via `-excluded-projects` — see [docs/hooks.md](docs/hooks.md#excluding-a-project-from-automatic-capture--a-real-feature-gap-this-port-had-until-now). |
| `CLAUDE_MEM_GO_TEST_POSTGRES_DSN` | Test-only: the throwaway database the Postgres tests write to. See [Testing](#testing). |

The Postgres tuning variables are explained, with the defaults and the bugs
that motivated them, in [docs/findings.md](docs/findings.md).

Precedence is `-db` > `$CLAUDE_MEM_DB` > `~/.claude-mem-go/observations.db`,
and `doctor` now names which of the three won, because the symptom of
getting it wrong looks exactly like data loss.

`CLAUDE_MEM_LOG_LEVEL` sets how much the hooks and the worker daemon
say: `DEBUG`, `INFO` (the default), `WARN`, `ERROR`, or `SILENT` — the
same names and the same variable real claude-mem reads, so one setting
configures either implementation. Three levels are worth knowing:

- `ERROR` shows only things that failed.
- `WARN` adds degradations — an observation stored but not embedded, a
  best-effort call to the daemon that did not land. These are not errors:
  the memory is there, some secondary capability is not.
- `DEBUG` adds the per-tool-call chatter, including each hook's
  "forwarded N bytes to worker" line. That line is the one to turn on
  when an observation seems to have gone missing, because at the default
  level a *successful* forward says nothing at all.

An unrecognized value falls back to `INFO` rather than silencing the
logs: a typo in this variable must not hide its own evidence, and for
fire-and-forget hooks these files are the only diagnosis there is.

`CLAUDE_MEM_OLLAMA_BASE_URL` is the same idea for the embedding server.
The address was hardcoded to `http://localhost:11434` at all ten
`embed.NewClient` call sites, so Ollama on a shared GPU box, a container
on another port, or any remote host simply could not be used — and since
hooks and the MCP server take no flags, there was no way to say so. Set
it and every path (hooks, MCP, `worker`, `reembed`, `doctor`) follows.

### Flags

Every subcommand takes `-db` (which wins over `CLAUDE_MEM_DB`). The other flag
families, by the commands that share them:

- **Socket/daemon** — `-socket` (every hook, `start`, `worker`, `doctor`);
  `-model`, `-max-concurrent`, `-metrics-addr`, `-stats`, and
  `-excluded-projects` on `start`/`worker` (`start` forwards them to the
  worker it spawns).
- **Embedding** — `-embed-model` on `ingest`, `stop`, `start`/`worker`,
  `semantic-search`, `prompt-context`, `mcp`, `doctor`, and `reembed`;
  `-embed-model ""` disables semantic features on that command.
- **Postgres recall** — `-hnsw-ef-search` on `semantic-search`,
  `prompt-context`, `mcp`, and `doctor` (see [docs/postgres.md](docs/postgres.md#-hnsw-ef-search-the-query-time-recallspeed-knob)).
- **Context injection** — `-limit` and `-excluded-projects` on `context`,
  `file-context`, `prompt-context`, and `stop`; `-min-prompt-len` on
  `prompt-context`.
- **Search** — `-project`, `-limit`, `-offset`, `-type`, `-date-start`,
  `-date-end`, `-order-by` on `search`; `-project`, `-limit` on
  `semantic-search`.
- **Maintenance** — `-older-than-days`/`-relative-paths`, `-project`, `-yes`
  on `prune`; `-project`, `-yes` on `reembed`; `-out` on `export`, `-in` on
  `import`; `-transcript`, `-limit` on `ingest`.

### Data directory

Data lives in `~/.claude-mem-go/` — `observations.db`, `worker.sock`,
`worker-stats.json`, and `worker.log` / `start.log` / `hook.log` (hooks run
detached from any terminal, so these logs are the only way to see what they
did). Every log file rotates at 5MB, keeping one prior generation
(`name.log.1`).

## Operating it

- **`doctor`** — health check: `claude` on `PATH`, worker reachable, database
  reachable, Ollama reachable with the model pulled, the plugin actually
  installed and runnable, plus each backend's own details (`journal_mode`,
  pool utilization, `hnsw_ef_search`, `embedding_dims_consistent`, and so
  on). Exit 1 only for critical failures. Also available in-session as
  `/mem-doctor`.
- **`stats`** — the worker writes `~/.claude-mem-go/worker-stats.json` after
  every processed event (persisted / deduped / failed counts, pool
  utilization); `doctor` prints it, and `-metrics-addr 127.0.0.1:9090` on
  `start`/`worker` serves the same data as Prometheus text at `/metrics`.
- **`search` / `semantic-search`** — keyword (FTS5 or `tsvector`) and
  meaning-based search. `-project` scopes to one project; empty searches the
  whole store. `search` filters by `-type` (comma-separated
  `discovery`/`change`/`decision`/`summary`/`manual`), a date window, and
  `-order-by`, and pages with `-offset`. The MCP server exposes the same reads
  as tools: `search_observations`, `semantic_search_observations`,
  `recent_observations`, `session_observations`, `session_start_context`,
  `file_observations`, `get_observations`, `timeline`,
  `observation_context`, `important_workflow`, the one write tool,
  `add_observation`, and — only once prompt persistence is opted into —
  `search_prompts` and `session_prompts` over the user's own stored prompts
  (see [Persisting prompts](docs/hooks.md#persisting-prompts-opt-in)).
- **`export` / `import`** — JSON Lines backup and restore, and the
  SQLite-to-Postgres migration path: `export` from one backend, `import` into
  the other. Import is idempotent (rows are deduped by `content_hash`) and
  preserves original timestamps and embeddings.
- **`prune`** — `-older-than-days N` deletes observations older than the
  cutoff; `-relative-paths` instead strips relative entries from
  `files_read`/`files_modified` (rows written before paths were
  canonicalized, which `file-context` can never match) and keeps the
  observations. Both are dry-run until `-yes`; `-project` scopes either.
- **`reembed`** — re-embeds every observation with no embedding, or one whose
  dimension no longer matches the configured model (probed live). Dry-run
  until `-yes` because each row costs an Ollama call.
- **`version`** — the exact commit and build time, from Go's own VCS stamping.
- **The worker daemon** — `start` (from `SessionStart`) spawns a detached
  `worker` if none is reachable, guarded by a spawn lock. `-max-concurrent`
  (default 4) is the number of observer sessions kept cached at once; a
  session arriving past it evicts the idlest cached one, and a negative value
  is clamped rather than crashing the daemon. To keep the worker alive across
  crashes and reboots, install the systemd or launchd template — see
  [Running the worker as a supervised service](docs/hooks.md#running-the-worker-as-a-supervised-service-optional).

Hook-by-hook behaviour (`SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `Stop`, `Setup`), `<private>` tags, and excluded projects are in
[docs/hooks.md](docs/hooks.md).

## Backends

**SQLite** (default) — a single file, zero dependencies. FTS5 keyword search
and brute-force cosine similarity for semantic search. Opened with WAL journal
mode, a 5s busy-timeout, and foreign keys on, all via DSN parameters so every
pooled connection gets them; the worker daemon and every CLI subcommand open
their own connection to the same file.

**Postgres + pgvector** (`postgres://...` DSN) — real full-text search
(`tsvector`/GIN) and a real HNSW ANN index for semantic search, for stores
that outgrow a linear scan. `docker-compose.yml` brings up
`pgvector/pgvector:pg16`. Setup, `CLAUDE_MEM_DB`, TLS, the
`-hnsw-ef-search` knob, and the 250,000-row measurements are in
[docs/postgres.md](docs/postgres.md).

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
  [Schema migrations](docs/development.md#schema-migrations)) — neither backend had one before.
- **`Setup` is wired, but only for one job** — `scripts/ensure-binary.sh`
  builds the plugin binary from the shipped source when it is missing or
  not runnable (a git-installed plugin ships no binary). It does not do
  what real claude-mem's Setup does (version-check a Node/Bun runtime),
  because a single static Go binary has no runtime to verify. Setup was
  observed not to fire under `claude -p`, so `scripts/run-hook.sh` also
  announces a missing/broken binary on SessionStart rather than relying
  on Setup alone.

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

# The Postgres tests need a database, and they will NOT guess at one.
docker compose up -d
docker compose exec postgres createdb -U claudemem claudemem_test
CLAUDE_MEM_GO_TEST_POSTGRES_DSN=postgres://claudemem:claudemem@localhost:55432/claudemem_test?sslmode=disable \
  go test ./internal/memory/postgres/... -v
```

**Point that variable at a throwaway database, never one you actually
use.** These tests write real rows, and the variable is mandatory
precisely because it used to be optional. `internal/memory/postgres/postgres_test.go`
previously fell back to `postgres://…@localhost:55432/claudemem` — the
same DSN this README documents for a real store, a few lines up at the
Postgres setup section. Nobody had to opt in to that; running
`go test ./...` was enough.

More on what the suite covers, CI, and why that variable is mandatory:
[docs/development.md](docs/development.md#testing).

## Documentation

- [docs/findings.md](docs/findings.md) — what this port fixed, and how each fix was verified; also how every subcommand and MCP tool was built.
- [docs/postgres.md](docs/postgres.md) — Postgres/pgvector setup, DSNs and TLS, `ef_search` tuning, recall measurements.
- [docs/hooks.md](docs/hooks.md) — hook-by-hook behaviour, the worker/hook split, privacy tags, excluded projects, supervised service.
- [docs/plugin-install.md](docs/plugin-install.md) — plugin install, the self-healing binary, bundled skills.
- [docs/development.md](docs/development.md) — schema migrations, testing details, releases.
- [bench/recall/README.md](bench/recall/README.md) — the ANN recall benchmark harness.
- [CHANGELOG.md](CHANGELOG.md) and [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).
