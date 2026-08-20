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
- `-project name` — scope to one project instead of every project in the
  store.
- `-yes` — actually re-embed. Omit it for the dry run.

(Fall back to `claude-mem-go reembed ...` on `PATH` if
`$CLAUDE_PLUGIN_ROOT` isn't set — see `mem-doctor`'s skill for why.)

## After running it

Suggest the user (or run yourself, if they ask) `mem-doctor` again to
confirm `embedding_dims_consistent=true` and the "needs (re-)embedding"
warning is gone.
