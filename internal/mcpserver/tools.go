package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func tools() []toolDef {
	return []toolDef{
		{
			Name: "important_workflow",
			Description: "3-LAYER WORKFLOW (ALWAYS FOLLOW):\n" +
				"1. search_observations(query) -> get an index of IDs (~50-100 tokens/result)\n" +
				"2. timeline(anchor=ID) -> get context around an interesting result\n" +
				"3. get_observations([IDs]) -> fetch full details ONLY for filtered IDs\n" +
				"NEVER fetch full details without filtering first. 10x token savings.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name: "search_observations",
			Description: "Keyword (full-text) search over claude-mem-go's persisted observations. " +
				"Omit `query` to ENUMERATE instead of search: every observation matching the other " +
				"filters, newest first unless orderBy says otherwise. Combined with dateStart/dateEnd " +
				"and offset, that is how you walk a project's full history — e.g. one ISO week at a " +
				"time for a timeline or digest report. recent_observations cannot do this: it has no " +
				"offset and stops at the 100 most recent.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":        map[string]any{"type": "string", "description": "Search terms. Omit or leave empty to enumerate everything matching the other filters instead of searching"},
					"limit":        map[string]any{"type": "integer", "description": "Max results (default 10)"},
					"offset":       map[string]any{"type": "integer", "description": "Skip this many leading results, for paging past a prior call's limit (default 0)"},
					"all_projects": map[string]any{"type": "boolean", "description": "Search every project in the store instead of just the current one (default false)"},
					"type":         map[string]any{"type": "string", "description": "Filter by observation type: discovery, change, decision, summary, or manual. Comma-separated for multiple (default: every type)"},
					"dateStart":    map[string]any{"type": "string", "description": "Only observations created on or after this date (RFC3339 or YYYY-MM-DD)"},
					"dateEnd":      map[string]any{"type": "string", "description": "Only observations created on or before this date (RFC3339 or YYYY-MM-DD)"},
					"orderBy":      map[string]any{"type": "string", "description": "Sort order: date_desc or date_asc (default: relevance when querying, date_desc when enumerating)"},
				},
			},
		},
		{
			Name:        "semantic_search_observations",
			Description: "Meaning-based search over claude-mem-go's persisted observations, via local embeddings + cosine similarity. Finds relevant observations even without keyword overlap.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":        map[string]any{"type": "string", "description": "A natural-language question or description"},
					"limit":        map[string]any{"type": "integer", "description": "Max results (default 10)"},
					"all_projects": map[string]any{"type": "boolean", "description": "Search every project in the store instead of just the current one (default false)"},
				},
				"required": []string{"query"},
			},
		},
		{
			Name: "observation_context",
			Description: "Get a ready-to-inject memory context block for a query — the on-demand form of " +
				"the UserPromptSubmit hook's automatic semantic recall, for explicitly asking \"what does " +
				"memory know relevant to X?\" mid-session. Unlike semantic_search_observations, which returns " +
				"a list of results for a caller to interpret, this returns pre-formatted text meant to be " +
				"read or dropped directly into a prompt.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":        map[string]any{"type": "string", "description": "A natural-language question or description"},
					"limit":        map[string]any{"type": "integer", "description": "Max observations to include (default 10)"},
					"all_projects": map[string]any{"type": "boolean", "description": "Search every project in the store instead of just the current one (default false)"},
				},
				"required": []string{"query"},
			},
		},
		{
			Name: "recent_observations",
			Description: "The most recent observations for the current project, newest first — " +
				"what happened lately, without a search query. Same read path SessionStart's automatic " +
				"context injection uses.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit":   map[string]any{"type": "integer", "description": "Max results (default 10)"},
					"project": map[string]any{"type": "string", "description": "Look at a different project instead of the current one"},
				},
			},
		},
		{
			Name: "session_start_context",
			Description: "Render the exact text the SessionStart hook injects automatically at the start of " +
				"a session for a project — matches real claude-mem's own session_start_context tool, which " +
				"calls the same /api/context/inject path its SessionStart hook uses. Unlike " +
				"recent_observations (same underlying data, but the abbreviated [id]-prefixed list format), " +
				"this returns the identical prose block a real session actually saw.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit":   map[string]any{"type": "integer", "description": "Max observations to include (default 5, matching the real hook's own default)"},
					"project": map[string]any{"type": "string", "description": "Look at a different project instead of the current one"},
				},
			},
		},
		{
			Name:        "session_observations",
			Description: "Every observation recorded for one Claude Code session, oldest first — what actually happened during that session, in order.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string", "description": "The session_id to look up"},
					"limit":      map[string]any{"type": "integer", "description": "Max results (default 10)"},
				},
				"required": []string{"session_id"},
			},
		},
		{
			Name: "file_observations",
			Description: "Prior observations that mention a specific file (read or modified) in the current " +
				"project — the same lookup the PreToolUse file-context hook runs automatically before a Read, " +
				"available on demand for any file, not just the one about to be read.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string", "description": "Path as it appears in files_read/files_modified (exact match, not a substring)"},
					"limit":     map[string]any{"type": "integer", "description": "Max results (default 10)"},
					"project":   map[string]any{"type": "string", "description": "Look at a different project instead of the current one"},
				},
				"required": []string{"file_path"},
			},
		},
		{
			Name: "get_observations",
			Description: "Fetch full details (narrative, facts, concepts, files) for specific observation IDs — " +
				"the other tools' list output is deliberately abbreviated (title/subtitle only) to keep results " +
				"short; use the [id] shown there with this tool to see everything about one or more of them.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Observation IDs to fetch (required)"},
				},
				"required": []string{"ids"},
			},
		},
		{
			Name: "timeline",
			Description: "Get chronological context AROUND one observation — depth_before/depth_after " +
				"observations immediately before and after it, in order. Use after a search to see what led " +
				"up to or followed a result, not just the result in isolation. Give it either \"anchor\" " +
				"(an observation ID, e.g. from a prior search_observations result) directly, or a \"query\" " +
				"to find the anchor automatically (the single best keyword match becomes the anchor).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"anchor":       map[string]any{"type": "integer", "description": "Observation ID to center the timeline on"},
					"query":        map[string]any{"type": "string", "description": "Find the anchor automatically via keyword search, if \"anchor\" isn't given"},
					"depth_before": map[string]any{"type": "integer", "description": "Observations to include before the anchor (default 10, max 100)"},
					"depth_after":  map[string]any{"type": "integer", "description": "Observations to include after the anchor (default 10, max 100)"},
					"project":      map[string]any{"type": "string", "description": "Look at a different project instead of the current one"},
				},
			},
		},
		{
			Name: "add_observation",
			Description: "Explicitly persist a fact, decision, or preference into claude-mem-go's memory — " +
				"for something worth remembering that isn't the direct result of one tool call. Automatic " +
				"capture (the PostToolUse hook) already records what tool calls did; use this for something " +
				"that should be recalled in a future session on its own, e.g. a decision the user just made " +
				"or a preference they stated.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":     map[string]any{"type": "string", "description": "Short summary (required)"},
					"subtitle":  map[string]any{"type": "string", "description": "One-line detail"},
					"narrative": map[string]any{"type": "string", "description": "Fuller explanation, if useful"},
					"facts":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Discrete facts worth keeping separately"},
					"concepts":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Concepts/tags that help future search find this"},
					"project":   map[string]any{"type": "string", "description": "Defaults to the current project"},
				},
				"required": []string{"title"},
			},
		},
	}
}

