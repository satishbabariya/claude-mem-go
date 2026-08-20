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

## Eight tools, five kinds of job

claude-mem-go exposes eight MCP tools — independently callable, not a
mandatory staged pipeline the way real claude-mem's own
search→timeline→get_observations sequence is (that staging exists to
manage token cost across separate raw-vs-compressed representations this
project's schema doesn't have). Two are search (below); three are direct
lookups when you already know what you want and don't need to search for
it; `get_observations` fetches full detail for IDs any of the others
already gave you; `timeline` gets chronological context AROUND one
result rather than the result in isolation; one — `add_observation` — is
the only *write* tool among them (see below). This skill is mainly about
finding what's already remembered, but recognizing when a request
actually needs `add_observation` instead of a search matters too:

- `recent_observations(limit?, project?)` — the current project's most
  recent observations, newest first. The same read path `SessionStart`'s
  automatic context injection already uses, reachable on demand (e.g. for
  a project other than the current one, via `project`).
- `session_observations(session_id, limit?)` — every observation from one
  Claude Code session, oldest first. Use when the user asks "what did we
  do in that session" and you have (or can find) the session_id.
- `file_observations(file_path, limit?, project?)` — prior observations
  that mention a specific file being read or modified. The same lookup the
  `PreToolUse` hook already runs automatically right before a `Read`, on
  demand for any file, not just the one about to be read.

Reach for these THREE first when the question doesn't need a query at
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

Depths default to 3 each side, capped at 100. Use this when the user asks
"what led up to X" or "what did we do right after Y," not when they just
want to know what X or Y was — `get_observations` answers that.

### `search_observations` — exact terms

Full-text (keyword) search. Use when the user's question contains specific
words likely to appear verbatim in a title or narrative — error messages,
function names, exact phrases.

```
search_observations(query="authentication token expired", limit=10)
```

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
