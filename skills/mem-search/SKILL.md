---
name: mem-search
description: Search claude-mem-go's persistent cross-session memory database. Use when the user asks "did we already solve this?", "how did we do X last time?", or needs work from a previous session that SessionStart's automatic context injection didn't already surface.
---

# Memory Search

claude-mem-go already injects memory automatically twice — a project's
most recent observations at `SessionStart`, and (separately) whatever's
semantically closest to the actual prompt just submitted, on every
`UserPromptSubmit` — so this skill is for everything neither automatic
path covers: older sessions, a different project, or a specific question
worth deliberately searching for rather than whatever showed up
unprompted.

## When to use

Use when the user asks about PAST sessions, not the current conversation:

- "Did we already fix this?"
- "How did we solve X last time?"
- "What did we find out about Y?"

## Thirteen tools, seven kinds of job

claude-mem-go exposes thirteen MCP tools — independently callable, not a
mandatory staged pipeline the way real claude-mem's own
search→timeline→get_observations sequence is (that staging exists to
manage token cost across separate raw-vs-compressed representations this
project's schema doesn't have; `important_workflow`, below, documents the
same pattern here as a recommendation, not an enforced one). Three are
search (below); four are direct lookups when you already know what you
want and don't need to search for it; `get_observations` fetches full
detail for IDs any of the others already gave you; `timeline` gets
chronological context AROUND one result rather than the result in
isolation; one — `add_observation` — is the only *write* tool among them
(see below); one — `important_workflow` — is pure guidance, not a lookup
at all; two — `search_prompts` and `session_prompts` — read the user's
own stored prompts rather than observations (opt-in, see below). This
skill is mainly about finding what's already remembered,
but recognizing when a request actually needs `add_observation` instead
of a search matters too:

- `recent_observations(limit?, project?)` — the current project's most
  recent observations, newest first, as an abbreviated `[id] title
  (project, tool)` list. The same underlying read `SessionStart`'s
  automatic context injection already uses, reachable on demand (e.g. for
  a project other than the current one, via `project`).
- `session_start_context(limit?, project?)` — the SAME underlying read as
  `recent_observations`, but formatted as the exact prose block
  `SessionStart` actually injects (no ids, no project/tool annotation) —
  use this instead of `recent_observations` when you specifically want to
  see or reproduce what a session actually saw at startup, not a list to
  work from.
- `session_observations(session_id, limit?)` — every observation from one
  Claude Code session, oldest first. Use when the user asks "what did we
  do in that session" and you have (or can find) the session_id.
- `file_observations(file_path, limit?, project?)` — prior observations
  that mention a specific file being read or modified. The same lookup the
  `PreToolUse` hook already runs automatically right before a `Read`, on
  demand for any file, not just the one about to be read.

Reach for these FOUR first when the question doesn't need a query at
all — "what's recent," "what happened last session," "what do we know
about this file" don't benefit from full-text or semantic matching, they
just need the right rows.

### `get_observations` — full detail for IDs you already have

Every tool above returns an abbreviated `[id] title (project, tool)` line
— deliberately, to keep list output short. `get_observations(ids=[...])`
fetches the full narrative/facts/concepts/files for one or more of those
IDs, once a specific one looks worth reading in full:

```
get_observations(ids=[42])
```

