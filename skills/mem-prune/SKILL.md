---
name: mem-prune
description: Delete old observations from claude-mem-go's memory database to control its size. Use when the user asks to "clean up old memories", "delete observations older than X", or "reduce the size of the memory database" — never on your own initiative.
---

# Memory Retention (prune)

claude-mem-go's `prune` subcommand deletes observations older than a
cutoff. This is the ONLY destructive operation this project's CLI
exposes — treat it with real caution, not like `mem-search`/`mem-doctor`.

## When to use

Only when the user explicitly asks to delete or clean up old memory —
never proactively, and never as a side effect of some other request. "The
database seems large" is not, on its own, a request to delete anything;
ask first.

## The safety rule: dry-run first, always

`prune` defaults to a dry run — without `-yes`, it only reports how many
observations WOULD be deleted, and deletes nothing. **Always run the dry
run first, show the user the count, and get their explicit confirmation
before ever adding `-yes`.** Do not skip straight to `-yes` even if the
user's request sounds specific ("delete everything older than 90 days") —
they may not have known how many observations that actually covers.

```
# Step 1 — always this first, and show the result to the user:
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" prune -older-than-days 90

# Step 2 — only after the user confirms the count is what they want:
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" prune -older-than-days 90 -yes
```

(Fall back to `claude-mem-go prune ...` on `PATH` if `$CLAUDE_PLUGIN_ROOT`
isn't set — see `mem-doctor`'s skill for why.)

## Flags

- `-older-than-days N` — required; there is no default (an unset cutoff
  refuses to run rather than guessing).
- `-project name` — scope to one project instead of every project in the
  store. Ask the user which they mean if it's ambiguous ("my memories" vs
  "all projects").
- `-yes` — actually delete. Omit it for the dry run.

## Before suggesting this at all

If the user's real concern is disk usage or a very large database,
consider whether `export` (see the `mem-export` skill) — backing up to a
file — is what they actually want instead of, or before, deletion.
