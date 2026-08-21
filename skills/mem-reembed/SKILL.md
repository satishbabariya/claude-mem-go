---
name: mem-reembed
description: Fix observations that have no embedding, or a stale one from a since-changed Ollama model, so they become findable via semantic_search_observations again. Use when mem-doctor reports embedding_dims_consistent=false, or "some observations need (re-)embedding," or the user asks to "fix semantic search" / "re-embed old memories."
---

# Fixing Stale or Missing Embeddings (reembed)

`claude-mem-go`'s `reembed` subcommand is the remediation half of what
`mem-doctor` can only detect: an observation with no embedding at all
(never got one — no model configured at capture time, or Ollama was down
that day), or one embedded with a dimension that no longer matches the
currently configured Ollama model (the model was swapped for a different
one at some point) — either way, `semantic_search_observations` will
never find it until it's re-embedded.

## When to use

- `mem-doctor`'s output shows `embedding_dims_consistent=false`, or "some
  observations need (re-)embedding with the current model"
- The user says semantic search feels incomplete or is missing something
  they know is in there
- The user explicitly asks to "fix semantic search" / "re-embed old
  memories" / "catch up embeddings"

Don't reach for this reflexively just because `mem-doctor` mentioned it —
confirm with the user first if it wasn't already the reason they asked
for a health check, since re-embedding costs one real Ollama API call
per affected row (free/local, but not instant for a large count).

## Dry-run first, same discipline as `mem-prune`

`reembed` defaults to a dry run — it reports how many observations WOULD
be re-embedded without touching anything. Always run that first and show
the count before adding `-yes`:

```
# Step 1 — always this first:
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" reembed

# Step 2 — only after confirming the count with the user:
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" reembed -yes
```

Not destructive the way `prune` is (nothing is ever deleted — this only
adds/replaces embeddings), but the real API cost is its own reason to
look before doing it, especially for a large count.

## Flags

- `-embed-model` — the model to re-embed with (default `nomic-embed-text`,
  should normally match whatever `mcp`/`worker`/`stop` are configured
  with — this IS the model observations will become searchable under).
  The server it talks to is `$CLAUDE_MEM_OLLAMA_BASE_URL` when set, else
  `http://localhost:11434`.
- `-project name` — scope to one project instead of every project in the
  store.
- `-db <path-or-DSN>` — the store to re-embed. Defaults to
  `$CLAUDE_MEM_DB` if that is set (the same store the hooks and MCP
  server use), else the local SQLite file.
- `-yes` — actually re-embed. Omit it for the dry run.

(Fall back to `claude-mem-go reembed ...` on `PATH` if
`$CLAUDE_PLUGIN_ROOT` isn't set — see `mem-doctor`'s skill for why.)

## A dry run reporting a surprising count is usually the wrong store

`reembed` and `mem-doctor` must be looking at the same database for their
numbers to mean anything together — doctor says "some observations need
(re-)embedding," reembed says how many. If reembed reports `0` right
after doctor flagged a problem (or a count far larger or smaller than the
user expects), suspect the store before suspecting the data: run
`doctor`, which names the store it used and says whether `$CLAUDE_MEM_DB`
chose it, and pass the same `-db` to both.

## If it stops early

A run that ends with `STOPPED after 5 consecutive failures` did not fail
on the data — it stopped because the embedding service looked down, and
deliberately did not work through the remaining rows. Report this to the
user as an infrastructure problem, not a memory problem:

- The rows already embedded are saved. The ones never attempted are
  unchanged and still need embedding.
- Re-running the exact same command after Ollama is healthy again
  continues from where it stopped — it does not redo the finished rows.
  (Verified: a run that stopped with 29 rows left re-embedded exactly
  those 29 on the next run.)
- The message names the server address it was using, which is the first
  thing to check — especially if `$CLAUDE_MEM_OLLAMA_BASE_URL` points
  somewhere unexpected.

A long run prints a progress line every few seconds (`… 400 re-embedded
— 12s elapsed, 33.1 rows/s`). Silence for more than a few seconds during
a large run is itself worth mentioning to the user.

## After running it

Suggest the user (or run yourself, if they ask) `mem-doctor` again to
confirm `embedding_dims_consistent=true` and the "needs (re-)embedding"
warning is gone.