// toolContent is one MCP tool-result content block. Text is the only shape
// this server produces.
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Server serves the MCP stdio protocol against one sqlite Store.
type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Query       string   `json:"query"`
		Limit       int      `json:"limit"`
		Offset      int      `json:"offset"` // search_observations: skip this many leading results
		AllProjects bool     `json:"all_projects"`
		ObsType     string   `json:"type"`         // search_observations: filter by observation type
		Project     string   `json:"project"`      // recent_observations, file_observations, add_observation: override the current project
		SessionID   string   `json:"session_id"`   // session_observations
		FilePath    string   `json:"file_path"`    // file_observations
		Title       string   `json:"title"`        // add_observation
		Subtitle    string   `json:"subtitle"`     // add_observation
		Narrative   string   `json:"narrative"`    // add_observation
		Facts       []string `json:"facts"`        // add_observation
		Concepts    []string `json:"concepts"`     // add_observation
		IDs         []int64  `json:"ids"`          // get_observations
		Anchor      int64    `json:"anchor"`       // timeline
		DepthBefore int      `json:"depth_before"` // timeline
		DepthAfter  int      `json:"depth_after"`  // timeline
		DateStart   string   `json:"dateStart"`    // search_observations
		DateEnd     string   `json:"dateEnd"`      // search_observations
		OrderBy     string   `json:"orderBy"`      // search_observations
	} `json:"arguments"`
}

