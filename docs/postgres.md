# Postgres + pgvector backend

Running claude-mem-go against Postgres instead of the default SQLite file.
The backend is selected by the shape of the store setting: a file path is
SQLite, a `postgres://` URL or keyword/value DSN is Postgres. Use it when a
store outgrows SQLite's linear-scan semantic search or when several machines
share one memory.

## Contents

- [Setup](#setup)
- [DSNs, TLS, and redaction](#dsns-tls-and-redaction)
- [Schema and migrations](#schema-and-migrations)
- [Full-text search](#full-text-search)
- [Semantic search](#semantic-search)
- [Tuning](#tuning)
- [Measurements](#measurements)
- [Operations](#operations)
- [Limits](#limits)

## Setup

`docker compose up -d` brings up both services this project can use:
Postgres + pgvector, and Ollama with `nomic-embed-text` pulled into a named
volume by the one-shot `ollama-pull` service (~274MB, once). Nothing needs to
be installed but Docker. If you already run Ollama natively, leave that
service stopped (`docker compose up -d postgres`) and keep pointing
`CLAUDE_MEM_OLLAMA_BASE_URL` at your own instance — Docker has no GPU access
on macOS, so a native install is faster.

```sh
docker compose up -d

# Set this once, in the profile your shell (and therefore Claude Code) reads:
export CLAUDE_MEM_DB="postgres://claudemem:claudemem@localhost:55432/claudemem?sslmode=disable"

./claude-mem-go doctor        # confirms which store won: -db > $CLAUDE_MEM_DB > built-in path
```

`docker-compose.yml` runs `pgvector/pgvector:pg16` on port 55432 with a
persistent named volume, `restart: unless-stopped` (it comes back after a
Docker restart but not after an explicit `docker stop`), `shm_size: 1gb`, and
`shared_buffers=512MB`, `maintenance_work_mem=512MB`, `work_mem=16MB`,
`effective_cache_size=3GB` — sized for a ~7.7GB Docker VM shared with other
work. Raise them on dedicated hardware; see [Tuning](#tuning) for why the
stock defaults are not a starting point.

**`CLAUDE_MEM_DB` is required, not a convenience.** Every subcommand takes
`-db`, but an installed plugin invokes the hooks and the MCP server with no
flags, so nothing inside it can pass one. Without the variable every hook and
MCP tool writes to `~/.claude-mem-go/observations.db` while `doctor -db
postgres://…` reports Postgres, and memory silently splits across two stores.
`doctor` names which of the three sources selected the store for this reason.
A worker daemon started before the variable was set keeps writing to the old
store; `start` replaces such a daemon on the next `SessionStart` (see
[hooks.md](hooks.md#sessionstart)).

To move an existing SQLite store: `export -db ~/.claude-mem-go/observations.db -out backup.jsonl`,
then `import -db postgres://… -in backup.jsonl`. Import is idempotent and
keeps timestamps and embeddings.

## DSNs, TLS, and redaction

Both pgx DSN forms work:

- URL: `postgres://user:password@host:5432/db?sslmode=verify-full`
- keyword/value: `host=localhost port=55432 user=claudemem dbname=claudemem sslmode=disable`

The statement timeout (below) is appended to either form appropriately — as a
query parameter on a URL, as a space-separated token on keyword/value — and is
left alone if the DSN already sets `statement_timeout`. A URL that does not
parse is passed through unchanged so the real connection error surfaces.

`sslmode=disable` is for the local container only. pgx honours the standard
libpq `sslmode` and `PGSSLMODE`, so TLS needs no code change: use
`sslmode=require` (encrypted, no certificate verification) or
`sslmode=verify-full` (recommended for anything production) across a real
network.

A DSN carries its password in plaintext. Every place a store setting can reach
a log line or stdout — `doctor`, the hook logs, the stats file, the error
`postgres.Open` returns — goes through `memory.RedactDSN`, which replaces the
password with `REDACTED` using a regex that only needs the
`scheme://user:password@` prefix to be well-formed, so a malformed DSN is
still redacted rather than echoed.

## Schema and migrations

One `observations` table with `BIGSERIAL` ids, `TIMESTAMPTZ` `created_at`,
`created_at_epoch` in milliseconds, `jsonb` for `facts`, `concepts`,
`files_read`, `files_modified`, a `vector(N)` `embedding` column, a generated
`search_vector`, and a `content_hash` unique key for idempotent inserts. A
`user_prompts` table holds opt-in stored prompts. Migrations are numbered,
idempotent, recorded in `schema_migrations`, and applied on `Open`
(`internal/memory/postgres/postgres.go`):

| Version | Migration |
|---|---|
| 1 | initial observations table + indexes (btree on project and `created_at_epoch`, GIN on `search_vector`, HNSW on `embedding`) |
| 2 | `CHECK` constraint on `observations.type` (`discovery`, `change`, `decision`, `summary`, `manual`), wrapped in a `DO` block so it is idempotent |
| 3 | `search_vector` extended to `facts` and `concepts` at weight `D` (column dropped and re-added, which backfills every row) |
| 4 | GIN indexes on `files_read` and `files_modified` for the file-context read |
| 5 | `next_steps` column for session summaries |
| 6 | `user_prompts` table + `tsvector` index |

How the framework works and how to add a migration is in
[development.md](development.md#schema-migrations).

## Full-text search

`Search` uses `websearch_to_tsquery('english', …)` over `search_vector`
(weights: title/subtitle/narrative above `facts`/`concepts` at `D`), ranked by
`ts_rank_cd` then `id` so paging with `-offset` is deterministic. Uppercase
`AND`/`OR`/`NOT` are operators on both backends, matching SQLite's FTS5
behaviour: `NOT term` is rewritten to `-term` (the only negation
`websearch_to_tsquery` honours), and every non-operator token is quoted so
lowercase `or`/`not` are plain words here as they are in FTS5. Hyphenated
terms like `claude-mem`, `key:value`, and parentheses all match without a
sanitizer. One residual difference is deliberate: Postgres's `english`
configuration strips stopwords and FTS5 does not, so a query made entirely of
stopwords can match differently.

## Semantic search

`SemanticSearch` orders by pgvector's cosine-distance operator (`<=>`) under
the HNSW index and reports `1 - distance` so scores match SQLite's cosine
similarity. The planner only chooses the index once a table is large enough
(around 10,000–20,000 rows); below that a sequential scan is exact.

**`hnsw.ef_search`** is the query-time recall/speed knob. The default here is
**200** (`postgres.DefaultHNSWEfSearch`), not pgvector's 40: on 20,000 real
`nomic-embed-text` embeddings, 40 measured 80% recall@10 unscoped and 71.2%
project-scoped — the path every hook takes, losing about three relevant
memories in ten with no error — while 200 measured 94% (95% on the latest
run) for ~0.3ms more, under 4ms p50 throughout. Override it with
`-hnsw-ef-search` on `semantic-search`, `prompt-context`, `mcp`, and
`doctor` (the last only so its health output reflects the value configured
elsewhere). It is applied per call with a transaction-scoped `SET LOCAL`, not
a session `SET`, because `database/sql` gives no control over which pooled
connection a call gets. The 1–1000 range is validated in Go at `Open`: Postgres
treats `hnsw.ef_search` as an unchecked placeholder on a connection that has
not yet touched the vector extension, so an out-of-range value would be
accepted or rejected depending on connection warm-up.

**Project scoping** uses pgvector 0.8's `hnsw.iterative_scan = strict_order`.
A project predicate is a post-filter on the HNSW walk: pgvector collects
`ef_search` globally-nearest candidates and then drops the ones from other
projects, so when a project's rows are not among the global nearest the
result is empty. Reproduced with 60,000 embedded rows in the queried project
and 20,000 nearer rows in another: 0 results, `Rows Removed by Filter: 40`.
Iterative scan keeps walking until enough rows survive the filter (~26ms on
that reproduction). On pgvector older than 0.8, detected once at `Open` from
`pg_extension`, `SemanticSearch` falls back to a `MATERIALIZED` CTE
pre-filter — exact but O(rows in project), 2.7s on the same data — because an
unsupported `hnsw.*` GUC errors on a warmed connection rather than degrading.
Unscoped searches need neither.

## Tuning

Pool and timeout settings use the same environment variable names and
defaults as real claude-mem, so settings migrate between the two:

| Variable | Default | Effect |
|---|---|---|
| `CLAUDE_MEM_POSTGRES_STATEMENT_TIMEOUT_MS` | 30000 | `statement_timeout` on every connection, so a hung query (lock contention from `prune`/`reembed`, a pathological plan, a network stall) releases its connection instead of wedging the 10-connection pool shared by every hook and the daemon |
| `CLAUDE_MEM_POSTGRES_CONNECTION_TIMEOUT_MS` | 5000 | Bound on each initial ping attempt, so a host that accepts TCP and never answers fails in seconds rather than per-OS TCP timeouts on every retry |
| `CLAUDE_MEM_POSTGRES_POOL_MAX` | 10 | `SetMaxOpenConns`; idle connections are capped at 5 |
| `CLAUDE_MEM_POSTGRES_IDLE_TIMEOUT_MS` | 30000 | `SetConnMaxIdleTime` |
| `CLAUDE_MEM_POSTGRES_EMBED_DIMS` | 768 | Width of the `vector(N)` column when the store is **created**; see [Dimension changes](#dimension-changes) |

`Open` retries its initial ping on a backoff of 0, 250ms, 500ms, 1s, 2s, 4s
(~7.75s of delay across 6 attempts, each bounded by the connection timeout),
so a hook or daemon starting a beat before the container's healthcheck passes
does not fail permanently.

Server-side, `maintenance_work_mem` governs HNSW index builds: at the stock
64MB pgvector reports *"hnsw graph no longer fits into maintenance_work_mem
after 16759 tuples"*. The compose file's 512MB moves the spill point to
141,896 tuples and cut a 250,000-row build from 73s to 40s; a real deployment
should go higher. Raising it needs `shm_size` above Docker's 64MB default, or
the build dies with `could not resize shared memory segment … No space left
on device`.

## Measurements

Full tables, method, and the retracted first result are in
[bench/recall/README.md](../bench/recall/README.md); CI guards the key
properties with 300 committed real embeddings. In summary: on 20,000 distinct
`nomic-embed-text` vectors, recall@10 rises monotonically from 80% at
`ef_search` 40 to 94% at 200 and 98% at 400, all under 4ms p50 (query
embedding itself costs ~33ms against local Ollama and dominates); scoped
recall at 40 is 71.2% and goes exact at 100+ because the planner abandons HNSW
for a bitmap scan over `idx_observations_project` on a 1,000-row project. At
250,000 rows with 768-dimension vectors (random, so recall is not quoted from
that run) the table plus indexes came to ~1.1GB (HNSW index 531MB), with
unscoped ANN search at 1.0ms, scoped at 2.0ms, `RecentByProject(20)` 0.7ms,
`CountByProject` 0.5ms, enumerate with a date window 10.5ms, enumerate at
offset 5000 3.3ms, project-scoped keyword search 12.3ms, and unscoped keyword
search 70.9ms (the term matched a fifth of the corpus). `ObservationsForFile`
went from 47.7ms to 12.9ms with migration 4's GIN indexes.

## Operations

**`doctor`** on a Postgres store reports pool utilization
(`pool_open_connections`, `pool_in_use`, `pool_idle`,
`pool_max_open_connections`), `vector_extension` version,
`hnsw_index_exists` (a schema drift would otherwise silently degrade every
semantic search to a table scan), `hnsw_ef_search` (`200 (default)` when
unset), `embedding_column_dims`, and the `embedding_dims` histogram with
`embedding_dims_consistent`. It warns when a store past 10,000 embedded rows
runs with `ef_search` lowered below 100, quoting the measured recall.

**`reembed`** re-embeds rows with no embedding or a dimension that does not
match the live model; dry run until `-yes`.

### Dimension changes

A pgvector column's width is fixed at creation. `CLAUDE_MEM_POSTGRES_EMBED_DIMS`
sizes it for a **new** store (e.g. 384 for `all-minilm`, 1024 for
`mxbai-embed-large`); `Open` reads the real width from the catalog and
`SaveEmbedding` fails with a message naming both numbers when a model does
not fit. To change models on an existing store, `export`, create a new store
with the variable set, `import`, then `reembed -yes`. `doctor`'s
`embedding_column_dims` is the number that decides whether a model can write
at all.

## Limits

- pgvector 0.8+ is needed for correct project-scoped ANN search at scale; older
  versions get the exact-but-slow CTE fallback.
- `hnsw.max_scan_tuples` (pgvector default 20,000) bounds how far an iterative
  scan walks; the benchmark shows scoped recall at the default `ef_search`
  holding at ~71–74% as that cap is lowered to simulate ~400,000 rows.
- Behaviour past 20,000 real rows, and projects large enough that the
  planner's exact fallback stops being cheap, are not measured.
- The store is shared across every project on a machine; tools and hooks
  scope by project, and `all_projects: true` is the explicit way out.
