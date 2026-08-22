# Postgres + pgvector backend

Everything about running claude-mem-go against Postgres instead of the default
SQLite file: choosing between the two, bringing up the container, the
`CLAUDE_MEM_DB` variable that makes an installed plugin use it at all, DSN and
TLS handling, the `-hnsw-ef-search` recall knob, and the measurements behind
the tuning advice. The recall harness itself lives in
[`bench/recall/README.md`](../bench/recall/README.md).

## Choosing a backend

`memory.Backend` is one interface with two implementations, selected by what
the `-db` flag looks like:

- **SQLite** (default, zero dependencies) — a file path. FTS5 keyword
  search, brute-force cosine similarity for semantic search. Opened with
  WAL journal mode, a 5s busy-timeout, and foreign keys on — all three
  fixed real, reproduced problems: this project's actual shape is the
  worker daemon and every CLI subcommand each opening their own connection
  to the same file, and the default rollback-journal mode's exclusive
  write lock made a second concurrent writer fail immediately with
  "database is locked." Separately, SQLite's foreign-key enforcement
  defaults to off regardless of what the schema declares, which meant
  `observation_vectors`' `ON DELETE CASCADE` had never actually fired —
  `prune` was silently leaving orphaned embedding rows behind. Both
  applied via DSN params (`_journal_mode`, `_busy_timeout`,
  `_foreign_keys`), not a one-time `PRAGMA` `Exec` call, since the latter
  only reaches whichever single pooled connection happens to run it.
- **Postgres + pgvector** (`postgres://...` DSN) — real full-text search
  (`tsvector`/GIN, no hand-written query sanitizer needed — Postgres
  tokenizes punctuation like the hyphen in "claude-mem" sanely by default,
  unlike SQLite's FTS5) and a real ANN index (HNSW) for semantic search
  instead of a linear scan. `docker-compose.yml` brings up
  `pgvector/pgvector:pg16`; every claim above (the HNSW index actually gets
  used, not just created; the hyphen query that broke FTS5 works here
  without a workaround) was checked with `EXPLAIN` and real queries against
  that container, not assumed. The container has a persistent named
  volume (data survives `docker compose down`) and `restart:
  unless-stopped`, so it comes back after a Docker Desktop/daemon restart
  without manual intervention — confirmed the policy actually applies via
  `docker inspect`, and confirmed Docker's real distinction between "you
  explicitly stopped it" (an explicit `docker stop`/`docker kill` — this
  policy correctly does NOT override that; verified directly) and a crash,
  which it would restart from.

## Setup

```sh
docker compose up -d

# Set this once, in the profile your shell (and therefore Claude Code)
# actually reads. It is what makes the plugin use Postgres at all:
export CLAUDE_MEM_DB="postgres://claudemem:claudemem@localhost:55432/claudemem?sslmode=disable"

./claude-mem-go ingest        # -db still works, and still wins over the env var
```

**`CLAUDE_MEM_DB` is not a convenience — without it an installed plugin
cannot reach Postgres at all.** Every subcommand takes `-db`, but nothing
that runs inside a plugin install can pass it: `hooks/hooks.json` invokes
the binary as `"$CLAUDE_PLUGIN_ROOT/claude-mem-go" start` (and `context`,
`prompt-context`, `file-context`, `hook`, `stop`), and `.mcp.json` execs
`... mcp` — no flags, and nowhere to add them that survives a plugin
update.

The result was a split brain, measured here before it was fixed: `doctor`
with no flag reported `~/.claude-mem-go/observations.db`, while `doctor
-db postgres://…` reported the Postgres store. Every hook and every MCP
tool call took the first branch, so an operator could follow this section
exactly, stand up Postgres, install the plugin, and have all of their
memory silently go to SQLite — with no error, and nothing anywhere naming
the second database.