func (s *Server) handleToolCall(ctx context.Context, req rpcRequest) *rpcResponse {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errorReply(req, -32602, fmt.Sprintf("invalid params: %v", err))
	}
	// Bounded the same way real claude-mem's own mem-search skill documents
	// its search tool ("max 100") — an unbounded limit lets a single call
	// force a scan/transfer sized however large the caller likes, which for
	// an MCP tool (arguments come from whatever's calling the server, not
	// necessarily a careful human) is worth capping rather than trusting.
	const maxLimit = 100
	limit := params.Arguments.Limit
	if limit <= 0 {
		limit = 10
	} else if limit > maxLimit {
		limit = maxLimit
	}
	offset := params.Arguments.Offset
	if offset < 0 {
		offset = 0
	}

	project := s.Project
	if params.Arguments.AllProjects {
		project = ""
	}

	// recent_observations and file_observations take their own optional
	// "project" argument (a different project on purpose), separate from
	// search's "all_projects" escape hatch (every project at once) — the
	// two tools have different shapes of override because "all recent
	// observations across every project, unscoped" isn't a coherent
	// request the way "search everything" is.
	scopedProject := s.Project
	if params.Arguments.Project != "" {
		scopedProject = params.Arguments.Project
	}

	var result toolCallResult
	switch params.Name {
	case "important_workflow":
		result = runImportantWorkflow()
	case "search_observations":
		dateStartMs, dsErr := memory.ParseDateArg(params.Arguments.DateStart)
		dateEndMs, deErr := memory.ParseDateArg(params.Arguments.DateEnd)
		if dsErr != nil {
			result = toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "search_observations: invalid dateStart: " + dsErr.Error()}}}
		} else if deErr != nil {
			result = toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "search_observations: invalid dateEnd: " + deErr.Error()}}}
		} else {
			result = s.runSearch(ctx, project, params.Arguments.Query, params.Arguments.ObsType, limit, offset,
				dateStartMs, dateEndMs, params.Arguments.OrderBy)
		}
	case "semantic_search_observations":
		result = s.runSemanticSearch(ctx, project, params.Arguments.Query, limit)
	case "observation_context":
		result = s.runObservationContext(ctx, project, params.Arguments.Query, limit)
	case "recent_observations":
		result = s.runRecent(ctx, scopedProject, limit)
	case "session_start_context":
		// Its own default (5), not the shared 10 every other tool above
		// uses — matching cmd/claude-mem-go/context.go's real SessionStart
		// hook default exactly, since the entire point of this tool is
		// returning what that hook actually injects.
		startLimit := params.Arguments.Limit
		if startLimit <= 0 {
			startLimit = 5
		} else if startLimit > maxLimit {
			startLimit = maxLimit
		}
		result = s.runSessionStartContext(ctx, scopedProject, startLimit)
	case "session_observations":
		result = s.runSession(ctx, params.Arguments.SessionID, limit)
	case "file_observations":
		result = s.runFile(ctx, scopedProject, params.Arguments.FilePath, limit)
	case "get_observations":
		result = s.runGetObservations(ctx, project, params.Arguments.IDs)
	case "timeline":
		result = s.runTimeline(ctx, scopedProject, params.Arguments.Anchor, params.Arguments.Query,
			params.Arguments.DepthBefore, params.Arguments.DepthAfter)
	case "add_observation":
		result = s.runAddObservation(ctx, scopedProject, params.Arguments.Title, params.Arguments.Subtitle,
			params.Arguments.Narrative, params.Arguments.Facts, params.Arguments.Concepts)
	default:
		return s.errorReply(req, -32602, fmt.Sprintf("unknown tool: %s", params.Name))
	}
	return s.reply(req, result)
}

