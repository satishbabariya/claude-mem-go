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
- **`PreToolUse` (file-context), `Stop` (session summary), and
  `UserPromptSubmit` (prompt-context) hooks wired**, closing three of the
  previously-unwired hooks. `UserPromptSubmit` is the sharpest of the
  three: it embeds the actual submitted prompt text and injects the
  observations semantically closest to *that specific question*, rather
  than `SessionStart`'s static "recent observations" dump — the same real
  gap real claude-mem's own `session-init` handler covers. Required a new
  `Prompt` field on `claude-agent-sdk-go`'s `HookInput` (confirmed against
  a real captured payload, tagged `v0.1.1`). Verified against a real,
  isolated Ollama call and a real `claude` CLI session: two topically
  distinct seeded observations, a prompt with no keyword overlap with
  either, and Claude's answer correctly identified the semantically
  relevant one and explicitly attributed it to injected memory. `Setup`
  remains unwired — real claude-mem uses it for Node/Bun version-checking,
  which has no equivalent for a single static Go binary.
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
- **`timeline`** — MCP tool #8: chronological context AROUND one
  observation (`depth_before`/`depth_after` observations surrounding an
  anchor), mirroring real claude-mem's own `timeline` tool ("step 2: get
  context around results"). Give it an `anchor` directly or a `query` to
  resolve one automatically via a single-result keyword search. New
  `Backend.Timeline` method in both backends, ordered by `id` rather than
  `created_at_epoch` (confirmed in both SQLite's rowid and Postgres's
  `BIGSERIAL` that id increases monotonically with insertion order,
  rather than assumed). Always scoped to the anchor's own project, not
  the caller's project argument at face value — the same cross-project
  leak class fixed for `Search`/`ByIDs` earlier, with its own dedicated
  regression test. Verified end to end against a real `claude` CLI
  session for both the direct-anchor and query-resolution paths.
- **`get_observations`/`Backend.ByIDs` capped at 100 IDs per call**
  (`store.MaxIDsPerLookup`). Found the hard way: a real test against this
  project's own SQLite driver showed 100,000 IDs failing outright with a
  raw "too many SQL variables" error, since the hand-built
  `IN (?,?,...)` placeholder list has no cap of its own. Enforced in both
  backends for parity (Postgres's `= ANY($1)` doesn't hit the same limit,
  but gets the identical bound anyway), and confirmed at the MCP protocol
  boundary that an oversized request comes back as a clean tool error
  mentioning the limit, not a raw driver error.
- **Hook payloads are now size-bounded** (`hook.MaxPayloadBytes`, 8MB,
  matching the MCP server's own JSON-RPC line cap) on both ends of the
  worker socket. Before this, an abnormally large `tool_response` (a
  `Bash` command catting a multi-gigabyte file) had no upper bound at
  all — a real risk specifically because the worker daemon is one
  long-lived process every project on the machine shares, so a single
  pathological tool call could balloon its memory for every other
  session using it too. Rejected whole, not truncated (a truncated JSON
  hook payload is corrupt, not just short). Verified against a real
  running daemon: sent an oversized payload over its actual socket,
  confirmed it logged a rejection and stayed alive, then confirmed a
  normal-sized payload right after processed correctly.
- **Skill docs re-synced with the actual tool surface** — `mem-search`
  still said "six tools" and never mentioned `get_observations` at all
  (added a whole iteration earlier and simply never back-filled into the
  skill instructions), and `mem-doctor`'s "what it checks" list predated
  both the worker activity counters and `Backend.HealthDetails()`, so it
  described a `doctor` that no longer matched what the binary actually
  prints. Confirmed the fix against the real tool count (`grep` against
  `mcpserver.go`'s actual tool definitions: 7, matching the corrected
  doc) rather than just editing prose.
- **`postgres.Open` retries its initial connection** on a short backoff
  (~7.75s worst case across 6 attempts) instead of failing permanently on
  the very first ping. Every real caller (the worker daemon, every CLI
  hook, the MCP server) passes a context with no deadline of its own, so a
  common startup race — this daemon, or a hook, starting a beat before
  Postgres's own container finishes its healthcheck — would otherwise
  hard-fail every time it happened to lose that race; a real, sustained
  outage is still the process supervisor's job (systemd/launchd's
  `Restart=on-failure`), not this retry loop's. Verified against the real
  docker-compose container: stopped it, called `Open`, restarted it
  mid-retry, confirmed `Open` recovered instead of failing on the first
  attempt.
- **Real binary releases** — `.goreleaser.yaml` + a tag-triggered
  `.github/workflows/release.yml` cross-compile `claude-mem-go` for
  linux/darwin × amd64/arm64 and attach them (plus a `checksums.txt`) to a
  real GitHub Release on every `vX.Y.Z` tag push. Before this, installing
  meant `go build` locally with no other option. Verified with a real
  snapshot build (no tag needed): all four targets compiled, and the
  darwin/arm64 binary was extracted and actually run — `version` and
  `doctor` both worked correctly.
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
