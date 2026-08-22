package mcpserver

import (
	"context"
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func runImportantWorkflow() toolCallResult {
	return toolCallResult{Content: []toolContent{{Type: "text", Text: `# Memory Search Workflow

**3-Layer Pattern (ALWAYS follow this):**

1. **search_observations** (or semantic_search_observations) - Get an index of results with IDs
   search_observations(query="...", limit=20)
   Returns: Abbreviated list with IDs, titles, subtitles (~50-100 tokens/result)

2. **timeline** - Get context around an interesting result
   timeline(anchor=<ID>, depth_before=3, depth_after=3)
   Returns: Chronological context showing what was happening around it

3. **get_observations** - Get full details ONLY for the relevant IDs
   get_observations(ids=[...])  # batch for 2+ items
   Returns: Complete details — narrative, facts, concepts, files (~500-1000 tokens/result)

**Why:** 10x token savings. Never fetch full details without filtering first.`}}}
}

func (s *Server) runSearch(ctx context.Context, project, query, obsType string, limit, offset int, dateStartMs, dateEndMs int64, orderBy string) toolCallResult {
	results, err := s.st.Search(ctx, project, query, obsType, limit, offset, dateStartMs, dateEndMs, orderBy)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "search failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSearchResults(results)}}}
}

// validateAddObservationSize checks add_observation's free-text arguments
// against the bounds above, returning a non-empty message identifying the
// first violation found (checked in the same order the fields are
// declared in toolCallParams) or "" if everything is within bounds.
func (s *Server) runAddObservation(ctx context.Context, project, title, subtitle, narrative string, facts, concepts []string) toolCallResult {
	if title == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "add_observation requires a \"title\" argument"}}}
	}
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to add to — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	if msg := validateAddObservationSize(title, subtitle, narrative, facts, concepts); msg != "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: msg}}}
	}

	o := memory.Observation{Type: "manual", Title: title, Subtitle: subtitle, Narrative: narrative, Facts: facts, Concepts: concepts}
	hash := memory.ContentHash(s.SessionID, "manual", title, narrative)
	res, err := s.st.Insert(ctx, s.SessionID, project, "manual", hash, o, 0)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "add_observation failed: " + err.Error()}}}
	}
	if !res.Inserted {
		return toolCallResult{Content: []toolContent{{Type: "text",
			Text: fmt.Sprintf("Already remembered (id=%d) — an identical observation (same title and narrative) was already added this session.", res.ID)}}}
	}

	// Additive only, same as worker.process's own embedding step: keyword
	// search on the row just inserted already works without this, and a
	// missing/unreachable Ollama must not undo a successful add. Without
	// this, a manually-added observation would be a second-class citizen
	// next to automatic capture — findable by search_observations/
	// recent_observations, but invisible to semantic_search_observations.
	embedNote := ""
	if s.EmbedModel != "" {
		text := embed.ObservationText(title, subtitle, narrative, facts)
		if vec, embedErr := embed.NewClient(s.EmbedModel).Embed(text); embedErr != nil {
			s.Log.Warnf("add_observation: embedding failed for observations.id=%d (semantic search won't find it): %v", res.ID, embedErr)
			embedNote = " (embedding failed, so semantic search won't find it — keyword search still will)"
		} else if saveErr := s.st.SaveEmbedding(ctx, res.ID, vec); saveErr != nil {
			s.Log.Warnf("add_observation: saving embedding for observations.id=%d failed: %v", res.ID, saveErr)
			embedNote = " (embedding failed, so semantic search won't find it — keyword search still will)"
		}
	}

	return toolCallResult{Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Remembered (id=%d): %s%s", res.ID, title, embedNote)}}}
}

func (s *Server) runSemanticSearch(ctx context.Context, project, query string, limit int) toolCallResult {
	if s.EmbedModel == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "semantic search is disabled on this server (no embed model configured)"}}}
	}
	vec, err := embed.NewClient(s.EmbedModel).Embed(query)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "embedding the query failed: " + err.Error()}}}
	}
	matches, err := s.st.SemanticSearch(ctx, project, vec, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "semantic search failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatVectorMatches(matches)}}}
}

// runObservationContext is observation_context — the on-demand form of
// cmd/claude-mem-go's prompt_context.go, the UserPromptSubmit hook that
// fires automatically against the actual text a user submits. This lets
// a caller (or Claude itself, mid-session) explicitly ask "what does
// memory know relevant to X?" and get back the SAME joined,
// ready-to-inject text that hook produces, not a raw list like
// runSemanticSearch just above — closest in shape (same embed +
// SemanticSearch pipeline) but meant as data for a caller to interpret,
// not text meant to be dropped directly into a prompt. Matches real
// claude-mem's own observation_context tool: one of the few read
// capabilities this server's hooks already had that had no on-demand MCP
// equivalent, unlike RecentByProject/BySessionID/ObservationsForFile
// (recent_observations/session_observations/file_observations).
func (s *Server) runObservationContext(ctx context.Context, project, query string, limit int) toolCallResult {
	if s.EmbedModel == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "observation_context is disabled on this server (no embed model configured)"}}}
	}
	if query == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "observation_context requires a \"query\" argument"}}}
	}
	vec, err := embed.NewClient(s.EmbedModel).Embed(query)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "embedding the query failed: " + err.Error()}}}
	}
	matches, err := s.st.SemanticSearch(ctx, project, vec, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "observation_context failed: " + err.Error()}}}
	}
	if len(matches) == 0 {
		return toolCallResult{Content: []toolContent{{Type: "text", Text: "No embedded observations relevant to that query."}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatObservationContext(matches)}}}
}
