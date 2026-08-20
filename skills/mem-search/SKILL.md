---
name: mem-search
description: Search claude-mem-go's persistent cross-session memory database. Use when the user asks "did we already solve this?", "how did we do X last time?", or needs work from a previous session that SessionStart's automatic context injection didn't already surface.
---

# Memory Search

claude-mem-go already injects a project's most recent observations
automatically at `SessionStart` — this skill is for everything that
injection doesn't cover: older sessions, a different project, or a
specific question phrased in a way worth searching for rather than
skimming a handful of recent items.

## When to use

Use when the user asks about PAST sessions, not the current conversation:

- "Did we already fix this?"
- "How did we solve X last time?"
- "What did we find out about Y?"

## Two tools, two different jobs

claude-mem-go exposes exactly two MCP tools — simpler than a multi-step
index/timeline/fetch pipeline, because this project's schema doesn't carry
the token-cost concerns that pipeline exists to manage (no separate raw-vs-
compressed representations to fetch in stages).

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
