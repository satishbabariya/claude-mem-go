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
- the worker daemon reachable on its Unix socket
- the database reachable (SQLite file or Postgres DSN, whichever `-db`
  points at)
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