Unknown IDs are silently omitted rather than erroring, and it's scoped to
the current project the same way `search_observations` is — pass
`all_projects: true` if the ID genuinely came from a different project's
search. Capped at 100 IDs per call (the same "max 100" convention every
other tool's `limit` uses) — this is a detail lookup for results a search
already returned, not a bulk export; call it again in batches if there
are genuinely more than 100 IDs worth reading.

### `timeline` — context AROUND a result, not the result alone

A search result in isolation doesn't say what led up to it or what
happened right after. `timeline(anchor?, query?, depth_before?, depth_after?)`
returns the anchor observation plus the observations immediately before
and after it, in chronological order — give it an `anchor` (an
observation ID, e.g. from a search result) directly, or a `query` to find
one automatically (the single best keyword match becomes the anchor):

```
timeline(anchor=42, depth_before=3, depth_after=3)
timeline(query="added rate limiting")   -- finds the anchor for you
```

Depths default to 10 each side, capped at 100. Use this when the user asks
"what led up to X" or "what did we do right after Y," not when they just
want to know what X or Y was — `get_observations` answers that.

### `search_observations` — exact terms

Full-text (keyword) search. Use when the user's question contains specific
words likely to appear verbatim in a title or narrative — error messages,
function names, exact phrases.

```
search_observations(query="authentication token expired", limit=10)
```

Add `type` to narrow to one kind of observation — `discovery` (found
something), `change` (did something), `decision` (chose something),
`summary` (a session's end-of-session recap), or `manual` (explicitly
added via `add_observation`):

```
search_observations(query="rate limiting", type="decision")
```

Add `offset` to page past a prior call's `limit` (default 0) — useful
when the first page didn't have what you needed but the match count
suggested there was more:

```
search_observations(query="rate limiting", limit=10, offset=10)  -- results 11-20
```

### Omit `query` to enumerate instead of search

`search_observations` with no `query` returns **everything matching the
other filters**, newest first (or oldest first with
`orderBy: "date_asc"`). This is the only way to walk a project's whole
history: `recent_observations` has no `offset` and stops at the 100 most
recent, so on a project with more than that, older observations are
unreachable through it.

```
search_observations(dateStart="2026-08-10", dateEnd="2026-08-16",
                    orderBy="date_asc", limit=100)          -- one week
search_observations(orderBy="date_asc", limit=100, offset=100)  -- next page
```

Reach for this when the user asks for something spanning a period rather
than matching a term — "what did we do last week", "walk me through this
project's history", a timeline or digest. Page with `offset` until a call
returns fewer than `limit` rows.

Combine it with `type` to enumerate one kind of thing — `type="decision"`
over a date range answers "what did we decide this month" without needing
to guess the words any of those decisions were recorded in, which is
exactly what keyword search cannot do.

### `semantic_search_observations` — meaning, not exact words

Embedding-based search. Use when the user's question is phrased
differently than however it was originally recorded — "how did we handle
login failures" should still find an observation titled "JWT token
validation added," even with no shared keywords.

```
semantic_search_observations(query="how did we handle login failures", limit=10)
```

Requires the worker/MCP server to have an embedding model configured
(`-embed-model`, default `nomic-embed-text` via Ollama) — if semantic
search returns nothing when keyword search finds results, the query terms
probably just don't overlap; try rephrasing, or fall back to keyword search.

### `observation_context` — the same search, pre-formatted to drop into a reply

Same embedding pipeline as `semantic_search_observations` (and the same
`-embed-model` requirement), but the output shape is different on
purpose: instead of a list of `[id] title` lines for you to read and
decide what to do with, it returns the exact ready-to-inject text block
`UserPromptSubmit`'s automatic recall already produces — a "Memory
relevant to what you just asked" block meant to be read or quoted
directly, not parsed. Reach for this specifically when the user is
explicitly asking "what does memory know about X" mid-conversation and
you want to surface that context verbatim, rather than when you're
deciding for yourself what to do with search results — `semantic_search_observations`
is still the right call for that:

```
observation_context(query="why did we move off of sqlite")
```

### `search_prompts` / `session_prompts` — the user's own words (opt-in)

Every tool above returns *observations* — a model's summary of what was
done. These two return what the user actually **asked**, verbatim, which
answers a different question: "what did I ask about X last week?" rather
than "what did we find out about X?". They only have data when the
`prompt-context` hook ran with `-store-prompts` (or
`CLAUDE_MEM_STORE_PROMPTS=1`); it is off by default because it stores the
user's exact words, and `<private>…</private>` spans are never stored.
An empty result is therefore usually "not enabled", not "never asked".

```
search_prompts(query="rate limiting", limit=10)      -- keyword search
search_prompts(limit=20, offset=20)                  -- no query: enumerate newest first
session_prompts(session_id="<the session_id>")       -- one session, in order
```

Each line is `[#<prompt_number> <session> <date>] <text>`; pass
`all_projects: true` to `search_prompts` to reach beyond the current
project. Reach for these when the user refers to something they *said*
or *asked for* ("I told you to skip the tests last time", "what was I
working on when I asked about the cache?") — an observation may not
record the request itself, only the work that followed.

### `add_observation` — the one write tool

Everything above only ever surfaces what `PostToolUse` already captured
automatically from a tool call. Use `add_observation` when there's
something worth remembering that ISN'T the direct result of one — a
decision the user just made, a stated preference, a piece of context that
should be recalled in a future session on its own:

```
add_observation(title="prefers tabs over spaces in Go code", narrative="stated explicitly, applies project-wide")
```

`title` is required; `subtitle`/`narrative`/`facts`/`concepts` are
optional. Calling it twice with the same `title`/`narrative` in the same
session is a no-op, not a duplicate — safe to call again if unsure
whether it already ran.

## Which one first?

Try `search_observations` first if the question names something specific
enough to likely appear verbatim (an error string, a package name, a
literal phrase someone would have typed). Reach for
`semantic_search_observations` when the question is about a *concept* or
*outcome* rather than exact wording, or when keyword search comes back
empty and the thing being asked about plausibly happened under different
words.

`important_workflow` (no arguments) returns a short reminder of the
`search_observations` → `timeline` → `get_observations` staging this
skill already describes above — narrow to a few relevant IDs with a
cheap search before paying the token cost of `get_observations`' full
detail, rather than fetching everything up front. Nothing this skill
tells you contradicts it; call it directly only if you want the reminder
in the tool's own words rather than this skill's.

## Examples

**Specific error message:**
```
search_observations(query="connection refused", limit=10)
```

**Conceptual question, wording unknown in advance:**
```
semantic_search_observations(query="why did we choose postgres over sqlite", limit=10)
```

**Keyword search came back empty, try semantic:**
```
search_observations(query="rate limiting")      -> no results
semantic_search_observations(query="rate limiting")  -> may still find
  an observation titled "Added request throttling" even with zero
  shared keywords
```

**"What have we been doing lately" — no query needed:**
```
recent_observations(limit=10)
```

**"What did we do in that other session" — a direct lookup, not a search:**
```
session_observations(session_id="<the session_id>")
```

**"What do we already know about this file" — before or instead of reading it:**
```
file_observations(file_path="src/auth/middleware.go")
```

**A search result's abbreviated line isn't enough detail — read the full observation:**
```
search_observations(query="rate limiting")   -> "[42] Added request throttling (proj, Bash)"
get_observations(ids=[42])                   -> full narrative/facts/concepts/files for id 42
```

**"What led up to that" / "what happened right after" — context around a result, not the result alone:**
```
timeline(query="added rate limiting", depth_before=3, depth_after=3)
```