// runImportantWorkflow is important_workflow — matches real claude-mem's
// own tool of the same name and shape: a zero-dependency, static-text
// tool that exists purely to teach an MCP client the intended usage
// pattern between the OTHER tools, not to look anything up itself. Unlike
// every other tool here, it never touches s.st — real claude-mem's
// version has this same "no server/handler dependency at all" property,
// unlike most of its other tools (which the tree-sitter/knowledge-graph
// system backs and are genuinely out of scope for this port).
//
// The point this teaches is real, not decorative: get_observations
// returns full narrative/facts/concepts/files for every ID it's given,
// which costs real tokens per result — fetching that for every row a
// broad search_observations/semantic_search_observations call returns,
// instead of first narrowing to a few IDs worth a closer look, wastes
// exactly the token budget this project's whole abbreviated-list-output
// convention (title/subtitle only, see get_observations's own
// description) exists to protect.
func validateAddObservationSize(title, subtitle, narrative string, facts, concepts []string) string {
	if len(title) > maxObservationTitleBytes {
		return fmt.Sprintf("add_observation: \"title\" is %d bytes, which exceeds the %d-byte limit", len(title), maxObservationTitleBytes)
	}
	if len(subtitle) > maxObservationSubtitleBytes {
		return fmt.Sprintf("add_observation: \"subtitle\" is %d bytes, which exceeds the %d-byte limit", len(subtitle), maxObservationSubtitleBytes)
	}
	if len(narrative) > maxObservationNarrativeBytes {
		return fmt.Sprintf("add_observation: \"narrative\" is %d bytes, which exceeds the %d-byte limit", len(narrative), maxObservationNarrativeBytes)
	}
	if len(facts) > maxObservationFactsCount {
		return fmt.Sprintf("add_observation: %d \"facts\" exceeds the %d-item limit", len(facts), maxObservationFactsCount)
	}
	for _, f := range facts {
		if len(f) > maxObservationFactBytes {
			return fmt.Sprintf("add_observation: a \"facts\" item is %d bytes, which exceeds the %d-byte limit", len(f), maxObservationFactBytes)
		}
	}
	if len(concepts) > maxObservationConceptsCount {
		return fmt.Sprintf("add_observation: %d \"concepts\" exceeds the %d-item limit", len(concepts), maxObservationConceptsCount)
	}
	for _, c := range concepts {
		if len(c) > maxObservationConceptBytes {
			return fmt.Sprintf("add_observation: a \"concepts\" item is %d bytes, which exceeds the %d-byte limit", len(c), maxObservationConceptBytes)
		}
	}
	return ""
}

// runAddObservation is the write side of this server's otherwise
// read-only surface: every other tool reads what PostToolUse already
// captured automatically. This lets Claude explicitly persist something
// worth remembering that isn't the direct result of one tool call — a
// decision, a stated preference — the same real gap real claude-mem's own
// observation_add tool closes. ContentHash(SessionID, "manual", title,
// narrative) reuses the exact same idempotency mechanism automatic
// capture relies on: calling this twice with the same title/narrative in
// the same session is a no-op, not a duplicate.

