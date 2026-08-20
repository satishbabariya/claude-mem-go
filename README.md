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

- **worker** — a persistent daemon, meant to be started once (see `start`)
  and left running. Listens on a Unix socket, processes PostToolUse
  payloads through a bounded `pool` of observer sessions.
- **hook** — the thin client Claude Code's `PostToolUse` hook actually
  invokes: forward stdin to the worker's socket, exit. Deliberately does
  *no* observation work itself — see "Why the worker/hook split" below.
- **start** — idempotent daemon launcher for `SessionStart`: spawns a
  detached worker if one isn't already running, using a `worker-spawn-gate.ts`-style
  lockfile so concurrent sessions starting at once don't spawn duplicates.
- **ingest** — one-shot: read a real transcript file, observe N tool calls,
  persist them. Useful for backfilling or testing without wiring up hooks.
- **search** / **semantic-search** — keyword (FTS5) and meaning-based
  (local embeddings + cosine similarity) search over what's been persisted.
- **mcp** — an MCP server (stdio, JSON-RPC 2.0) exposing `search_observations`
  and `semantic_search_observations` as tools any MCP client — including
  Claude Code itself — can call directly. Wire format confirmed against a
  real `claude` session, not assumed from the spec (see `mcpserver/`'s doc
  comment); end-to-end tool calls verified against the real CLI too.

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
- **The SQLite schema is a narrower subset** of claude-mem's real
  `observations` table (40+ migrations' worth of sync/origin-device
  bookkeeping and an FTS5 shadow table are not replicated here) — this
  persists what an observation actually *contains* plus a content-hash
  dedup key, not claude-mem's full multi-device sync machinery.
- **No skills surface** — the MCP server exposes search/recall as tools;
  claude-mem's broader plugin surface (skills, slash commands) has no
  analog here yet.

## Testing

```sh
go test ./...          # all packages that don't need `claude`/Ollama/Docker
go test ./... -race

docker compose up -d   # then postgres/... runs against the real container
go test ./postgres/... -v
```

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
