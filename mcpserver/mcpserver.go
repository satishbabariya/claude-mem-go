// Package mcpserver exposes claude-mem-go's search/recall surface as an MCP
// (Model Context Protocol) server — the third of the three named gaps
// toward enterprise readiness, and the one real claude-mem covers with
// src/server/mcp/recall-mcp-server.ts + src/servers/mcp-server.ts.
//
// The wire format here isn't guessed from the spec: it's captured from a
// real `claude` CLI session. Configuring --mcp-config to point at a stub
// command that just dumped stdin to a file showed the actual first message:
//
//	{"method":"initialize","params":{"protocolVersion":"2025-11-25", ...},"jsonrpc":"2.0","id":0}
//
// — one JSON-RPC 2.0 object per line, no Content-Length framing (that's
// LSP, not MCP). Everything below is built against that observed shape,
// then verified end to end against the real CLI (see mcpserver_test.go and
// the manual --mcp-config run this was developed with).
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/store"
)

const protocolVersion = "2025-11-25"

// generateSessionID returns a short random hex string — see Server.
// SessionID's doc comment for why one per MCP server process is the
// right scope. Not a cryptographic secret, just a collision-resistant
// grouping key, but crypto/rand costs nothing extra to use correctly here.
func generateSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Never actually expected to fail in practice on any real OS —
		// fall back to a fixed marker rather than leaving SessionID empty
		// (which would make every add_observation call before the next
		// Run() collide on content_hash instead of just sharing a
		// less-random-looking session grouping).
		return "mcp-session-fallback"
	}
	return "mcp-" + hex.EncodeToString(b[:])
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// isNotification reports whether a request carries no id — MCP (like any
// JSON-RPC 2.0 peer) never expects a response to a notification, and
// responding anyway confuses real clients that track outstanding request
// ids.
func isNotification(req rpcRequest) bool { return len(req.ID) == 0 }

// toolDef is one entry of a tools/list response — mirrors the MCP Tool
// shape (name, description, inputSchema as a JSON Schema object).
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func tools() []toolDef {
	return []toolDef{
		{
			Name:        "search_observations",
			Description: "Keyword (full-text) search over claude-mem-go's persisted observations.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":        map[string]any{"type": "string", "description": "Search terms"},
					"limit":        map[string]any{"type": "integer", "description": "Max results (default 10)"},
					"all_projects": map[string]any{"type": "boolean", "description": "Search every project in the store instead of just the current one (default false)"},
				},
				"required": []string{"query"},
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
					"depth_before": map[string]any{"type": "integer", "description": "Observations to include before the anchor (default 3, max 100)"},
					"depth_after":  map[string]any{"type": "integer", "description": "Observations to include after the anchor (default 3, max 100)"},
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
type Server struct {
	DBPath     string
	EmbedModel string // empty disables semantic_search_observations
	// Project scopes search_observations/semantic_search_observations to
	// one project by default — this is one shared database across every
	// project ever recorded on the machine, and an MCP client working on
	// project A must not silently see project B's memory. Empty means "no
	// current project," which searches everything (matches the pre-scoping
	// behavior, used by tests and any caller that genuinely has none).
	Project string
	// SessionID scopes add_observation's manually-added rows the same way
	// PostToolUse's real session_id scopes automatic capture. MCP tool
	// calls carry no session_id of their own (unlike a hook payload), and
	// Claude Code spawns one MCP server process per session (see
	// .mcp.json), so a per-process value generated once at Run() start is
	// the natural substitute — every add_observation call within one real
	// session shares it, and a different session gets a different one for
	// free by virtue of being a different process. Empty until Run()
	// generates one; tests that don't call Run() can set this directly.
	SessionID string
	Log       *log.Logger

	st store.Backend
}

// Run reads newline-delimited JSON-RPC requests from r and writes responses
// to w until r is exhausted or a write fails. It opens the store once for
// the whole session rather than per-call, since a real MCP session's stdio
// pipes stay open for as long as the client keeps them (potentially a
// whole Claude Code session).
func (s *Server) Run(r io.Reader, w io.Writer) error {
	st, err := backend.Open(context.Background(), s.DBPath, 0)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	s.st = st
	defer st.Close()

	if s.SessionID == "" {
		s.SessionID = generateSessionID()
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.Log.Printf("malformed json-rpc line, skipping: %v (%d bytes)", err, len(line))
			continue
		}

		resp := s.handle(req)
		if resp == nil {
			continue // notification — MCP forbids responding to these
		}
		out, err := json.Marshal(resp)
		if err != nil {
			s.Log.Printf("FAILED marshaling response: %v", err)
			continue
		}
		if _, err := w.Write(append(out, '\n')); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return scanner.Err()
}

func (s *Server) handle(req rpcRequest) *rpcResponse {
	s.Log.Printf("<- %s", req.Method)
	switch req.Method {
	case "initialize":
		return s.reply(req, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "claude-mem-go", "version": "0.1.0"},
		})
	case "notifications/initialized":
		return nil // notification: acknowledged by doing nothing
	case "ping":
		return s.reply(req, map[string]any{})
	case "tools/list":
		return s.reply(req, map[string]any{"tools": tools()})
	case "tools/call":
		return s.handleToolCall(req)
	default:
		if isNotification(req) {
			return nil
		}
		return s.errorReply(req, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) reply(req rpcRequest, result any) *rpcResponse {
	if isNotification(req) {
		return nil
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *Server) errorReply(req rpcRequest, code int, message string) *rpcResponse {
	if isNotification(req) {
		return nil
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: message}}
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Query       string   `json:"query"`
		Limit       int      `json:"limit"`
		AllProjects bool     `json:"all_projects"`
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
	} `json:"arguments"`
}

func (s *Server) handleToolCall(req rpcRequest) *rpcResponse {
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
	case "search_observations":
		result = s.runSearch(project, params.Arguments.Query, limit)
	case "semantic_search_observations":
		result = s.runSemanticSearch(project, params.Arguments.Query, limit)
	case "recent_observations":
		result = s.runRecent(scopedProject, limit)
	case "session_observations":
		result = s.runSession(params.Arguments.SessionID, limit)
	case "file_observations":
		result = s.runFile(scopedProject, params.Arguments.FilePath, limit)
	case "get_observations":
		result = s.runGetObservations(project, params.Arguments.IDs)
	case "timeline":
		result = s.runTimeline(scopedProject, params.Arguments.Anchor, params.Arguments.Query,
			params.Arguments.DepthBefore, params.Arguments.DepthAfter)
	case "add_observation":
		result = s.runAddObservation(scopedProject, params.Arguments.Title, params.Arguments.Subtitle,
			params.Arguments.Narrative, params.Arguments.Facts, params.Arguments.Concepts)
	default:
		return s.errorReply(req, -32602, fmt.Sprintf("unknown tool: %s", params.Name))
	}
	return s.reply(req, result)
}

