# claude-mem-go

A Go reimplementation of claude-mem's core loop for Claude Code: capture what
each tool call did, compress it into a structured observation with a model,
persist it, and recall it — automatically at session start, on every prompt,
and before every file read — or on demand through an MCP server. It is built
on [claude-agent-sdk-go](https://github.com/satishbabariya/claude-agent-sdk-go),
a subprocess wrapper around the `claude` CLI's `stream-json` protocol, rather
than the closed-source npm SDK. One static binary, a SQLite file by default,
Postgres + pgvector when a store outgrows a linear scan.

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

- The `claude` CLI on `PATH`. It is the model backend; claude-mem-go manages
  no API keys of its own.
- [Ollama](https://ollama.com) with an embedding model pulled
  (`ollama pull nomic-embed-text`) for semantic search. Keyword search works
  without it.
- Go 1.25+ to build (what `go.mod` requires; CI also tests the latest stable).
- Docker, only for the optional Postgres backend.

## Quick start

Build, then try the store without wiring any hooks:

```sh
go build -o claude-mem-go ./cmd/claude-mem-go

./claude-mem-go ingest -limit 3                         # observe 3 tool calls from your latest transcript
./claude-mem-go search "some keyword"                   # full-text keyword search (a query is required)
./claude-mem-go semantic-search "a question phrased differently"
./claude-mem-go doctor                                  # claude CLI, worker, database, Ollama, plugin
```

Install it as a Claude Code plugin so the hooks and MCP server run
automatically. Use project scope while evaluating; a user-scope install
affects every Claude Code session on the machine.

```sh
claude plugin validate .
claude plugin marketplace add /path/to/claude-mem-go --scope project   # run inside the target project
claude plugin install claude-mem-go@claude-mem-go-local --scope project
./claude-mem-go doctor
```

The plugin wires `hooks/hooks.json` (six hook events), `.mcp.json` (the MCP
server), and the `/mem-*` skills. See [docs/plugin-install.md](docs/plugin-install.md).
To use the MCP server without the plugin, copy `.mcp.json.example`; to wire
hooks by hand, copy `.claude/settings.json.example`.

## Architecture

```
transcript / hook payload  →  observer (claude-agent-sdk-go Session)  →  classify
                                                                            │
                                                      memory.Backend (SQLite or Postgres)
```

Every hook is a short-lived process. `PostToolUse` forwards its payload over a
Unix socket to a long-lived worker daemon, which runs the observer session and
writes the observation; the hook exits in milliseconds. The recall hooks
(`context`, `prompt-context`, `file-context`) read the store directly. Why the
split exists, and what each hook does, is in [docs/hooks.md](docs/hooks.md).

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

## Configuration

### Environment variables

Hooks and the MCP server are invoked by Claude Code with no flags, so
environment variables are the only configuration that reaches them. Every
`CLAUDE_MEM_*` variable the code reads is listed here; the test
`TestEveryEnvVarIsDocumented` fails if one is missing.

| Variable | What it does |
|---|---|
| `CLAUDE_MEM_DB` | SQLite file path or `postgres://` DSN. Without it an installed plugin cannot reach Postgres at all — see [docs/postgres.md](docs/postgres.md#setup). |
| `CLAUDE_MEM_LOG_LEVEL` | `DEBUG`, `INFO` (default), `WARN`, `ERROR`, or `SILENT` for the hooks and the worker daemon. An unrecognized value falls back to `INFO`, never to silence. |
| `CLAUDE_MEM_OLLAMA_BASE_URL` | Embedding server address; default `http://localhost:11434`. Followed by every path: hooks, MCP, `worker`, `reembed`, `doctor`. |
| `CLAUDE_MEM_POSTGRES_STATEMENT_TIMEOUT_MS` | Per-statement timeout added to the Postgres DSN so one hung query cannot wedge the pool (default 30000). |
| `CLAUDE_MEM_POSTGRES_CONNECTION_TIMEOUT_MS` | Bound on each initial-connection ping attempt (default 5000). |
| `CLAUDE_MEM_POSTGRES_POOL_MAX` | Postgres connection-pool size (default 10). |
| `CLAUDE_MEM_POSTGRES_IDLE_TIMEOUT_MS` | How long an idle pooled Postgres connection is kept (default 30000). |
| `CLAUDE_MEM_POSTGRES_EMBED_DIMS` | Vector column width when a Postgres store is first created (default 768; must match the embedding model). Does not resize an existing store. |
| `CLAUDE_MEM_STORE_PROMPTS` | `1` makes the `UserPromptSubmit` hook persist each prompt's text after `<private>` stripping. Off by default because it stores the user's verbatim words. Same as `-store-prompts`. See [docs/hooks.md](docs/hooks.md#persisting-prompts-opt-in). |
| `CLAUDE_MEM_EXCLUDED_PROJECTS` | Real claude-mem's variable. This port takes the same glob patterns via the `-excluded-projects` flag instead — see [docs/hooks.md](docs/hooks.md#excluded-projects). |
| `CLAUDE_MEM_GO_TEST_POSTGRES_DSN` | Test-only: the throwaway database the Postgres tests write to. See [Testing](#testing). |

The store is chosen by precedence `-db` > `$CLAUDE_MEM_DB` >
`~/.claude-mem-go/observations.db`; `doctor` names which one won, because a
daemon and a CLI reading different stores looks exactly like data loss.

Log levels worth knowing: `WARN` adds degradations (an observation stored but
not embedded; a best-effort call to the daemon that did not land) — the memory
is there, a secondary capability is not. `DEBUG` adds per-tool-call chatter,
including each hook's "forwarded N bytes to worker" line; at the default level
a *successful* forward logs nothing, so `DEBUG` is what shows whether a hook
forwarded at all.

### Flags

Every store-touching subcommand takes `-db`, which wins over `CLAUDE_MEM_DB`.
The other flag families, by the commands that share them:

- **Socket/daemon** — `-socket` on every hook, `start`, `worker`, `doctor`;
  `-max-concurrent` (default 4), `-metrics-addr` and `-excluded-projects` on
  `start`/`worker` (`start` forwards them to the worker it spawns); `-stats`
  on `start`, `worker` and `doctor`.
- **Model** — `-model` (default `haiku`), the model the observer runs, on
  `start`/`worker`, `ingest` and `stop`.
- **Embedding** — `-embed-model` (default `nomic-embed-text`) on `ingest`,
  `stop`, `start`/`worker`, `prompt-context`, `semantic-search`, `mcp`,
  `doctor`, and `reembed`; `-embed-model ""` disables semantic features on
  that command.
- **Postgres recall** — `-hnsw-ef-search` (default 200, range 1–1000) on
  `semantic-search`, `prompt-context`, `mcp`, and `doctor`. See
  [docs/postgres.md](docs/postgres.md#semantic-search).
- **Context injection** — `-limit` and `-excluded-projects` on `context`,
  `file-context`, `prompt-context`, and `stop`; `-min-prompt-len` (default
  20) and `-store-prompts` on `prompt-context`.
- **Search** — `-project`, `-limit`, `-offset`, `-type`, `-date-start`,
  `-date-end`, `-order-by` on `search`; `-project`, `-limit` on
  `semantic-search`.
- **Maintenance** — `-older-than-days` or `-relative-paths`, `-project`, `-yes`
  on `prune`; `-project`, `-yes` on `reembed`; `-out` on `export`, `-in` on
  `import`; `-transcript`, `-limit` on `ingest`.

`claude-mem-go <command> -h` prints each command's flags and defaults.

### Data directory

`~/.claude-mem-go/` holds `observations.db`, `worker.sock`,
`worker-stats.json`, and the logs: `worker.log`, `start.log`, `hook.log`,
`context.log`, `prompt-context.log`, `file-context.log`, `stop.log`, `mcp.log`, and
`missing-binary.log`. Hooks run detached from any terminal, so these logs are
the only record of what they did. Every log the binary writes rotates at 5MB,
keeping one prior generation (`name.log.1`); `missing-binary.log` is appended
by the shell wrapper at a point where the binary may not exist, so it is the
one file with no rotation.

## Operating it

- **`doctor`** — the health check to start with. Verifies the `claude` CLI,
  whether the plugin is installed *and its binary runs*, the worker daemon
  (build version, store, pool saturation, recall counters), the database
  (including an empty store on an installed plugin, unembedded rows, and
  sessions with no summary), Ollama with the model pulled, and each backend's
  own details (`journal_mode`, pool utilization, `hnsw_ef_search`,
  `embedding_dims_consistent`, and so on). Exit 1 only for critical failures.
  Also available in-session as `/mem-doctor`.
- **`stats`** — what the store contains. The worker separately writes
  `~/.claude-mem-go/worker-stats.json` after every event (persisted, deduped,
  failed counts; pool utilization; recall counters; build version and store);
  `doctor` prints it, and `-metrics-addr 127.0.0.1:9090` on `start`/`worker`
  serves the same data as Prometheus text at `/metrics`.
- **`search` / `semantic-search`** — keyword (FTS5 or `tsvector`) and
  meaning-based search. `-project` scopes to one project; empty searches the
  whole store. `search` filters by `-type` (comma-separated
  `discovery`/`change`/`decision`/`summary`/`manual`), a date window, and
  `-order-by` (`date_desc`/`date_asc`; default relevance), and pages with
  `-offset`.
- **The MCP server** (`mcp`) exposes the same reads as tools, scoped to the
  current project unless `all_projects: true`: `important_workflow`,
  `search_observations`, `semantic_search_observations`,
  `observation_context`, `recent_observations`, `session_start_context`,
  `session_observations`, `file_observations`, `get_observations`,
  `timeline`, the one write tool `add_observation`, and `search_prompts` and
  `session_prompts` — always listed, but empty unless prompt persistence was
  enabled when the session ran. List limits are capped at 100 per call.
- **`export` / `import`** — JSON Lines backup and restore, and the
  SQLite-to-Postgres migration path: `export` from one backend, `import` into
  the other. Import is idempotent (rows dedupe on `content_hash`) and keeps
  original timestamps and embeddings.
- **`prune`** — `-older-than-days N` deletes observations (and stored
  prompts) older than the cutoff; `-relative-paths` instead strips relative
  entries from `files_read`/`files_modified` (rows written before paths were
  canonicalized, which `file-context` can never match) and keeps the rows.
  Both are dry runs until `-yes`; `-project` scopes either.
- **`reembed`** — re-embeds every observation with no embedding, or one whose
  dimension no longer matches the configured model (probed live). Dry run
  until `-yes`, because each row costs an Ollama call.
- **`version`** — exact commit and build time from Go's VCS stamping, with a
  `-dirty` marker for an uncommitted tree.
- **The worker daemon** — `start` (from `SessionStart`) spawns a detached
  `worker` if none is reachable, replaces one running an older build or
  writing to the built-in store while the session is configured for another,
  and is guarded by a spawn lock. `-max-concurrent` (default 4) is the number
  of observer sessions kept cached; a session arriving past it evicts the
  idlest cached one. To survive crashes and reboots, install the systemd or
  launchd template — see [docs/hooks.md](docs/hooks.md#running-the-worker-as-a-supervised-service).

## Backends

**SQLite** (default) — a single file, zero dependencies, pure Go
(`modernc.org/sqlite`). FTS5 keyword search and brute-force cosine similarity
for semantic search. Opened with WAL journal mode, a 5s busy timeout, and
foreign keys on, all as DSN parameters so every pooled connection gets them;
the daemon and every CLI subcommand open their own connection to the same
file.

**Postgres + pgvector** (`postgres://...` DSN) — `tsvector`/GIN full-text
search and an HNSW index for semantic search, for stores that outgrow a linear
scan. `docker-compose.yml` brings up `pgvector/pgvector:pg16` with tuned
memory settings. Setup, DSN forms, the `-hnsw-ef-search` knob, and the
measurements behind its default are in [docs/postgres.md](docs/postgres.md).

## Known limitations

- **SQLite semantic search is a linear scan.** Fine at the scale one
  project's observations reach; it will not scale to millions of rows. Use
  the Postgres backend once scale is a real concern.
- **The schema is a narrower subset** of claude-mem's `observations` table:
  what an observation contains plus a content-hash dedup key, not the
  multi-device sync bookkeeping. Both backends have a versioned migration
  path for additions — see [docs/development.md](docs/development.md#schema-migrations).
- **`Setup` only builds the binary.** `scripts/ensure-binary.sh` builds the
  plugin binary from the shipped source when it is missing or not runnable; a
  static Go binary has no runtime to version-check the way real claude-mem's
  Setup does. Setup does not fire under `claude -p`, so `scripts/run-hook.sh`
  also announces a missing binary at `SessionStart`.
- **The Stop-hook summary is best-effort.** It queries the daemon for
  in-flight work and waits for the count to settle, but the hook is
  fire-and-forget and its wait is bounded. See [docs/hooks.md](docs/hooks.md#stop).

## Security

[SECURITY.md](SECURITY.md) covers the trust model (no auth on the MCP server
or worker socket — the boundary is the local OS user; the one network surface
is the opt-in Prometheus endpoint, which should stay on localhost), what is
hardened (DSN redaction, bounded inputs, panic recovery), what is out of scope
(encryption at rest, rate limiting), and how to report a vulnerability.

## Testing

```sh
go test ./...          # every package that does not need claude, Ollama, or Docker
go test ./... -race

# Postgres tests need a database and will not guess at one:
docker compose up -d
docker compose exec postgres createdb -U claudemem claudemem_test
CLAUDE_MEM_GO_TEST_POSTGRES_DSN=postgres://claudemem:claudemem@localhost:55432/claudemem_test?sslmode=disable \
  go test ./internal/memory/postgres/... -v
```

Point `CLAUDE_MEM_GO_TEST_POSTGRES_DSN` at a throwaway database, never one you
use: the tests write real rows. Without the variable the Postgres tests skip.
What the suite covers, the Linux and CI setup, and the release process are in
[docs/development.md](docs/development.md).

## Documentation

- [docs/hooks.md](docs/hooks.md) — each hook, the worker daemon, privacy, excluded projects, supervised service.
- [docs/postgres.md](docs/postgres.md) — Postgres/pgvector setup, DSNs, search behaviour, tuning, operations.
- [docs/plugin-install.md](docs/plugin-install.md) — install and uninstall, what gets wired, the self-healing binary, troubleshooting.
- [docs/development.md](docs/development.md) — layout, building, testing, schema migrations, releases, CI.
- [docs/findings.md](docs/findings.md) — the record of defects found and fixed, with how each was verified.
- [bench/recall/README.md](bench/recall/README.md) — the ANN recall benchmark harness and its measurements.
- [CHANGELOG.md](CHANGELOG.md) and [SECURITY.md](SECURITY.md).

## License

MIT — see [LICENSE](LICENSE).
