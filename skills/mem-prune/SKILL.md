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

# report, then strip, relative file paths file-context can never match
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" prune -relative-paths
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" prune -relative-paths -yes
```

(Fall back to `claude-mem-go prune ...` on `PATH` if `$CLAUDE_PLUGIN_ROOT`
isn't set — see `mem-doctor`'s skill for why.)

## Confirm WHICH store you're about to delete from

`prune` acts on one store, and which one is not obvious. With no `-db`
flag it uses `$CLAUDE_MEM_DB` when that is set (the same store the hooks
and MCP server write to), and the local SQLite file otherwise.

That matters more here than anywhere else, in both directions. A dry run
against the wrong store reports a count that has nothing to do with the
data the user is thinking of — usually `0`, which reads as reassuring
right up until `-yes` deletes from somewhere else entirely. Run
`mem-doctor` first and show the user the store it names, so the count
they approve is a count from the database they mean:

```
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" doctor   # names the store, and says
                                             # whether $CLAUDE_MEM_DB chose it
```

Pass `-db <postgres DSN>` explicitly if it needs to be a different store
than the one doctor reports — and if you do, say so to the user, because
it is then not the store their hooks are writing to.

## Flags

- `-older-than-days N` — required; there is no default (an unset cutoff
  refuses to run rather than guessing).
- `-relative-paths` — a different mode, mutually exclusive with
  `-older-than-days`: strips RELATIVE entries from `files_read`/
  `files_modified` on observations written before file paths were
  canonicalized. The `PreToolUse` file-context lookup can never match
  those (Claude Code sends absolute paths), and they cannot be made
  absolute — the cwd they were relative to was never stored, and a guess
  would make the row match the wrong file. The observations are kept;
  only the unmatchable path entries go. Dry-run without `-yes`.
- `-db <path-or-DSN>` — the store to prune. Defaults to `$CLAUDE_MEM_DB`
  if set, else the local SQLite file. See the section above.
- `-project name` — scope to one project instead of every project in the
  store. Ask the user which they mean if it's ambiguous ("my memories" vs
  "all projects").
- `-yes` — actually delete. Omit it for the dry run.

## Before suggesting this at all

If the user's real concern is disk usage or a very large database,
consider whether `export` (see the `mem-export` skill) — backing up to a
file — is what they actually want instead of, or before, deletion.
