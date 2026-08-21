---
name: mem-doctor
description: Check whether claude-mem-go is actually working — worker daemon reachable, database reachable, Ollama reachable with the configured embedding model pulled. Use when the user asks "is memory working?", "why isn't context showing up?", "is claude-mem-go healthy?", or when observations/search/recall seem to be silently doing nothing.
---

# Memory Health Check

claude-mem-go's `doctor` subcommand is an operational health check, not a
search tool — reach for `mem-search` for "did we already do X," and this
skill for "is the memory system itself functioning."

## When to use

Use when the user is asking about the SYSTEM, not its contents:

- "Is claude-mem-go working?"
- "Why isn't it remembering anything?"
- "Is the worker running?"
- "Is semantic search available?"
- Recall/search seems to return nothing even for things that should be there.

Don't use this for "did we solve X before" — that's `mem-search`.

## What it checks

- `claude` CLI on `PATH` (the observer needs to spawn it)
- the worker daemon reachable on its Unix socket, and if so its real
  activity counters (`processed`/`duplicates`/`observer_errors`/
  `insert_errors`/`embed_errors`/pool utilization/`cached_sessions`) —
  informational, but the first place to look if observations seem to be
  silently going nowhere
- **what the store actually contains** — observation/project/session
  counts and how recent the newest one is. This is the only check that
  answers "is anything being remembered", as opposed to "is every
  component reachable". They come apart: `doctor` will happily report
  "All critical checks passed" while capture has been dead for weeks,
  because `PostToolUse` is fire-and-forget and a failing hook writes to a
  log nobody reads. If the counts look wrong or the newest observation is
  much older than the user's recent activity, say so — that is the
  finding, even when every other line has a check mark. `claude-mem-go
  stats` prints the same information in more detail.
- the database reachable (SQLite file or Postgres DSN, whichever `-db`
  points at), plus backend-specific facts nothing else surfaces:
  SQLite's real `journal_mode`/`foreign_keys`/`busy_timeout_ms` PRAGMA
  settings, or for Postgres, real connection-pool utilization, the
  installed `pgvector` extension version, whether the HNSW index
  semantic search actually depends on still exists (a missing HNSW
  index would otherwise silently degrade every semantic search to a full
  table scan with nothing saying so), and the configured
  `hnsw_ef_search` value (`default (40)` unless `-hnsw-ef-search` was
  passed to `doctor` — pass the identical value used on `mcp`/
  `semantic-search`/`prompt-context` to confirm it's actually what you
  think it is, not a live Postgres session setting, since the override
  only ever applies per query via a transaction-scoped `SET LOCAL`)
- `embedding_dims`/`embedding_dims_consistent` — whether every embedded
  observation shares the same vector dimension. If `false` (SQLite only;
  Postgres can't have this), the embedding model was changed at some
  point without re-embedding old rows: those older observations will
  never surface via `semantic_search_observations` again (silently, no
  error) until they're re-embedded. If the user hits this, the fix is
  re-ingesting/re-adding those observations with the current model — not
  something to do automatically without asking, since it means real API
  calls to Ollama for every affected row.
- Ollama reachable with the configured embedding model actually pulled
  (informational only — no Ollama just means no semantic search, not a
  broken install)

It distinguishes critical failures (exit code 1 — nothing works without
these) from informational ones (worker not running is fine, `start`
launches it lazily).

## How to run it

The binary's location depends on how claude-mem-go was installed:

```
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" doctor
```

If `$CLAUDE_PLUGIN_ROOT` isn't set (not running inside this plugin's
install, e.g. a manual dev checkout), fall back to whatever's on `PATH` or
the checkout's build output:

```
claude-mem-go doctor
```

Report the output back to the user plainly — it's already written for a
human to read directly, not something to re-summarize or reformat.
