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
	Log     *log.Logger

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
		Query       string `json:"query"`
		Limit       int    `json:"limit"`
		AllProjects bool   `json:"all_projects"`
		Project     string `json:"project"`    // recent_observations, file_observations: override the current project
		SessionID   string `json:"session_id"` // session_observations
		FilePath    string `json:"file_path"`  // file_observations
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
