package contextfmt

import (
	"strings"
	"testing"

	"claude-mem-go/store"
)

func res(title, subtitle string, next ...string) store.SearchResult {
	return store.SearchResult{Observation: store.Observation{
		Title: title, Subtitle: subtitle, NextSteps: next,
	}}
}

// TestSessionStartWithoutNextSteps pins the ordinary shape — no leading
// block at all when nothing is unfinished. This is the common case, and a
// phantom "Unfinished" heading at the top of every session would be worse
// than the feature is worth.
func TestSessionStartWithoutNextSteps(t *testing.T) {
	got := SessionStart([]store.SearchResult{
		res("switched to Postgres", "SQLite could not keep up"),
	})
	want := "Relevant memory from previous sessions in this project:\n\n" +
		"- switched to Postgres — SQLite could not keep up"
	if got != want {
		t.Fatalf("SessionStart =\n%q\nwant\n%q", got, want)
	}
}

// TestSessionStartLeadsWithNextSteps is the case whose absence let the two
// copies of this formatter drift: the guard that was supposed to keep them
// identical only ever exercised observations WITHOUT next steps, so the
// copy that never gained the feature still matched.
func TestSessionStartLeadsWithNextSteps(t *testing.T) {
	got := SessionStart([]store.SearchResult{
		res("session summary", "wrapped up the migration", "finish the backfill", "delete the old flag"),
		res("switched to Postgres", ""),
	})
	want := "Unfinished from the last session in this project:\n\n" +
		"- finish the backfill\n" +
		"- delete the old flag\n\n" +
		"Relevant memory from previous sessions in this project:\n\n" +
		"- session summary — wrapped up the migration\n" +
		"- switched to Postgres"
	if got != want {
		t.Fatalf("SessionStart =\n%q\nwant\n%q", got, want)
	}
	// Order is the point: unfinished work is useless below a wall of
	// history the reader may never reach.
	if strings.Index(got, "Unfinished") > strings.Index(got, "Relevant memory") {
		t.Error("the unfinished block must come FIRST")
	}
}

// TestLatestNextStepsUsesOnlyTheNewest guards the deliberate choice not to
// merge across sessions. recent is newest-first; steps from older sessions
// were most likely done, and presenting stale intentions as current is
// worse than omitting them.
func TestLatestNextStepsUsesOnlyTheNewest(t *testing.T) {
	got := LatestNextSteps([]store.SearchResult{
		res("newest", ""),
		res("has steps", "", "the current one"),
		res("older", "", "long since done"),
	})
	if len(got) != 1 || got[0] != "the current one" {
		t.Fatalf("LatestNextSteps = %v, want exactly [the current one] — older sessions' steps must not accumulate", got)
	}
}

// TestPromptContextShape pins the other injected block.
func TestPromptContextShape(t *testing.T) {
	got := PromptContext([]store.VectorMatch{
		{Observation: store.Observation{Title: "token TTL", Subtitle: "900 seconds"}},
	})
	want := "Memory relevant to what you just asked:\n\n- token TTL — 900 seconds"
	if got != want {
		t.Fatalf("PromptContext =\n%q\nwant\n%q", got, want)
	}
}
