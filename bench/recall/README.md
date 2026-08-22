# Recall benchmark

Answers a question this project asserted for a long time without
measuring: **does the HNSW index actually return what an exact search
would?** "Production-grade ANN" is a claim about recall, not just latency.

An earlier scale run deliberately refused to quote a recall figure,
because its 250,000 vectors were randomly generated — random
high-dimensional vectors are near-orthogonal, which is a degenerate case
for HNSW and would have produced a number that says nothing about real
use. This harness uses real embeddings of real engineering prose from a
local Ollama model instead, which is why its numbers mean something.

## Method

`seed` writes N observations of varied engineering prose and embeds each
one for real (fixed RNG seed, so the corpus is reproducible). `measure`
then, for each of ten natural-language queries phrased *differently* from
the corpus — so it measures semantic retrieval rather than string overlap
— compares two result sets over the same rows:

- **exact**: `enable_indexscan`/`enable_bitmapscan` off, forcing a full scan
- **ANN**: `enable_seqscan` off, forcing the HNSW index

`recall@10` is how much of the exact top-10 the ANN top-10 recovers.
Forcing both plans is what makes this a fair comparison at any corpus
size: below roughly ten thousand rows the planner picks a sequential scan
on its own, so a "natural" query would silently measure exact-vs-exact
and report a meaningless 100%.

## Running it

```sh
docker compose up -d
createdb ... # any throwaway database
go run ./bench/recall/seed    "postgres://…/throwaway?sslmode=disable" 20000
go run ./bench/recall/measure "postgres://…/throwaway?sslmode=disable"
```

Seeding is bounded by Ollama, measured here at ~30 embeddings/second, so
20,000 rows takes about eleven minutes.

## A retracted result, and why it was wrong

**The first version of this harness reported 95.0% recall@10 at 3,000
rows. That number was wrong and has been withdrawn.** It is documented
here rather than quietly deleted, because the failure is the kind that
produces a confident, plausible, completely meaningless figure.

Two defects combined:

1. **The corpus could not be unique.** The generator drew from 15
   subjects x 8 verbs x 12 objects — a hard ceiling of 1,440 distinct
   sentences regardless of row count. A 20,000-row corpus held 1,440
   distinct vectors, each repeated about fourteen times.
2. **Recall was counted as id overlap.** When the nearest text appears
   fourteen times, the exact top-10 is ten copies of ONE vector at an
   identical distance. Which ten ids come back is arbitrary, and the
   sequential scan and the HNSW walk break that tie differently. The
   metric was reporting tie-break agreement, not retrieval quality.

What exposed it was a physical impossibility rather than a failing test:
at 20,000 rows the curve read 84 -> **100** -> 81 -> 83 -> 85. Recall
cannot fall as `ef_search` rises, because a larger candidate list
explores strictly more of the graph. The 3,000-row corpus was duplicated
too (1,252 distinct across 3,000), just mildly enough that the resulting
curve looked flat and believable instead of obviously broken.

Both defects are now fixed independently, so neither alone can bring the
failure back: the seeder appends a real `file:line` detail that makes
every text unique, and `recallAt` counts how many ANN results are at
least as close as the exact k-th distance — a definition that is correct
even if some future corpus does contain ties. `measure` additionally
**refuses to run** when the corpus is under 99% distinct, and **fails**
if recall ever drops as `ef_search` rises, rather than printing a number
no one can tell is broken.

## Measured

20,000 real `nomic-embed-text` embeddings (768 dimensions), all distinct,
pgvector 0.8.6, cosine distance. Recall is distance-based, not id
overlap.

| `hnsw.ef_search` | recall@10 | p50 query |
|---|---|---|
| 20 | 71.0% | 1.5ms |
| 40 (pgvector default) | **80.0%** | 2.2ms |
| 100 | 92.0% | 2.1ms |
| 200 | 94.0% | 3.9ms |
| 400 | 98.0% | 3.9ms |

The curve is now monotonic, which is the first thing to check: recall
cannot fall as `ef_search` rises.

One row of this table has been corrected, and the reason is worth
keeping. `ef_search` 100 originally measured 82.0%; after the corpus was
bulk-`UPDATE`d to redistribute projects for the scoped benchmark and then
`VACUUM ANALYZE`d, it reproduces at 92.0% across three consecutive runs.
Every other row is unchanged. Rewriting 20,000 rows replaces every tuple
and changes the HNSW graph, so this is the same "never measure a freshly
mutated table" trap documented below — visible here in the benchmark's
own numbers rather than in someone else's.

