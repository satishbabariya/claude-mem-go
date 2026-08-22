// Package contextfmt renders the memory blocks this project injects into
// Claude Code, in one place.
//
// It exists because these formatters were duplicated — once in the hooks
// under package main, once in mcpserver, whose on-demand tools
// deliberately return the byte-identical text the hooks produce
// automatically. The duplication was justified by an import cycle:
// mcpserver cannot import package main. But main already imports
// mcpserver (for cmdMCP), so the cycle only ever blocked one direction,
// and the copies were kept in step by a test asserting one fixture.
//
// That is not sufficient, and it failed exactly as you would expect. When
// SessionStart's block gained an "Unfinished from the last session"
// section for next_steps, only the hook's copy got it; mcpserver's copy
// silently diverged, and the byte-for-byte guard still passed because its
// fixture had no next steps. A test that pins one example cannot pin an
// invariant about every future field.
//
// A neutral package neither side owns removes the class of bug rather
// than the instance: there is now one implementation, so "identical" is
// true by construction instead of by vigilance.
package contextfmt

import (
	"fmt"
	"strings"

	"claude-mem-go/store"
)

// SessionStart renders the block the SessionStart hook injects, and that
// the session_start_context MCP tool returns verbatim.
func SessionStart(recent []store.SearchResult) string {
	var b strings.Builder
	// Unfinished work goes FIRST and separately, because it is the one
	// thing here that is not merely context. Everything below records what
	// happened; these are the things that had NOT happened when the last
	// session ended, which is what a new session most needs before
	// deciding what to do.
	if steps := LatestNextSteps(recent); len(steps) > 0 {
		b.WriteString("Unfinished from the last session in this project:\n\n")
		for _, st := range steps {
			fmt.Fprintf(&b, "- %s\n", st)
		}
		b.WriteString("\n")
	}
	b.WriteString("Relevant memory from previous sessions in this project:\n\n")
	for _, r := range recent {
		fmt.Fprintf(&b, "- %s", r.Observation.Title)
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, " — %s", r.Observation.Subtitle)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// LatestNextSteps returns the next steps from the newest observation that
// has any. recent is newest-first, so the first hit is the most recent —
// and only that one is used, rather than accumulating every session's
// leftovers into a growing list of things probably long since done.
func LatestNextSteps(recent []store.SearchResult) []string {
	for _, r := range recent {
		if len(r.Observation.NextSteps) > 0 {
			return r.Observation.NextSteps
		}
	}
	return nil
}

// PromptContext renders the block the UserPromptSubmit hook injects, and
// that the observation_context MCP tool returns verbatim.
func PromptContext(matches []store.VectorMatch) string {
	var b strings.Builder
	b.WriteString("Memory relevant to what you just asked:\n\n")
	for _, m := range matches {
		fmt.Fprintf(&b, "- %s", m.Observation.Title)
		if m.Observation.Subtitle != "" {
			fmt.Fprintf(&b, " — %s", m.Observation.Subtitle)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
