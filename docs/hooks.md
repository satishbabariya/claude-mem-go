# Hooks and the worker daemon

How each Claude Code hook event is handled, why the hook binaries do no work
themselves and hand everything to a long-lived worker daemon, and the two
operator-facing controls that shape what gets captured: `<private>` tags and
excluded projects. The wiring itself is in `hooks/hooks.json`; installing it is
covered in [plugin-install.md](plugin-install.md).

## The subcommands behind each hook

- **worker** — a persistent daemon, meant to be started once (see `start`)
  and left running. Listens on a Unix socket, processes PostToolUse
  payloads through a bounded `pool` of observer sessions. Opens its
  `memory.Backend` exactly once for the daemon's whole lifetime (fixed from
  opening and closing a fresh one on every single event) — real connection
  churn otherwise, since a daemon meant to run for days would pay a fresh
  connection (a real TCP handshake against Postgres) per tool call
  indefinitely; verified end-to-end by restarting a live daemon and
  sending it multiple real events in sequence through the one handle.
  Combined with the Postgres backend's now-bounded connection pool
  (`SetMaxOpenConns`/`SetMaxIdleConns`, previously left at
  `database/sql`'s default of unlimited) — the two together are what keep
  a burst of concurrent tool calls from being the only thing standing
  between this daemon and a shared Postgres server's `max_connections`.
  `postgres.Open` also now retries its initial ping on a short backoff
  (~7.75s worst case across 6 attempts) instead of failing permanently on
  the very first attempt — every real caller (this daemon, every CLI hook,
  the MCP server) passes a context with no deadline of its own, so without
  an internal bound a startup race (this daemon, or a hook, starting a
  beat before Postgres's own container finishes its healthcheck) would
  otherwise hard-fail every single time it happened to lose that race.
  Verified against the real docker-compose container, not simulated: the
  container was stopped, `Open` was called, the container was restarted
  ~1.5s into the retry window, and `Open` returned successfully instead of
  failing on its first ping — confirmed by measuring that it actually took
  over a second, not that it merely didn't error.
- **hook** — the thin client Claude Code's `PostToolUse` hook actually
  invokes: forward stdin to the worker's socket, exit. Deliberately does
  *no* observation work itself — see "Why the worker/hook split" below.
- **start** — idempotent daemon launcher for `SessionStart`: spawns a
  detached worker if one isn't already running, using a `worker-spawn-gate.ts`-style
  lockfile so concurrent sessions starting at once don't spawn duplicates.
- **context** — the other `SessionStart` hook, and the piece that makes
  this project actually function as *memory* rather than an on-demand
  search tool: it looks up the current project's most recent observations
  and injects them as context Claude sees automatically, before anyone asks
  for anything. Verified against a real session, not just unit-tested: a
  distinctive marker was seeded directly into the database, and a real
  `claude -p` session — with no tools, asked only about its own injected
  context — correctly reported it back verbatim.
- **prompt-context** — the `UserPromptSubmit` hook: embeds the actual
  submitted prompt text and injects the semantically closest observations
  — sharper than `context`'s static recent-observations dump, since it
  responds to what's actually being asked rather than just "what happened
  lately." Real claude-mem's own gap of the identical shape (`session-init`
  does the same "embed the prompt, semantic-search, inject" against its
  own server-backed store). Skips prompts under 20 characters (too short
  to embed meaningfully) and disables entirely with `-embed-model ""`, the
  same convention `mcp`/`stop` use. See "`UserPromptSubmit`" below for how
  this was verified against a real Ollama call and a real `claude` session.
- **file-context** — the `PreToolUse` hook (matcher `Read`): real
  claude-mem's own per-file recall, distinct from `context`'s per-project
  recall. Looks up prior observations that mention the specific file about
  to be read (via `files_read`/`files_modified`) and injects them before
  the read happens. Verified against a real session the same way `context`
  was: a distinctive marker seeded directly into the database, and a real
  `claude` session — asked to read that exact file — correctly reported the
  injected context back.
- **stop** — the `Stop` hook: synthesizes everything recorded during one
  session into a single `type=summary` observation (real claude-mem's
  "summarize" step). Idempotent the same way ingestion is — the key is the
  session_id alone, so a session that ends more than once (or a Stop that
  fires twice) still gets exactly one summary, verified by running it twice
  against the same real session and confirming the second call recognized
  the duplicate and did nothing. Now embeds the summary too (a real,
  previously-undiscovered gap: this hook had no `-embed-model` flag at all
  and never called `SaveEmbedding`, so a session summary — arguably the
  single most information-dense observation this project ever
  produces — had been invisible to `semantic_search_observations` from the
  day this hook was written, findable only by keyword search or listing.
  Found by pattern-matching against the identical gap `add_observation`
  had). Verified against a real session: seeded observations, ran `stop`
  with the real embed model, and confirmed the resulting summary was
  found by `semantic-search` using a query with zero keyword overlap.

  A real, more consequential race found by an actual full-stack
  integration test (two genuine concurrent `claude` sessions, run
  through real hooks pointed at explicit isolated paths — not a
  synthetic unit test): `Stop` fires the instant a session ends, but
  `PostToolUse`'s own observation for the session's LAST tool call is a
  real, fire-and-forget async LLM call the worker daemon runs in the
  background, taking several real seconds. A single, immediate database
  read raced ahead of it — reproduced directly, one real session's `Stop`
  hook found *zero* observations 4 seconds before the worker finished
  persisting the one observation that session actually had, silently
  skipping the summary entirely. In a longer session, the identical race
  would instead silently produce a summary missing just its most recent,
  often most contextually important, action. Fixed by polling until the
  count stops growing across consecutive checks (up to ~12s, costing
  nothing perceptible since `Stop` already runs fire-and-forget) rather
  than trusting a single read — but the *first* version of that fix had
  its own real bug, caught live before it shipped: treating two
  consecutive zero-reads as "confirmed empty" is wrong, since a session
  with exactly one real tool call also reads zero on every check until
  the observer call actually finishes, which routinely took longer than
  one poll interval. Only a count that has gone *positive* and then
  stops growing is real evidence of stability; a genuinely tool-call-free
  session now correctly pays the full wait budget instead of exiting
  early on a false stabilization. Verified with dedicated tests proving
  each specific failure mode (false-positive stabilization at zero, a
  perpetually-growing count that must still terminate, a genuinely-empty
  session paying the full budget) and by re-running the original two
  real concurrent sessions twice more — the first re-run still found
  the flaw, the second, after fixing it, correctly summarized both.

  A *third* real bug, found the same way but against the Postgres
  backend's own full-stack test with a single session running TWO real
  tool calls back to back: even the corrected "two consecutive matching
  non-zero reads = stable" rule was still wrong, because observations
  for one session are processed *sequentially* — one worker mutex
  serializes turns per session (see `internal/worker/sessions.go`) — so the count
  can sit at 1 for several real seconds while the second tool call's
  observation is still mid-flight, and two quick matching reads during
  that plateau falsely "confirm" stability. Reproduced live down to the
  timestamp: the first tool call's observation landed, then the second
  started six seconds later on the same `session_id`, and `Stop` fired
  its summary in between, producing "from 1 observations" instead of 2.
  Fixed by replacing the 2-check rule with a streak counter requiring 10
  consecutive matching non-zero reads (~9s of confirmed no-growth,
  chosen well above a single observation's own observed ~5-8s latency)
  before trusting the count, with the overall wait budget raised from
  12 to 45 polls to accommodate. This narrows the race further but is
  still explicitly a heuristic, not a guarantee — there is no way for
  this hook to know for certain that *every* `PostToolUse` event for a
  session has finished, only to infer it from the count holding steady
  long enough; a fully robust fix would need the worker to expose real
  per-session "is a turn still in flight" state for this hook to query
  directly, which is a legitimate architectural follow-up, not something
  this fix attempts. Verified with a new dedicated test reproducing the
  exact plateau shape (a count that holds at a stale, lower value for
  several checks — fewer than the required streak — before the real
  next observation lands), confirmed as a genuine regression test by
  temporarily reverting the streak threshold back to 2 and watching it
  fail, then restoring it.

  That "legitimate architectural follow-up" is now built: the worker
  daemon exposes real per-session in-flight state over its own socket
  (a small plain-text `INFLIGHT <session_id>` query alongside the
  existing fire-and-forget hook-forwarding protocol, answered
  synchronously — see `hook.QueryInFlight`/`worker`'s `inflightTracker`),
  and `Stop` queries it directly instead of only ever inferring from a
  row count. This closes the gap two ways: it exits fast once the worker
  confirms nothing is left in flight (no more waiting out a long
  row-count streak once the real answer is already known), and — the
  half that actually matters for correctness — it *extends* the wait
  ceiling from ~45 seconds to five minutes once the worker confirms real
  activity, but only then, so a genuinely tool-call-free session still
  finishes in the original ~45s. That extension is not cosmetic: a live
  re-verification run of the previous fix hit a real single observation
  that took 104 seconds — longer than the old ceiling — which would have
  been cut off mid-flight, summarizing an incomplete session, without a
  live signal justifying the wait. A row-count streak reaching its own
  "stable" threshold no longer overrides the worker's own "still busy"
  answer, either — an override this fix specifically needed, verified by
  briefly disabling it and confirming a test failure it exists to catch
  (the old heuristic locking in early despite the worker reporting
  ongoing work). Verified live end to end with the actual compiled
  binary: a real worker daemon, a real two-tool-call `claude` session,
  and `Stop`'s own log correctly reporting "from 2 observations" via the
  new query path, not the row-count fallback.

## Hook coverage, audited against the shipped CLI

Recorded so it doesn't get re-derived. Claude Code 2.1.238 defines **31**
hook events (extracted from the shipped binary's own `hook_event_name`
literals, not from docs): `ConfigChange`, `CwdChanged`, `DirectoryAdded`,
`Elicitation`, `ElicitationResult`, `FileChanged`, `InstructionsLoaded`,
`MessageDisplay`, `Notification`, `PermissionDenied`, `PermissionRequest`,
`PostCompact`, `PostToolBatch`, `PostToolUse`, `PostToolUseFailure`,
`PreCompact`, `PreToolUse`, `SessionEnd`, `SessionStart`, `Setup`, `Stop`,
`StopFailure`, `SubagentStart`, `SubagentStop`, `TaskCompleted`,
`TaskCreated`, `TeammateIdle`, `UserPromptExpansion`, `UserPromptSubmit`,
`WorktreeCreate`, `WorktreeRemove`.

Real claude-mem's own `plugin/hooks/hooks.json` wires **six**:
`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`,
and `Setup`. This port wires the same five capture hooks. The one
difference, `Setup`, exists there to run `bun install` — the marketplace
extracts files without installing dependencies, so the worker crashed
with `Cannot find module 'zod/v3'` on the first hook invocation. A single
static Go binary has no runtime dependencies to materialize, so that
specific need does not transfer. (The separate, genuinely useful `Setup`
idea for this port — fetching a release asset when the binary is missing
— is described above and is still open.)

`PreCompact` looks like the natural hook for a memory system and is
deliberately not wired: `stop` builds its summary from observations
already persisted per tool call, not from the live context, so compaction
does not destroy anything it needs.

## The handler diff against real claude-mem is exhausted

Recorded so it isn't re-mined. Real claude-mem's hook logic lives in
seven files under `src/cli/handlers/` (1,061 lines). All seven have been
read line-by-line against this port's equivalents. What each produced:

| handler | maps to | outcome |
|---|---|---|
| `summarize.ts` | `stop` | **gap found**: `stop_hook_active` re-entry guard |
| `file-context.ts` | `file-context` | **two gaps**: file-mtime staleness gate; per-session dedup + specificity scoring |
| `context.ts` | `context` | **gap found**: git-root project resolution and worktree `allProjects` reads |
| `observation.ts` | `hook` | at parity — its three guards all present; its missing-`cwd` throw is a transcript-path fallback here, which is strictly better |
| `session-init.ts` | `prompt-context` | at parity or ahead — the `>= 20` prompt gate and internal-protocol skip were already present, plus duplicate-prompt dedup and privacy tag-stripping that real claude-mem has no equivalent of |
| `user-message.ts` | — | Cursor-only banner (Discord link, promo line, viewer URL). UI, and out of scope |
| `file-edit.ts` | — | Cursor's `afterFileEdit`. Claude Code has no analogue: `PostToolUse` with `matcher: "*"` already captures Edit/Write natively |

The one thing deliberately left unported is `file-context.ts`'s
`FILE_READ_GATE_MIN_BYTES` size gate — the constant's value is not
defined anywhere findable in that repo, and inventing a tuning threshold
would not be parity.

## Why the worker/hook split

An earlier version had the `PostToolUse` hook call the observer directly.
Testing that against a real Claude Code session showed it losing
observations: `"async": true` only means Claude Code doesn't wait for the
hook — it does **not** mean the hook's child process survives the
invoking `claude` process exiting. Both attempts died mid-observation. The
fix is what's here: a daemon started once and left running (detached via
`Setsid` so it survives its own launcher exiting too), with hooks doing
nothing but a fire-and-forget local socket write.

Both sides of that socket write are now bounded (`hook.MaxPayloadBytes`,
8MB — matching the MCP server's own JSON-RPC line cap): `hook.Forward`
rejects an oversized payload outright rather than sending it, and the
worker daemon's own socket read enforces the identical bound as
defense-in-depth. Without this, an abnormally large `tool_response` (a
`Bash` command that cats a multi-gigabyte file, a `Read` of a huge log)
had no upper bound at all — a real risk specifically because the worker
is one long-lived process every project on the machine shares, so a
single pathological tool call could balloon its memory for every other
session using it too, and would otherwise get stringified verbatim into
an observer prompt at a real per-token API cost. An oversized payload is
rejected whole, not truncated — a truncated JSON hook payload is corrupt,
not just short, so there's no safe partial-forward here. Verified against
a real running (isolated, throwaway) daemon: sent an 8MB+100-byte payload
over its actual socket, confirmed the daemon logged a rejection and
stayed alive, then sent a normal-sized payload through the same daemon
and confirmed it processed normally afterward.

Bounded in bytes, but not in wall-clock time until a later pass found the
gap: `handleConn`'s read had no deadline at all, only the byte cap above —
a client that dials the socket and never writes or closes (a stalled
process, or a bug in some future caller not going through `hook.Forward`)
leaked that goroutine and its underlying file descriptor for as long as
the daemon ran, which is meant to be days. More relevant once the
`INFLIGHT` query protocol (see the Stop-hook section above) started
sharing this same socket as a synchronous request/response exchange, not
just the original one-way hook forward — the client side already set its
own deadline, but the server side never did. Fixed with a
`handleConnReadTimeout` (30s, generous enough that neither a normal hook
forward nor a real `INFLIGHT` query — both near-instant on a local Unix
socket — ever come close to it). Verified with a real `net.Conn` (not
mocked): a test shrinks the timeout, connects a client that deliberately
never writes or closes, and confirms `handleConn` actually returns once
the deadline elapses rather than hanging — confirmed as a genuine
regression test by temporarily removing the deadline call and watching
the test time out before restoring it.

## `UserPromptSubmit` — semantic context injection on the actual prompt

`SessionStart`'s context injection (`context` subcommand) is a static
"most recent observations" dump, decided before Claude has any idea what
the user is about to ask. `prompt-context` closes the sharper gap real
claude-mem's own `session-init` handler covers: it fires on the actual
submitted prompt text, embeds it via the same local Ollama call
`semantic-search` uses, and injects the observations semantically closest
to *that specific question* — recall that responds to what's actually
being asked, not just "what happened lately."

Needed a new field on `claude-agent-sdk-go`'s `HookInput`
(`Prompt string`, confirmed against a real captured `UserPromptSubmit`
payload — same technique this package's other fields were verified with —
tagged `v0.1.1`) since nothing had ever needed the submitted prompt text
before. Guards mirror real claude-mem's own: prompts under 20 characters
are skipped rather than embedded (too short to be a meaningful semantic
anchor, and would waste an Ollama round-trip on every single message for
no benefit), and `-embed-model ""` disables the hook entirely — the same
on/off convention `mcp`/`stop` already use.

Verified against a real, isolated Ollama-backed setup, not a mock: two
topically distinct observations seeded and embedded (one about choosing
Postgres/pgvector for ANN search, one about log rotation), a real
`claude -p` session asked "what database technology did we choose for
scaling similarity search over embeddings?" — no keyword overlap with the
seeded title's exact wording — and Claude's answer came back correctly
identifying Postgres/pgvector/HNSW, explicitly attributing it to injected
memory rather than a codebase search. Deliberately run against a
throwaway `-db` file via `.claude/settings.json` rather than a full
plugin install, to avoid writing test rows into this machine's real,
shared production database (the same discipline applied throughout this
project's live-verification history).

### Persisting prompts (opt-in)

Real claude-mem stores every prompt the user submits in a
`user_prompts` table; this port, until now, kept only a SHA-256 of each
prompt in daemon memory for the 10-second dedupe window and discarded it
(issue #1). The table now exists on both backends — SQLite migration v8
(`user_prompts` plus a `user_prompts_fts` FTS5 index), Postgres migration
v6 (a generated `tsvector` under a GIN index) — with the same columns as
the reference (`session_id`, `project`, `prompt_text`, `prompt_number`,
`created_at`, `created_at_epoch`).

**Writing to it is off by default**, because this is the one kind of row
that stores the user's *verbatim words* rather than a model's summary of
them. Enable it with `-store-prompts` on the `prompt-context` hook, or
`CLAUDE_MEM_STORE_PROMPTS=1` in the environment the hook runs in. Nothing
else changes: the hook still prints `{}` and never fails on a storage
error (logged at `WARN`).

What is stored, and what never is: the write sits *after* every privacy
gate the hook already applies, in order — the excluded-projects list, the
internal-protocol-payload check (a `<task-notification>` is not user
text), `<private>…</private>` stripping, and the wholly-private check. A
prompt that is entirely private is not stored at all; a prompt with
private spans stores only the stripped text. Prompts shorter than
`-min-prompt-len` are stored even though they are not embedded — "yes" is
still part of what was said. The 10-second duplicate-firing guard applies,
so a doubled `UserPromptSubmit` does not store the prompt twice.

Reading it back: the `search_prompts` (keyword, or enumerate newest-first
with no query) and `session_prompts` (one session, in `prompt_number`
order) MCP tools, both scoped to the current project like their
observation counterparts. `stats` reports a `prompts` line once any
exist; `prune -older-than-days` deletes prompts past the cutoff in the
same scope as observations (its count stays observations-only); `export`
writes prompt rows after the observation rows tagged `"kind":"prompt"`,
and `import` dispatches on that tag — rows without one are observations,
so older export files still import unchanged.

## Excluding a project from automatic capture — a real feature gap this port had until now

Real claude-mem lets a user opt specific projects out of automatic
tracking entirely (`CLAUDE_MEM_EXCLUDED_PROJECTS`, a comma-separated
glob-pattern list checked at the top of every automatic hook handler —
`shouldTrackProject`/`isProjectExcluded` in
`src/shared/should-track-project.ts`/`src/utils/project-filter.ts`).
claude-mem-go had no equivalent anywhere: `worker.Daemon.process`
(automatic capture) and the `context`/`file-context`/`prompt-context`/
`stop` hooks (automatic recall/summary) all tracked unconditionally.
The only way to keep a sensitive, client-confidential, or scratch
project out of a shared memory database was to not install the plugin
at all — all-or-nothing, unlike real claude-mem's per-project opt-out.

Closed with a new `excludeproject` package and a `-excluded-projects`
flag on `worker` (forwarded from `start`, the same way `-model`/
`-embed-model`/`-max-concurrent` already are) and on each of `context`/
`file-context`/`prompt-context`/`stop` individually — matching real
claude-mem's own design of checking this at the top of every automatic
handler independently, not one central gate. A comma-separated list of
glob patterns (`*`, `**`, `?`, and a leading `~` for the home directory)
matched against both the full path and the directory's basename — a
project whose path or name matches is skipped entirely, logged as
`skip: project excluded (cwd=...)`, before any real work happens (for
`worker`, deliberately before ever spawning or reusing an observer
subprocess, so an excluded project never pays for an LLM call it's about
to throw away).

Deliberately ported to match real claude-mem's *exact* glob semantics
rather than inventing a new dialect — a user migrating an existing
`CLAUDE_MEM_EXCLUDED_PROJECTS` value should get identical matching
behavior here. Verified directly against the real TypeScript source, not
just re-derived from reading it: ran real claude-mem's own
`isProjectExcluded`/`globToRegex` functions through Node against the
exact same 19 test cases this port's own unit tests use, confirming
byte-for-byte identical results on every one — including the one
initially-surprising case (an empty path against a bare `*` pattern)
that turned out to be `shouldTrackProject`'s own separate `!cwd` guard
short-circuiting before `isProjectExcluded` is ever consulted, not a
divergence in the glob logic itself; this port's `IsExcluded` correctly
folds that same guard in, since it's the one function standing in for
both real claude-mem functions combined.

Verified live end to end, not just unit-tested: two throwaway projects
sharing one real running worker daemon, one with a directory name
matching an exclusion pattern and one without, each driven by a real
`claude` CLI session performing an identical `Read` tool call. The
matching project's `PostToolUse` event was skipped before any observer
call — confirmed via the worker's own log and, more directly, by
querying the real resulting database afterward: exactly one observation
existed, from the unmatched project, with the excluded project's
directory producing zero rows.

## Privacy: `<private>` tags

### No `<private>` tag redaction anywhere — this port's biggest privacy gap

Real claude-mem trains users on a specific convention, stated directly in
its own `UserPromptSubmit` banner: wrap anything in
`<private>...</private>` to keep it out of memory. `src/utils/
tag-stripping.ts`'s `stripMemoryTags` strips six tag names — `private`,
`claude-mem-context`, `system_instruction`, `system-instruction`,
`persisted-output`, `system-reminder` — and real claude-mem applies it at
both of its actual capture boundaries: the `PostToolUse` ingestion path
(`tool_input`/`tool_response`, stripped in `shared.ts` before an
observation is even queued) and prompt handling (`SessionRoutes.ts`
skips session-init/injection entirely — `reason: 'private'` — when a
prompt is wholly wrapped in a tag; `summarize.ts` strips the Stop hook's
summarized text the same way).

This port had none of it. `grep -rn "private" --include=*.go` (excluding
tests) across the whole repository returned zero matches — a
`<private>` block in a tool's output would be captured, sent to the
observer LLM prompt verbatim, summarized, embedded, and persisted
exactly like any other content, then surfaced through
`search_observations`/`semantic_search_observations` to any MCP caller.

Fixed with a new `privacy` package (`StripTags`/`StripMemoryTags`)
porting the exact tag set. It can't be tag-stripping.ts's single combined
regex with a `\1` backreference tying each open tag to its matching
close tag by name — Go's `regexp` package (RE2) has no backreference
support at all — so it's one compiled literal pattern per tag name
instead. Verified this produces identical results to the TS version for
same-tag nesting (the case tag-stripping.ts's own tests exercise:
`<private>a<private>b</private>c</private>` strips down to the same
dangling-`c</private>`-remains-as-literal-text result either way); the
two approaches can differ only in which tag's internal counter gets
credited when two *different* tag types are nested inside each other,
which this package doesn't expose a per-tag breakdown for, so it isn't
observable.

Wired in at the two places that matter for this port's architecture:
`worker.go`'s `PostToolUse` capture strips `tool_input`/`tool_response`
*before* `transcript.Truncate`, not after — a `<private>` block that
happened to straddle the 1500-byte truncation cutoff would otherwise be
left with a dangling, unclosed tag this package's regex could never
match, defeating the whole point — and `prompt-context`'s
`UserPromptSubmit` handling skips the embedding call entirely for a
wholly-private prompt, matching real claude-mem's `reason: 'private'`
skip. `stop.go` needed no separate change: its session summary is built
purely from already-persisted (by then already-redacted) observations,
never from re-reading the raw transcript.

Every unit test case was ported directly from real claude-mem's own
`tests/utils/tag-stripping.test.ts` — read and ported from the actual
test file, not re-derived from prose — covering basic single/multiple/
interleaved tag removal, multiline tag content, ReDoS-volume timing (150
tags and a 10,000-character single tag, both under a 1-second budget),
tags embedded inside JSON strings, both `system_instruction` spellings,
and `system-reminder` including the realistic case real claude-mem's own
test exists for: a tool result carrying an injected CLAUDE.md dump that
itself contains a nested `claude-mem-context` block.

Verified live past the unit tests too, on both call sites, each confirmed
as a genuine fix (not a tautological check) by temporarily removing the
strip call, rebuilding, and rerunning the identical scenario before
restoring it: a real `worker.process()` call with a `<private>` block in
`tool_response` confirmed the transcript.ToolCall actually handed to the
observer contains neither the tag markup nor the secret text inside it
(the disabled version leaked both straight through); and the real,
compiled `prompt-context` binary against a real Ollama model confirmed a
wholly-private prompt is skipped before any embedding call is ever made,
while a partially-private prompt still gets its non-private remainder
embedded and searched normally (the disabled version sent the literal
`<private>...</private>` markup and the secret inside it straight to a
real embedding call).

### A `<private>` prompt only redacted the prompt text — its tool calls and Stop summary weren't suppressed

The tag-stripping fix above only ever gated the prompt's own semantic
embedding. Real claude-mem's privacy guarantee goes further:
`PrivacyCheckValidator.checkUserPromptPrivacy` is consulted at both its
actual capture boundaries — before queueing a `PostToolUse` observation
(`shared.ts`) and before generating a Stop-time summary
(`SessionRoutes.ts`) — and suppresses the *entire turn*, not just the
prompt text, once that turn's persisted prompt stripped to nothing. This
port had no equivalent: a user who wrapped a prompt in
`<private>...</private>` still had every tool call from that turn fully
observed by the LLM, persisted, and included in the Stop summary — the
privacy promise silently stopped at the prompt itself.

Fixed by extending the worker daemon's existing plain-text socket
protocol — already used for the `INFLIGHT <session_id>` query — with two
more message kinds: a fire-and-forget `PRIVATE <session_id> <0|1>`
marker and a request/response `ISPRIVATE <session_id>` query
(`hook.SetSessionPrivate`/`hook.QueryPrivate`, parsed by
`hook.ParsePrivacyMarker`/`hook.ParsePrivacyQuery`). `prompt-context`
sends the marker on **every** `UserPromptSubmit`, private or not — not
only when private. That matters: real claude-mem's own
`PrivacyCheckValidator` doc comment documents a real bug it exists to
avoid (issues #2794/#2795) — a session whose `user_prompts` row is
absent (session-init hadn't run yet) must never be treated as private,
or every observation for that session would be silently frozen forever.
A sticky "mark private and never clear it" flag would reproduce exactly
that failure mode the moment a later, non-private prompt superseded an
earlier private one, so the flag has to be actively re-asserted (to
`false`) on every non-private prompt too, not just set once and left.

The worker's `process()` (`PostToolUse`) and `stop` (`Stop`) both check
the flag — `sessionCache.isPrivate`, defaulting to `false` for a session
with no flag ever recorded, matching real claude-mem's own
absent-signal-defaults-to-allow behavior — right alongside the existing
project-exclusion check, before any real work (spawning an observer
subprocess, opening the store) happens. `stop` runs as its own separate
process with no direct access to the worker's in-memory state, so it
queries `ISPRIVATE` over the socket the same way it already queries
`INFLIGHT`; a query failure (daemon unreachable) deliberately falls
through to summarizing normally rather than skipping, since an unknown
signal must never be treated as "private" either.

One structural difference worth calling out: real claude-mem's
`UserPromptSubmit`-equivalent and its later observation-ingestion calls
run on separate async paths that can genuinely race (hence #2794/#2795
in the first place). This port's `UserPromptSubmit` hook
(`prompt-context`) is **not** registered `"async": true` in
`hooks.json` — it blocks Claude Code's own turn until it returns — so
the privacy marker is guaranteed to reach the worker before Claude Code
even begins the tool-calling turn that could fire a `PostToolUse` event
in response to that same prompt. This port's version is race-free for a
reason real claude-mem's own can't be: the hook chain itself enforces
the ordering, not a lookup against a persisted row.

Verified with new unit tests in the `worker` package — wire-format round
trips for both new message kinds, the full setter/query exchange driven
through the real client functions against a real worker over a real Unix
socket (confirming a later `false` marker actually supersedes an earlier
`true` one, and that a session with no marker ever sent reads back as
not private), `process()` skipping before it ever touches the
nil-backed session cache that would otherwise panic (mirroring the
project-exclusion regression test's own technique), and a stale-entry
eviction test for the map bounding this state's lifetime. Verified live
past the unit tests too: a real worker daemon, a `<private>` prompt
followed by real `PostToolUse` and `Stop` payloads for one session
(confirmed zero rows in the real database across both), against a
control session with a normal prompt (a real observation AND a real
session summary, both persisted) — confirmed as a genuine fix by
temporarily removing the `process()` gate and watching the regression
test fail with a real nil-pointer panic before restoring it.

## Running the worker as a supervised service (optional)

By default, the worker daemon only ever starts lazily: `start` (invoked
from `SessionStart`) spawns it if it isn't already running, guarded by a
spawn lock so concurrent sessions starting at once don't race. That's
fine for normal use, but it means a crashed daemon, or a machine reboot
with no Claude Code session active to trigger the next `SessionStart`,
leaves memory capture silently dead until something starts a session
again.

`deploy/systemd/claude-mem-go-worker.service` (Linux, user-level
systemd) and `deploy/launchd/com.claude-mem-go.worker.plist` (macOS
launchd user agent) are optional templates for supervising it
independently — install either one and the daemon restarts itself on
crash and comes back after a reboot, with no conflict with `start`'s own
lazy-launch logic (`start` checks whether a worker is already reachable
before spawning one, so having a supervisor keep it running just means
that check always finds one already there). See the comments at the top
of each file for install steps.

Verified for real, not just written and assumed: the systemd unit was
checked with `systemd-analyze verify` in a real systemd container (Docker
— its `%h` specifier resolves correctly, and the file parses clean); the
launchd plist was actually loaded on a real macOS machine, confirmed the
supervised process came up and answered `doctor` on its own socket, then
killed the process directly and confirmed `KeepAlive` relaunched it
within seconds — genuine crash recovery, not assumed from the plist's own
claimed behavior.
