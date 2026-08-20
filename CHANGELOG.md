# Changelog

Grouped by rough milestone, not one entry per commit — `git log` is the
exact, granular history; this is the "what actually changed and why"
summary. Dates are when each milestone landed, not a formal release
process (this project doesn't cut tagged releases on a schedule).

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
- **`PreToolUse` (file-context) and `Stop` (session summary) hooks wired**,
  closing two of the previously-unwired hooks.
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
