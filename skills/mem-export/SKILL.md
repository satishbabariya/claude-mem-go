---
name: mem-export
description: Back up claude-mem-go's memory database to a file, or restore/migrate one from a file. Use when the user asks to "back up my memories", "export the memory database", "move my memories to Postgres", or "restore from a backup".
---

# Memory Backup & Migration (export / import)

claude-mem-go's `export`/`import` subcommands are its only backup and
cross-backend migration story — there's no other way to get data out of
the store, or move it between the SQLite and Postgres backends.

## When to use

- "Back up my memories" / "export the database" → `export`
- "Restore from a backup" / "import that file" → `import`
- "Move my memories to Postgres" (or from Postgres to SQLite) → `export`
from the source backend, `import` into the destination — the same file
works for both, since both backends implement the same interface.

## export

Writes every observation as JSON Lines (one per line) to a file.

```
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" export -out backup.jsonl
```

Add `-db <postgres DSN>` to export from the Postgres backend instead of
the default SQLite file. No `-project` filter exists on export — it's
always everything, since a partial backup is a worse default than a
complete one.

## import

Reads a file `export` wrote and re-inserts every row.

```
"$CLAUDE_PLUGIN_ROOT/claude-mem-go" import -in backup.jsonl
```

Add `-db <postgres DSN>` to import into the Postgres backend (the
migration path). This is safe to run more than once, or against a
database that already has some of the data: each row keeps its original
content-hash, so an already-present row is skipped, not duplicated —
tell the user this if they're worried about re-running it.

(Fall back to `claude-mem-go export`/`import` on `PATH` if
`$CLAUDE_PLUGIN_ROOT` isn't set — see `mem-doctor`'s skill for why.)

## Not destructive, but be clear about direction

Neither command deletes anything from the source — `export` only reads,
and `import` only adds. If the user's actual goal involves also deleting
old data (freeing up space, not just backing it up), that's a separate,
genuinely destructive step — see the `mem-prune` skill, and don't combine
the two without the user explicitly asking for both.