## DSNs, TLS, and password redaction

  `sslmode=disable` above is for the local Docker Compose container only
  (it's on `localhost`, nothing else can see that traffic). Every DSN
  example in this repo uses it for the same reason — pgx's `stdlib` driver
  already honors the standard libpq `sslmode` parameter, so no code change
  is needed to use TLS, just don't copy `sslmode=disable` into a DSN that
  crosses a real network: use `sslmode=require` (encrypted, no certificate
  verification) or `sslmode=verify-full` (encrypted and verified,
  recommended for anything production) against a real Postgres instance.
  A real Postgres DSN carries its password in plaintext — every place a
  `-db` value could reach a log line or stdout (`doctor`'s output, the
  `context`/`stop`/`file-context` hook logs, the error `postgres.Open`
  returns and every caller further up the stack that logs it) now goes
  through `store.RedactDSN` first, replacing the password with
  `REDACTED`. This was a genuine leak before, not a hypothetical one, and
  the first version of the redaction function itself had a real bug: it
  used `net/url.Parse`-then-reserialize and fell back to returning the
  *unredacted* original whenever parsing failed — exactly backwards for a
  redaction function — which a deliberately malformed DSN (a well-formed
  `user:password@` prefix followed by a broken host) triggered
  immediately. Fixed with a regex-based approach that only needs the
  `scheme://user:password@` prefix to be well-formed, independent of
  whatever comes after — verified against the exact malformed-DSN case
  that leaked, and against a real live-container auth failure.

## `-hnsw-ef-search`: the query-time recall/speed knob

  `-hnsw-ef-search` (on `semantic-search`, `prompt-context`, and `mcp`)
  overrides pgvector's own `hnsw.ef_search` query-time recall/speed
  tradeoff — the mandate's own "real ANN vector search... at scale"
  otherwise has no actual knob once `observations` grows well past the
  row counts pgvector's built-in default (40) was tuned against. Building
  it surfaced a genuinely surprising real behavior: pgvector's documented
  1..1000 bound on this value is **not reliably enforced by Postgres
  itself**. `hnsw.ef_search` is a custom GUC pgvector's extension
  registers, and until something on a given backend connection has
  already touched the vector extension, Postgres treats the name as an
  unchecked placeholder — reproduced directly, identical Go code (`BEGIN`,
  `SET LOCAL hnsw.ef_search = 1001`, `COMMIT`) correctly errored through a
  connection a prior real `Insert`/`SaveEmbedding` call had already warmed
  up, but silently accepted the exact same invalid value with no error at
  all on an otherwise-idle fresh connection whose first-ever query was
  that `SET LOCAL`. A caller with a typo'd or misconfigured value would
  have no reliable way to notice, since whether Postgres rejects it
  depends on incidental connection warm-up state, not the value itself.
  Fixed by validating the range in Go at `Open` time instead of ever
  trusting Postgres to catch it, so a bad value fails the same way every
  time regardless of connection state. The override itself is applied via
  a transaction-scoped `SET LOCAL`, not a plain `SET`, against the pooled
  connection every backend call shares — `database/sql` gives no control
  over which physical connection any one call gets, so a plain `SET`
  would silently persist onto whatever unrelated query the pool next
  hands that same connection. Verified against the real container: the
  out-of-range rejection, and that a valid override doesn't leak past its
  own call (checked on a pool forced to a single connection, so this is
  deterministic rather than merely likely).

## Measured at scale: 250,000 observations, 768-dimension vectors

"At scale" was claimed here long before it was demonstrated, so it was
measured: a 250,000-row corpus in the real Docker container, built
through the app's own schema and migrations, queried through the real Go
code paths rather than by hand-written SQL. Table plus indexes came to
~1.1GB (the HNSW index alone is 531MB).

| read path | latency |
|---|---|
| `SemanticSearch` unscoped, top 10 (ANN) | **1.0ms** |
| `SemanticSearch` scoped to a project | **2.0ms** |
| `RecentByProject(20)` | 0.7ms |
| `CountByProject` | 0.5ms |
| `Search` enumerate + date window | 10.5ms |
| `Search` enumerate at offset 5000 | 3.3ms |
| `Search` keyword, project-scoped | 12.3ms |
| `Search` keyword, unscoped | 70.9ms |

`EXPLAIN` confirms the HNSW index is genuinely chosen at this size
(`Index Scan using idx_observations_embedding_hnsw`), which small-scale
tests cannot show — the planner will not pick it on a few thousand rows.
The one number worth watching is unscoped keyword search at 70.9ms: the
term used matches roughly a fifth of the corpus, and ranking 50,000
matches by `ts_rank_cd` is inherently more work than ranking the 5,000 a
project scope leaves. Scoped search, which is what the hooks actually
run, is 12.3ms.

Recall is **not** quoted for this run: these vectors are randomly
generated, and random high-dimensional vectors are near-orthogonal — a
degenerate case for HNSW, so a figure from them would say nothing about
real use. It is measured separately, against real embeddings, by
`bench/recall` (see that directory's README), against a forced exact scan
over the same rows.

An earlier version of this section quoted **95.0% recall@10** from that
harness. **That figure was wrong and has been retracted** — the corpus
could only produce 1,440 distinct sentences regardless of row count, so
it was mostly duplicate vectors, and recall counted as id overlap was
really measuring how two query plans broke ties among identical
distances. Both defects are fixed; `bench/recall/README.md` documents
them in full rather than quietly deleting the number.

The corrected measurement, on 20,000 distinct real `nomic-embed-text`
embeddings:

| `hnsw.ef_search` | recall@10 | scoped recall@10 |
|---|---|---|
| 40 (pgvector default) | **80.0%** | **71.2%** |
| 200 | 94.0% | exact (planner skips HNSW) |
| 400 | 98.0% | exact (planner skips HNSW) |

**pgvector's default is not good enough at this size**, and the
project-scoped path every hook actually runs is the weaker of the two —
roughly 71%, meaning about three relevant memories in ten silently
missing. `-hnsw-ef-search 200` fixes both, for under 4ms p50, and
`doctor` now flags any store past 10,000 embedded rows still on the
default. The default itself is deliberately unchanged: one corpus on one
embedding model is not enough evidence to alter search behaviour for
every existing store.

Three real problems surfaced only at this size, all now fixed:

- **`ObservationsForFile` had no usable index.** It runs before every
  `Read` tool call, and the jsonb containment test was a post-filter over
  every row in the project — 47.7ms with a plan reading `Rows Removed by
  Filter: 4687`. Migration 4 adds GIN indexes on `files_read` and
  `files_modified`: 12.9ms, and the cost now scales with matches instead
  of with project size.
- **`maintenance_work_mem` was far too small.** At the stock 64MB
  pgvector reported *"hnsw graph no longer fits into maintenance_work_mem
  after 16759 tuples"* — index builds degrade at seventeen thousand rows.
  Raised to 512MB in `docker-compose.yml`, which moves the spill point to
  141,896 tuples and cuts the 250k build from 73s to 40s. It still spills
  at 250k, so a real deployment should go higher again; the point is that
  the shipped default is not a starting point.
- **Docker's default 64MB `/dev/shm` made raising it impossible.**
  Postgres puts parallel workers' shared memory there, so the build died
  with `could not resize shared memory segment ... No space left on
  device` — an error whose text gives no hint that the container's shm
  size is the cause. `shm_size: 1gb` is now set explicitly.

## Related findings

The Postgres-specific defects found and fixed in this port, each with its
reproduction, are recorded in [findings.md](findings.md):

- [No statement timeout — a single hung query could wedge the entire connection pool](findings.md#the-postgres-backend-had-no-statement-timeout--a-single-hung-query-could-wedge-the-entire-connection-pool)
- [No per-attempt timeout on the initial connection ping](findings.md#the-initial-connection-ping-had-no-per-attempt-timeout--a-firewalled-or-black-holed-postgres-could-hang-every-retry-not-just-the-query-timeout-above)
- [Pool size and idle timeout were hardcoded, and the idle timeout was 10x off](findings.md#the-postgres-pools-size-and-idle-timeout-were-hardcoded--and-the-idle-timeout-was-10x-off-from-real-claude-mems-own-default)
- [Keyword search silently ignored `facts` and `concepts`](findings.md#postgres-keyword-search-silently-ignored-facts-and-concepts--a-measured-cross-backend-divergence)
- [Boolean search operators worked on SQLite and silently broke on Postgres](findings.md#boolean-search-operators-worked-on-sqlite-and-silently-broke-on-postgres)
- [The project filter could make semantic search return zero results](findings.md#the-postgres-backends-project-filter-could-make-semantic-search-return-zero-results)
- [Hard-wired to 768-dimension embeddings](findings.md#the-postgres-backend-was-hard-wired-to-768-dimension-embeddings-and-said-so-in-a-comment-that-was-wrong)
