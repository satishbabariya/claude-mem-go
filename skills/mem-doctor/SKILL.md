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
- **two critical findings about the install itself**, which mean capture
  is configured and not working — not that the store is damaged:
  - `✘ plugin binary unusable` — the plugin is installed but its binary
    cannot run (missing, wrong architecture, or a partial copy). Every
    hook resolves that binary, so nothing is being captured at all.
    Rebuilding it in the plugin root is the fix.
  - `✘ the store is EMPTY, but the plugin is installed` — capture is
    wired up and has never recorded anything. Distinct from the ordinary
    "the store is empty" note, which is expected when the plugin is not
    installed. Treat this as a real problem and look at the hook logs in
    `~/.claude-mem-go/`.

- **three findings about the worker daemon that are easy to misread** —
  all of them describe a daemon that is running but disagrees with the
  command you just ran. Each has a specific meaning, and none of them
  means "the store is broken":
  - `✘ the worker daemon is writing to a DIFFERENT store than this
    command reads` — **critical, and the most misleading symptom in the
    whole tool.** The daemon opened its store once at startup and never
    re-reads `$CLAUDE_MEM_DB`. Every automatic capture path goes through
    it, so observations are landing in the daemon's store while searches
    and `stats` read the other one. The usual cause is that
    `$CLAUDE_MEM_DB` changed after the daemon started. Report this as
    "memory is being written somewhere else", not as data loss — nothing
    is lost, it is in the store `doctor` names. The fix is to restart the
    daemon (stop it; the next SessionStart respawns it).
    `start` replaces a daemon that predates the user's `$CLAUDE_MEM_DB`
    on the next SessionStart (daemon on the built-in path, session
    configured elsewhere). When both stores are explicit it deliberately
    does not — that would thrash between them — so tell the user to stop
    the daemon by hand if they really reconfigured.
  - `… the worker daemon is running an older build` — the daemon applies
    the rules it started with, including how project names are derived,
    so writes and reads can silently disagree. SessionStart replaces it
    automatically, so this normally resolves itself; only flag it if it
    persists.
  - `… all N observer slot(s) are held by cached sessions` — an arriving
    session evicts the idlest cached one and takes its slot, so nothing
    is dropped, but sessions that keep displacing each other respawn
    their observer on every tool call (slower capture, more spend).
    Relevant when the user runs more than N Claude Code sessions at
    once. The fix is a larger `-max-concurrent`.

  None of these appear when the daemon is not running: they are claims
  about a live daemon, and `doctor` reports the stats of a stopped one as
  "last worker activity before it stopped" rather than as current state.

- the database reachable (SQLite file or Postgres DSN, whichever `-db`
  points at), plus backend-specific facts nothing else surfaces:
  SQLite's real `journal_mode`/`foreign_keys`/`busy_timeout_ms` PRAGMA
  settings, or for Postgres, real connection-pool utilization, the
  installed `pgvector` extension version, whether the HNSW index
  semantic search actually depends on still exists (a missing HNSW
  index would otherwise silently degrade every semantic search to a full
  table scan with nothing saying so), and the configured
  `hnsw_ef_search` value (`200 (default)` unless `-hnsw-ef-search` was
  passed to `doctor` — pass the identical value used on `mcp`/
  `semantic-search`/`prompt-context` to confirm it's actually what you
  think it is, not a live Postgres session setting, since the override
  only ever applies per query via a transaction-scoped `SET LOCAL`)
- **`recall[...]` lines — whether memory is actually being FOUND.** Every
  other counter here describes writes. These describe reads, broken down
  by which hook did the reading, because an empty result means opposite
  things depending on where it came from:
  - `recall[session]` — SessionStart's context injection. Persistently
    empty means sessions are starting with **no memory at all**, the most
    consequential silent failure this system has.
  - `recall[prompt]` — UserPromptSubmit's semantic recall. Persistently
    empty on a project that HAS observations means semantic search is
    reaching none of them (a missing embedding model, unembedded rows, or
    an `ef_search` too low — see the next bullet).
  - `recall[file]` — the PreToolUse file lookup. **Mostly empty is
    normal** here: most files have no prior observations. It is reported
    for completeness and deliberately excluded from the warning below.

  `doctor` warns only when a majority of at least 10 *session/prompt*
  recalls came back empty. On a young store that is simply correct and
  not a fault; on a store with real content it means memory is not
  reaching it.
- **a recall warning on large Postgres stores** — past 10,000 embedded
  rows still on pgvector's default `ef_search`, `doctor` reports that
  semantic search is measurably missing results: `bench/recall` measures
  80% recall@10 unscoped and ~71% for the project-scoped searches every
  hook actually runs, meaning roughly three relevant memories in ten are
  silently absent. Nothing else surfaces this — the queries succeed, they
  just return less than the store holds. The remedy is
  `-hnsw-ef-search 200` on `mcp`/`semantic-search`/`prompt-context`
  (measured under 4ms p50). Not a failure and not counted as one: it is a
  tuning finding on a working system, and the default is deliberately
  left as pgvector ships it.
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