func (s *Server) runSearch(project, query string, limit int) toolCallResult {
	results, err := s.st.Search(project, query, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "search failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSearchResults(results)}}}
}

// runRecent, runSession, and runFile all reuse RecentByProject/BySessionID/
// ObservationsForFile — the exact same Backend methods SessionStart's
// context injection, the Stop hook's session summary, and the PreToolUse
// file-context hook already rely on. They needed no new store code, only
// an MCP surface: the same recall these hooks push automatically was not
// previously reachable on demand.
func (s *Server) runRecent(project string, limit int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	results, err := s.st.RecentByProject(project, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "recent_observations failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSearchResults(results)}}}
}

func (s *Server) runSession(sessionID string, limit int) toolCallResult {
	if sessionID == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "session_observations requires a \"session_id\" argument"}}}
	}
	results, err := s.st.BySessionID(sessionID, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "session_observations failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatSearchResults(results)}}}
}

func (s *Server) runFile(project, filePath string, limit int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	if filePath == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "file_observations requires a \"file_path\" argument"}}}
	}
	results, err := s.st.ObservationsForFile(project, filePath, limit)
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
func (s *Server) runGetObservations(project string, ids []int64) toolCallResult {
	if len(ids) == 0 {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "get_observations requires a non-empty \"ids\" argument"}}}
	}
	results, err := s.st.ByIDs(ids)
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
func (s *Server) runTimeline(project string, anchor int64, query string, depthBefore, depthAfter int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	if anchor == 0 && query == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "timeline requires either an \"anchor\" (observation id) or a \"query\" to find one"}}}
	}
	if depthBefore <= 0 {
		depthBefore = 3
	}
	if depthAfter <= 0 {
		depthAfter = 3
	}

	if anchor == 0 {
		matches, err := s.st.Search(project, query, 1)
		if err != nil {
			return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "timeline: resolving anchor via query failed: " + err.Error()}}}
		}
		if len(matches) == 0 {
			return toolCallResult{Content: []toolContent{{Type: "text", Text: "No observations matched that query, so there's no anchor to build a timeline around."}}}
		}
		anchor = matches[0].ID
	}

	results, err := s.st.Timeline(project, anchor, depthBefore, depthAfter)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "timeline failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatTimeline(results, anchor)}}}
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
func (s *Server) runAddObservation(project, title, subtitle, narrative string, facts, concepts []string) toolCallResult {
	if title == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "add_observation requires a \"title\" argument"}}}
	}
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to add to — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}

	o := store.Observation{Type: "manual", Title: title, Subtitle: subtitle, Narrative: narrative, Facts: facts, Concepts: concepts}
	hash := store.ContentHash(s.SessionID, "manual", title, narrative)
	res, err := s.st.Insert(s.SessionID, project, "manual", hash, o, 0)
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
			s.Log.Printf("add_observation: embedding failed for observations.id=%d (semantic search won't find it): %v", res.ID, embedErr)
			embedNote = " (embedding failed, so semantic search won't find it — keyword search still will)"
		} else if saveErr := s.st.SaveEmbedding(res.ID, vec); saveErr != nil {
			s.Log.Printf("add_observation: saving embedding for observations.id=%d failed: %v", res.ID, saveErr)
			embedNote = " (embedding failed, so semantic search won't find it — keyword search still will)"
		}
	}

	return toolCallResult{Content: []toolContent{{Type: "text", Text: fmt.Sprintf("Remembered (id=%d): %s%s", res.ID, title, embedNote)}}}
}

func (s *Server) runSemanticSearch(project, query string, limit int) toolCallResult {
	if s.EmbedModel == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "semantic search is disabled on this server (no embed model configured)"}}}
	}
	vec, err := embed.NewClient(s.EmbedModel).Embed(query)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "embedding the query failed: " + err.Error()}}}
	}
	matches, err := s.st.SemanticSearch(project, vec, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "semantic search failed: " + err.Error()}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatVectorMatches(matches)}}}
}

func formatSearchResults(results []store.SearchResult) string {
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
func formatTimeline(results []store.SearchResult, anchor int64) string {
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
func formatFullObservations(results []store.SearchResult) string {
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
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatVectorMatches(matches []store.VectorMatch) string {
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
