# Installing as a Claude Code plugin

The full install story: the plugin manifest and what was verified through it,
how the binary gets built when a git-installed plugin ships none
(`scripts/ensure-binary.sh` and `scripts/run-hook.sh`), and the skills the
plugin bundles. The release pipeline that produces downloadable binaries is
described in [development.md](development.md#releases).

`.claude-plugin/plugin.json` + `.claude-plugin/marketplace.json` +
`hooks/hooks.json` + `.mcp.json` make this a real, installable Claude Code
plugin — not just something wired by hand-editing `.claude/settings.json`.
Validated with the real CLI, and installed/exercised end to end at
**project scope** (never user/machine-wide — that would affect every other
Claude Code session on the box, not just a test):

```sh
go build -o claude-mem-go ./cmd/claude-mem-go
claude plugin validate .                                          # manifest sanity check

# From inside a project you want claude-mem-go active in:
claude plugin marketplace add /path/to/claude-mem-go --scope project
claude plugin install claude-mem-go@claude-mem-go-local --scope project
```

`SessionStart`, `PostToolUse`, and the MCP server were confirmed live
through this exact mechanism (not `--mcp-config`/manual settings):
`SessionStart` starts the worker and injects context, `PostToolUse` reaches
the worker and persists a real observation, and `search_observations`
returns real rows through the plugin-bundled `.mcp.json` — all in one
project-scoped install/uninstall cycle, cleaned up afterward. `Stop` (session
summarization) is wired into the same `hooks/hooks.json` and was verified
directly against real, already-persisted session observations rather than
re-run through a full plugin install cycle.

## The self-healing install path

The **install path is now self-healing and self-announcing**, which it
was not. The binary is gitignored — a build artifact, not source — so a
plugin installed from a git source ships the complete Go source and no
binary. Every hook then failed with `No such file or directory`, Claude
Code swallowed that, and memory was silently dead forever. The trap
compounds: `doctor` is a subcommand of the very binary that is missing,
so the tool an operator would reach for could not run either. Reproduced
by unpacking `git archive HEAD` and running a real session against it.

Two layers, because one was demonstrably not enough:

- **`Setup` hook** (`scripts/ensure-binary.sh`) builds the binary when it
  is missing or unrunnable — matching real claude-mem's own `Setup` hook,
  which exists to `bun install` its runtime dependencies for the same
  class of reason. It runs off the hot path with a 300s timeout, because
  a genuinely cold build measures ~33s here while SessionStart's hooks
  allow 10–15s. A working binary costs one `version` call and exits. It
  tests runnability rather than existence, since a wrong-architecture
  binary stats fine and only fails when executed.
- **Hook wrapper** (`scripts/run-hook.sh`) that every hook goes through,
  because `Setup` alone could not be relied on: it was observed **not**
  firing for a genuinely installed plugin under `claude -p`, nor during
  `claude plugin install`. On the healthy path it `exec`s the binary. On
  a missing one, SessionStart's `context` hook — the only place a hook
  can put text in front of the user — returns a real `additionalContext`
  payload, and the remaining hooks log to
  `~/.claude-mem-go/missing-binary.log`. It deliberately does not build:
  10–15s against a ~33s cold build would only time out.

Verified end to end against a real session with no binary present: the
session itself told the user *"To fix claude-mem-go, run this and restart
Claude Code: cd … && go build -o claude-mem-go ./cmd/claude-mem-go"*.
After healing, capture works through the wrapper — confirmed by a real
observation landing in the store.

Fetching a matching release asset instead of building remains unwired,
and deliberately so: there are no published releases or tags yet, and the
repository is private, so asset download would need auth. Building from
the source the plugin already ships is the fix that works for the
distribution actually in use.

## Bundled skills

- **skills/mem-search** — a real Claude Code skill (`/mem-search`) teaching
  Claude when to reach for `search_observations` vs.
  `semantic_search_observations`. Validated with `claude plugin validate
  --strict`, and verified live: installed the plugin, ran `/mem-search
  claude-mem installation` in a real session, and confirmed via the MCP
  server's own log that a genuine `tools/call` fired — not a hallucinated
  answer.
  Went stale as the MCP surface grew past when this skill was first
  written — found by hand rather than assumed current: it still said
  "eight tools" and never mentioned `observation_context`,
  `important_workflow`, or `search_observations`'s `offset` argument, all
  added later the same session. The skill whose entire job is teaching
  which tool to reach for is the one place staleness here actually
  matters. Updated to document all ten, verified live the same way as the
  original: `--plugin-dir` loading this plugin into a real `claude -p`
  session and asking it to list every MCP tool name the skill mentions —
  all ten came back, confirming the updated file is what a real session
  actually reads, not just that the markdown parses.
- **skills/mem-doctor** — a second skill (`/mem-doctor`) surfacing the
  `doctor` health check *inside* a Claude Code session instead of only from
  a raw terminal — "is memory actually working" shouldn't require dropping
  out of the conversation to find out. Verified the same way as
  `mem-search`: installed the plugin at project scope in a throwaway
  directory, ran `/mem-doctor` in a real session, and got the real health
  check's own output back (worker/database/Ollama status), confirming
  `$CLAUDE_PLUGIN_ROOT` resolves correctly for a skill-invoked command, not
  just for hooks and the MCP server.
  Went stale the same way `mem-search` did, caught in the same pass:
  never mentioned `doctor`'s `-hnsw-ef-search` flag or the
  `hnsw_ef_search` health field it reports, both added earlier the same
  session as this skill's own last edit. Updated, and verified the same
  live way: `--plugin-dir` loading this plugin into a real `claude -p`
  session and asking it to list every distinct thing the skill says
  `doctor` checks — `hnsw_ef_search` came back among them.
- **skills/mem-timeline** — the port's answer to real claude-mem's
  `timeline-report` and `weekly-digests` skills: a narrative report of a
  project's history, either as one timeline or week-by-week. Only became
  possible once `search_observations` learned to enumerate with no query
  (see the Testing notes and CHANGELOG) — before that, a project's full
  history could not be read through the MCP surface at all. Validated
  with `claude plugin validate --strict` and verified live the same way
  `mem-search` was: a real session loaded the skill and made ten genuine
  `tools/call` invocations, confirmed in the MCP server's own log rather
  than inferred from the reply. That live run also caught a real bug in
  the skill's own instructions — it enumerated another project with
  `all_projects` but omitted it from the follow-up `get_observations`,
  which is project-scoped and rejected the ids, so the report was built
  from titles alone. Fixed by saying so explicitly.
- **skills/mem-prune, skills/mem-export, and skills/mem-reembed** —
  surface `prune`, `export`/`import`, and `reembed` as
  `/mem-prune`/`/mem-export`/`/mem-reembed` the same way `mem-doctor`
  surfaces `doctor`. `mem-prune`'s instructions are written to treat this
  as the one genuinely destructive operation in the CLI: always run the
  dry run first, show the count, and get explicit confirmation before
  ever adding `-yes` — verified live, not just written and hoped for:
  asked a real session to "clean up memories older than 1 day" and
  confirmed it ran the dry run, reported the count, and asked whether to
  actually delete rather than doing so on its own. `mem-reembed`'s
  instructions apply the identical dry-run-first discipline for a
  different reason: not destructive, but a real Ollama API cost per row.
