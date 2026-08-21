---
name: mem-timeline
description: Produce a narrative report of a project's development history from claude-mem-go's stored observations — a full-project timeline, or a week-by-week digest. Use when the user asks for a "timeline report", "project history", "development journey", "what happened last week/month", "weekly digest", or a "story" of how a project got here.
---

# Timeline & Digest Reports

Turn a project's stored observations into a narrative rather than a list.
`mem-search` answers "find the thing I'm thinking of"; this answers "tell
me what happened over this period", which needs the observations in
chronological order and in full, not the top N matches for a term.

## When to use

- "Write a timeline report for this project"
- "What did we get done last week?" / "…this month?"
- "Give me a week-by-week digest of this project"
- "How did we end up with this architecture?"

Not for a specific lookup — "did we fix the parser bug" is `mem-search`.

## The one tool call that makes this possible

`search_observations` **with no `query`** enumerates instead of searching:
every observation matching the other filters, in the order you ask for.

```
search_observations(orderBy="date_asc", limit=100, offset=0)
```

This is the only way to walk a whole project. `recent_observations` has
no `offset` and stops at the 100 most recent, so on any project with more
history than that, the older observations cannot be reached through it at
all.

**Always page.** Keep increasing `offset` by `limit` until a call returns
fewer than `limit` rows. Stopping at the first page silently reports on a
fraction of the project and reads exactly like a complete answer — the
same failure the session summary itself used to have.

## Recipe: full-project timeline

1. Enumerate the whole project, oldest first, paging until exhausted.
2. Note the first and last `created_at` to establish the real span.
3. Group into phases by what changed, not by fixed date buckets — the
   observations' own titles usually make the phase boundaries obvious.
4. Write the narrative: what the project was, what problems came up, what
   was decided, where it ended.

Reach for `type="decision"` as a second pass to pull the decisions out
explicitly — those carry the "why" that `change` observations assume.

## Recipe: week-by-week digest

1. Establish the span (step 2 above).
2. For each ISO week in the span, enumerate just that week:

```
search_observations(dateStart="2026-08-10", dateEnd="2026-08-16",
                    orderBy="date_asc", limit=100)
```

3. Write one chapter per week, carrying forward a short "where things
   stood" block so consecutive chapters read as one story.
4. Say so explicitly when a week has no observations — a quiet week is a
   real finding, not a gap to paper over.

## Getting the detail

Enumeration returns abbreviated `[id] title (project, tool)` lines. When
a specific observation clearly matters to the narrative, pull its full
narrative and facts:

```
get_observations(ids=[42, 87])
```

Do this for the handful that carry the story, not for everything —
`get_observations` is capped at 100 ids and the full text is far more
tokens than the list.

**Pass `all_projects: true` whenever you are reporting on a project other
than the one you're running in** — to `search_observations` *and* to
`get_observations`. Both default to the current project, and
`get_observations` rejects ids belonging to another one. A live run of
this skill hit exactly that: it enumerated another project's 150
observations successfully, then could not read a single narrative,
because the enumeration passed `all_projects` and the follow-up detail
call did not. The report that came back was built from titles alone.

## Reporting honestly

- Say what period the report actually covers, and how many observations
  it is drawn from.
- Don't infer work that isn't recorded. These observations come from tool
  calls, so conversation-only decisions may never have been captured —
  worth stating when the record looks thin.
- If a project is excluded from capture (see `mem-doctor`), say that
  rather than reporting an empty history as if nothing happened.
