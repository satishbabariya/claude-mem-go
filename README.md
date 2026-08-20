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
                                                            store (SQLite: FTS5 + vectors)
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
```

Data lives in `~/.claude-mem-go/` — `observations.db`, `worker.sock`,
and `worker.log` / `start.log` / `hook.log` (hooks run detached from any
terminal, so these logs are the only way to see what they did).

## Known limitations

- **Semantic search is brute-force cosine similarity**, not an ANN index —
  fine at the scale one project's observations realistically reach,
  won't scale to millions of rows the way a real vector DB would.
- **The SQLite schema is a narrower subset** of claude-mem's real
  `observations` table (40+ migrations' worth of sync/dedup/content-hash
  columns are not replicated here) — this persists what an observation
  actually *contains*, not claude-mem's full sync/dedup machinery.
- **No MCP server / skills surface yet** — this covers the
  capture-and-search loop, not claude-mem's full plugin surface.

## Testing

```sh
go test ./...          # all packages that don't need `claude`/Ollama at all
go test ./... -race
```

Packages with pure logic (`classify`, `pool`, `store`, `transcript`,
`observer`'s retry policy, `worker`'s spawn-lock) have real unit tests.
`claude-agent-sdk-go`'s `Session`/`Query` and this project's end-to-end
worker/hook flow are validated by hand against the real `claude` CLI and
Claude Code hooks (see the project history) rather than mocked — mocking
the subprocess protocol would test the mock, not the thing that actually
breaks.
