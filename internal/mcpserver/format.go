package mcpserver

import (
	"fmt"
	"strings"

	"github.com/satishbabariya/claude-mem-go/internal/contextfmt"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func formatSearchResults(results []memory.SearchResult) string {
	if len(results) == 0 {
		return "No matching observations."
	}
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "[%d] %s (%s, %s)\n", r.ID, r.Observation.Title, r.Project, r.ToolName)
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, "    %s\n", r.Observation.Subtitle)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatTimeline is timeline's formatter — abbreviated like
// formatSearchResults (title/subtitle only; use get_observations for full
// detail on any one of these), but marks the anchor row with "→" so the
// caller can see which one the before/after entries are actually relative
// to, especially when it was resolved automatically from a query rather
// than given directly.
func formatTimeline(results []memory.SearchResult, anchor int64) string {
	if len(results) == 0 {
		return "No observations found."
	}
	var b strings.Builder
	for _, r := range results {
		marker := " "
		if r.ID == anchor {
			marker = "→"
		}
		fmt.Fprintf(&b, "%s [%d] %s (%s, %s)\n", marker, r.ID, r.Observation.Title, r.Project, r.ToolName)
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, "    %s\n", r.Observation.Subtitle)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatFullObservations is get_observations' formatter — the one place
// this server prints narrative/facts/concepts/files, deliberately omitted
// from formatSearchResults/formatVectorMatches to keep list output short.
func formatFullObservations(results []memory.SearchResult) string {
	if len(results) == 0 {
		return "No observations found for those ids (wrong id, already pruned, or belongs to a different project)."
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%d] %s (%s, %s)\n", r.ID, r.Observation.Title, r.Project, r.ToolName)
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, "Subtitle: %s\n", r.Observation.Subtitle)
		}
		if r.Observation.Narrative != "" {
			fmt.Fprintf(&b, "Narrative: %s\n", r.Observation.Narrative)
		}
		if len(r.Observation.Facts) > 0 {
			fmt.Fprintf(&b, "Facts: %s\n", strings.Join(r.Observation.Facts, "; "))
		}
		if len(r.Observation.Concepts) > 0 {
			fmt.Fprintf(&b, "Concepts: %s\n", strings.Join(r.Observation.Concepts, ", "))
		}
		if len(r.Observation.FilesRead) > 0 {
			fmt.Fprintf(&b, "Files read: %s\n", strings.Join(r.Observation.FilesRead, ", "))
		}
		if len(r.Observation.FilesModified) > 0 {
			fmt.Fprintf(&b, "Files modified: %s\n", strings.Join(r.Observation.FilesModified, ", "))
		}
		// Rendered here as well as in contextfmt.SessionStart: a summary's
		// next_steps are its most actionable field, and get_observations is
		// the one tool that claims to show everything about a row.
		if len(r.Observation.NextSteps) > 0 {
			fmt.Fprintf(&b, "Next steps: %s\n", strings.Join(r.Observation.NextSteps, "; "))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatObservationContext delegates to contextfmt.PromptContext, the
// same function cmd/claude-mem-go/prompt_context.go's formatPromptContext
// calls — so observation_context returns the identical ready-to-inject
// text the UserPromptSubmit hook produces automatically, not a fresh
// format only coincidentally similar to it. contextfmt exists precisely
// because mcpserver can't import package main (which imports mcpserver
// for cmdMCP; importing it back would be a cycle): a shared leaf package
// is the only way both callers can use one implementation rather than
// two copies that drift.
func formatObservationContext(matches []memory.VectorMatch) string {
	return contextfmt.PromptContext(matches)
}

// formatSessionStartContext delegates to contextfmt.SessionStart, the
// same function cmd/claude-mem-go/context.go's real SessionStart hook
// calls, for the reason formatObservationContext's own doc comment
// explains: the whole point of session_start_context is returning the
// identical text that hook injects, and a shared package is the only way
// to guarantee that across the import-cycle boundary.
func formatSessionStartContext(recent []memory.SearchResult) string {
	return contextfmt.SessionStart(recent)
}

func formatVectorMatches(matches []memory.VectorMatch) string {
	if len(matches) == 0 {
		return "No embedded observations to search."
	}
	var b strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&b, "[%d] score=%.3f %s (%s, %s)\n", m.ID, m.Score, m.Observation.Title, m.Project, m.ToolName)
		if m.Observation.Subtitle != "" {
			fmt.Fprintf(&b, "    %s\n", m.Observation.Subtitle)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
