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
- **MCP tool surface** grew from 2 tools to 6: `recent_observations`,
  `session_observations`, `file_observations` alongside
  `search_observations`/`semantic_search_observations`, plus
  `add_observation` — the surface's only *write* tool, letting Claude
  explicitly persist something worth remembering that isn't the direct
  result of one tool call (closing a real gap real claude-mem's own
  `observation_add` tool covers).
- **`PreToolUse` (file-context) and `Stop` (session summary) hooks wired**,
  closing two of the previously-unwired hooks.
- **`prune`** — retention command (dry-run by default), and **`export`/
  **`import`** — backup and the SQLite↔Postgres migration path. Both new;
  neither existed before.
- **Worker observability** — activity counters (`worker-stats.json`)
  surfaced through `doctor`, where before there was only raw log text.
- **`version` command** — build commit/time via Go's own VCS stamping.
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