**The headline is that pgvector's default is not good enough here.** At
20,000 rows it returns 80% of what an exact scan would — one relevant
memory in five missing, with no error and nothing in any output to
suggest anything went wrong. `-hnsw-ef-search 200` buys 94% and `400`
buys 98%, and the whole range stayed under 4ms p50, so the cost of
fixing it is roughly a millisecond and a half.

Two honest caveats on those timings. The p50 differences between
adjacent rows are near the noise floor for ten samples — the defensible
reading is "the entire range is a few milliseconds", not that 100 is
genuinely faster than 40. And latency here excludes embedding the query,
which is measured separately at ~33ms against local Ollama and dominates
every figure in this table.

`doctor` now reports this: a store past 10,000 embedded rows still on the
default prints the measured recall and the flag that fixes it. The
default itself is deliberately unchanged — see `efSearchRecallFloor` in
`cmd/claude-mem-go/doctor.go` for why one corpus on one embedding model
is not enough evidence to silently change search behaviour for every
existing store.

`SET LOCAL hnsw.ef_search` was confirmed to actually take effect on a
pooled connection (`SHOW` returns the requested value), since this
project has previously found Postgres accepting that GUC silently
without applying it.

At 20,000 rows the planner also chooses the HNSW index *on its own* —
verified with `EXPLAIN` — whereas at 3,000 rows it still prefers a
sequential scan. Forcing both plans is what makes the comparison valid at
either size; without that, a "natural" query below the crossover
silently compares exact against exact and reports a meaningless 100%.

## Scoped recall — the case the hooks actually use

`bench/recall/scoped` measures the project-filtered path, through the
real `postgres.Store.SemanticSearch` rather than hand-written SQL. It is
a separate question from the numbers above, not a variation on them: a
project filter is a POST-filter on the HNSW scan, which this project has
already caught returning **zero** rows for a project holding 60,000
observations, and `SemanticSearch` fixes that with
`hnsw.iterative_scan` — an entirely different traversal.

On 20,000 rows spread over 20 projects (1,000 each), at pgvector's
defaults:

| | |
|---|---|
| scoped recall@10 | **71.2%** |
| short results | 0/200 |
| empty results | 0/200 |
| p50 / p95 | 3.5ms / 8.5ms |

The post-filter bug is genuinely fixed — nothing came back empty or
short. But **scoped recall at the default is worse than unscoped**
(71.2% vs 80%), and this is the path every hook takes.

Raising `ef_search` fixes it, though not by the mechanism it appears to:

| `ef_search` | recall@10 | plan actually used |
|---|---|---|
| default (40) | 71.2% | HNSW (approximate) |
| 100 | 100.0% | exact (planner skipped HNSW) |
| 200 | 100.0% | exact (planner skipped HNSW) |
| 400 | 100.0% | exact (planner skipped HNSW) |

At 100 and above the planner stops using the HNSW index altogether and
takes a bitmap scan over `idx_observations_project`, reading all 1,000
rows of the project and sorting them exactly — pgvector's cost estimate
for an HNSW scan grows with `ef_search`, so past a point the exact path
simply costs less. **Recall is 100% because the query became exact, not
because the approximation improved.** That is good behaviour for a
moderate project size, but it is a different fact from what a recall
column alone appears to say, which is why the harness prints the plan
next to every figure.

### Two methodological traps this benchmark hit, both worth knowing

**Measuring on a freshly bulk-updated table.** The first scoped run
reported 100.0%; a rerun minutes later reported 71.2% with nothing
changed. The corpus had just had 20,000 rows `UPDATE`d to assign
projects, and autovacuum/autoanalyze ran *between* the two — the first
number was measured against 20,000 dead tuples and stale statistics. Any
measurement taken right after a bulk write is measuring the write.
`VACUUM ANALYZE` first, then confirm the figure reproduces.

**A corpus the same size as the cap.** `hnsw.max_scan_tuples` defaults to
exactly 20,000, so on a 20,000-row corpus `iterative_scan` can walk the
entire table and any perfect result is unfalsifiable. Rather than embed
200,000 rows to escape it, the sweep lowers the cap on a fixed corpus,
which is equivalent: recall held at 71.2% down to a cap of 1,000 (~a
400,000-row corpus), so the ceiling is `ef_search` interacting with the
filter, not the scan cap.

**What this does not measure**: behaviour past 20,000 real rows, and
projects large enough that the planner's exact fallback stops being
cheap — at 1,000 rows per project it is; at 100,000 it would not be.