// runRecent, runSession, and runFile all reuse RecentByProject/BySessionID/
// ObservationsForFile — the exact same Backend methods SessionStart's
// context injection, the Stop hook's session summary, and the PreToolUse
// file-context hook already rely on. They needed no new store code, only
// an MCP surface: the same recall these hooks push automatically was not
// previously reachable on demand.
func (s *Server) runRecent(ctx context.Context, project string, limit int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	results, err := s.st.RecentByProject(ctx, project, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "recent_observations failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSearchResults(results)}}}
}

// runSessionStartContext is session_start_context — matches real
// claude-mem's own tool of the same name, which renders the exact text
// its SessionStart-equivalent injection path produces (real claude-mem's
// handleSessionStartContext calls /api/context/inject, backed by the
// same generateContextWithStats its SessionStart hook uses). Reuses
// RecentByProject, the same Backend method cmd/claude-mem-go/context.go's
// real SessionStart hook calls — the point isn't a new read path, it's
// exposing the SAME one on demand, formatted identically
// (formatSessionStartContext duplicates context.go's formatContext byte
// for byte) rather than through recent_observations' different,
// abbreviated [id]-prefixed list shape.
func (s *Server) runSessionStartContext(ctx context.Context, project string, limit int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	recent, err := s.st.RecentByProject(ctx, project, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "session_start_context failed: " + err.Error()}}}
	}
	if len(recent) == 0 {
		// The real hook would inject nothing at all in this case (an
		// empty additionalContext) — this text exists only because an MCP
		// tool call still needs a non-empty response to say so explicitly.
		return toolCallResult{Content: []toolContent{{Type: "text", Text: "No prior observations for this project — SessionStart would inject nothing."}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSessionStartContext(recent)}}}
}

func (s *Server) runSession(ctx context.Context, sessionID string, limit int) toolCallResult {
	if sessionID == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "session_observations requires a \"session_id\" argument"}}}
	}
	results, err := s.st.BySessionID(ctx, sessionID, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "session_observations failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSearchResults(results)}}}
}

func (s *Server) runFile(ctx context.Context, project, filePath string, limit int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	if filePath == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "file_observations requires a \"file_path\" argument"}}}
	}
	results, err := s.st.ObservationsForFile(ctx, project, filePath, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "file_observations failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSearchResults(results)}}}
}

// runGetObservations is the detail-lookup companion to every list-shaped
// tool above (search_observations, recent_observations, etc.): their output
// deliberately shows only title/subtitle to keep results short (see
// formatSearchResults), so there was previously no way to see an
// observation's narrative/facts/concepts/files without a separate CLI
// invocation outside the MCP surface entirely.
//
// Scoped to the current project the same way search_observations is (via
// the caller's own "all_projects" argument, already resolved into project
// by the time this is called) — without that, a caller that merely guessed
// or iterated IDs could read another project's observations out of this
// single shared database, the same class of cross-project leak
// Search/SemanticSearch were fixed for earlier.
func (s *Server) runGetObservations(ctx context.Context, project string, ids []int64) toolCallResult {
	if len(ids) == 0 {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "get_observations requires a non-empty \"ids\" argument"}}}
	}
	results, err := s.st.ByIDs(ctx, ids)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "get_observations failed: " + err.Error()}}}
	}
	if project != "" {
		scoped := results[:0]
		for _, r := range results {
			if r.Project == project {
				scoped = append(scoped, r)
			}
		}
		results = scoped
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatFullObservations(results)}}}
}

