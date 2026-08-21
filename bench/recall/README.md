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
go run ./bench/recall/seed    "postgres://…/throwaway?sslmode=disable" 3000
go run ./bench/recall/measure "postgres://…/throwaway?sslmode=disable"
```

Seeding is bounded by Ollama, measured here at ~28 embeddings/second, so
3,000 rows takes under two minutes.

## Measured

3,000 real `nomic-embed-text` embeddings (768 dimensions), pgvector
0.8.6, cosine distance:

| `hnsw.ef_search` | recall@10 |
|---|---|
| 20 | 95.0% |
| 40 (pgvector default) | **95.0%** |
| 100 | 95.0% |
| 200 | 95.0% |
| 400 | 98.0% |

**At this corpus size the knob barely matters.** Recall is flat across a
ten-fold range and only moves at 400. That flatness was checked rather
than assumed: `SET LOCAL hnsw.ef_search` was confirmed to actually take
effect on a pooled connection (`SHOW` returns the requested value), since
this project has previously found Postgres accepting that GUC silently
without applying it. The likeliest reading is that a 3,000-node graph is
shallow enough that even a narrow search explores most of it, and the
five misses are near-ties in distance that only a much wider search
separates.

**What this does not measure**: sensitivity at scale. `ef_search` is
expected to matter more as the graph deepens, and demonstrating that
needs a corpus large enough to be slow to embed. The honest summary is
that the default is fine here, and that anyone running materially more
data should re-run this rather than trust a 3,000-row result.