// runTimeline is the chronological-context companion to search: real
// claude-mem's own `timeline` tool exists as "step 2" of its own
// search→timeline→get_observations pipeline ("get context around
// results"), and this project's version covers the identical real need —
// a search result in isolation doesn't say what led up to it or came
// right after, and RecentByProject/BySessionID answer a different
// question ("what's recent"/"what happened this session") than "what
// surrounds this ONE specific observation."
//
// anchor is used directly if non-zero; otherwise query resolves one via a
// single-result Search — the same "find it for me" convenience real
// claude-mem's own timeline tool offers. Scoping and the cross-project
// anchor check both happen inside Backend.Timeline itself, the same
// single source of truth ByIDs/Search already rely on for their own
// project-safety checks.
func (s *Server) runTimeline(ctx context.Context, project string, anchor int64, query string, depthBefore, depthAfter int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	if anchor == 0 && query == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "timeline requires either an \"anchor\" (observation id) or a \"query\" to find one"}}}
	}
	if anchor != 0 && query != "" {
		// Matches real claude-mem's own SearchManager.timeline explicit
		// error for this — this port used to silently prefer anchor and
		// ignore query with no error at all, which could hide a genuine
		// caller mistake (e.g. a stale query left over from copy-pasting
		// a different call) rather than surfacing it.
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "timeline: cannot provide both \"anchor\" and \"query\" — use one or the other"}}}
	}
	// 10, not 3: real claude-mem's own SearchManager.timeline defaults
	// depth_before/depth_after to 10 when omitted (its tool schema's own
	// description text says "default 3," but that's a real doc/behavior
	// mismatch in claude-mem itself — this matches what actually happens
	// on a real call, not the stale doc string).
	if depthBefore <= 0 {
		depthBefore = 10
	}
	if depthAfter <= 0 {
		depthAfter = 10
	}

	if anchor == 0 {
		matches, err := s.st.Search(ctx, project, query, "", 1, 0, 0, 0, "")
		if err != nil {
			return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "timeline: resolving anchor via query failed: " + err.Error()}}}
		}
		if len(matches) == 0 {
			return toolCallResult{Content: []toolContent{{Type: "text", Text: "No observations matched that query, so there's no anchor to build a timeline around."}}}
		}
		anchor = matches[0].ID
	}

	results, err := s.st.Timeline(ctx, project, anchor, depthBefore, depthAfter)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "timeline failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatTimeline(results, anchor)}}}
}

// Size bounds for add_observation's free-text arguments — unlike every
// other tool here, these come straight from whatever's calling the MCP
// server (arguments are JSON strings/arrays with no schema-enforced
// length) and land, unlike a hook payload, directly in a database row and
// an Ollama embedding request rather than being read once and discarded.
// Before this, an MCP client (accidentally or otherwise) could hand
// add_observation a multi-megabyte "title" and it would be accepted
// exactly like a real one: stored forever, printed in full by every list
// tool's formatSearchResults/formatFullObservations output (unlike
// PostToolUse's captured fields, which transcript.FieldCap already bounds
// before an observation is ever built), and shipped whole to
// embed.ObservationText's embedding request.
//
// The per-field caps mirror what observer.go's own prompt already asks a
// well-behaved caller for — title a "short title", subtitle a "one-line
// detail", narrative "one paragraph" — sized several times larger than any
// real value ever needs so genuine content is never truncated, the same
// generous-but-real-bound approach transcript.FieldCap takes for hook
// payload fields. The facts/concepts count and per-item caps match the
// "no legitimate caller needs more than a page" reasoning already
// governing memory.MaxIDsPerLookup and memory.MaxTimelineDepth: facts and
// concepts are meant to be a handful of discrete, short items (a fact is
// a sentence, a concept is a tag), not an unbounded list.
const (
	maxObservationTitleBytes     = 500
	maxObservationSubtitleBytes  = 1000
	maxObservationNarrativeBytes = 10000
	maxObservationFactsCount     = 50
	maxObservationFactBytes      = 1000
	maxObservationConceptsCount  = 50
	maxObservationConceptBytes   = 200
)
