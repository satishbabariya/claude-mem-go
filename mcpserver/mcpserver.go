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
	"runtime/debug"
	"strings"

	"claude-mem-go/backend"
	"claude-mem-go/embed"
	"claude-mem-go/store"
)

const protocolVersion = "2025-11-25"

// serverVersion reports the build's git commit, the same source of truth
// `version`/`doctor` already use (see cmd/claude-mem-go/version.go's own
// doc comment for why: Go's VCS stamping needs no separate version file
// to keep in sync). A real, found-by-hand staleness bug this replaces:
// the "initialize" response's serverInfo.version was hardcoded to "0.1.0"
// and never updated across several real version bumps since — an MCP
// client introspecting it got a number three releases stale, silently
// wrong in exactly the way a hand-maintained version string always
// eventually is. mcpserver can't import cmd/claude-mem-go's
// buildVersionString directly (that package imports mcpserver for cmdMCP;
// importing back would cycle, the same constraint formatObservationContext's
// own doc comment explains), so this computes the short revision directly
// rather than duplicating that function's full human-readable formatting
// — a raw commit hash is enough for what serverInfo.version is for
// (correlating a report against a build), not something a person reads.
func serverVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	return "unknown"
}

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
	// HNSWEfSearch overrides pgvector's own hnsw.ef_search query-time
	// recall/speed tradeoff for semantic_search_observations and
	// observation_context — see postgres.Store.SemanticSearch's own doc
	// comment. Ignored entirely for a SQLite DBPath. 0 (the zero value)
	// leaves pgvector's built-in default (40) in place.
	HNSWEfSearch int
	Log          *log.Logger

	st store.Backend
}

// Run reads newline-delimited JSON-RPC requests from r and writes responses
// to w until r is exhausted or a write fails. It opens the store once for
// the whole session rather than per-call, since a real MCP session's stdio
// pipes stay open for as long as the client keeps them (potentially a
// whole Claude Code session).
func (s *Server) Run(r io.Reader, w io.Writer) error {
	st, err := backend.Open(context.Background(), s.DBPath, 0, s.HNSWEfSearch)
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

func (s *Server) handle(req rpcRequest) (resp *rpcResponse) {
	// handle runs synchronously in Run's scanner loop, not a spawned
	// goroutine — an unrecovered panic here doesn't just fail one tool
	// call, it terminates the entire process (Go's default for a panic
	// that unwinds past main), ending the whole MCP session and, if this
	// was mid-write, potentially the whole `claude` session using it.
	// Found not hypothetically: a real, reproducible panic existed in
	// store.Store.SemanticSearch (a negative limit slicing out of
	// bounds) before that call site was fixed — this recover is the
	// backstop for that entire class of bug, not a substitute for fixing
	// root causes when they're found.
	defer func() {
		if r := recover(); r != nil {
			s.Log.Printf("PANIC recovered handling %s: %v", req.Method, r)
			resp = s.errorReply(req, -32603, fmt.Sprintf("internal error: %v", r))
		}
	}()
	s.Log.Printf("<- %s", req.Method)
	switch req.Method {
	case "initialize":
		return s.reply(req, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "claude-mem-go", "version": serverVersion()},
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
		dateStartMs, dsErr := store.ParseDateArg(params.Arguments.DateStart)
		dateEndMs, deErr := store.ParseDateArg(params.Arguments.DateEnd)
		if dsErr != nil {
			result = toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "search_observations: invalid dateStart: " + dsErr.Error()}}}
		} else if deErr != nil {
			result = toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "search_observations: invalid dateEnd: " + deErr.Error()}}}
		} else {
			result = s.runSearch(project, params.Arguments.Query, params.Arguments.ObsType, limit, offset,
				dateStartMs, dateEndMs, params.Arguments.OrderBy)
		}
	case "semantic_search_observations":
		result = s.runSemanticSearch(project, params.Arguments.Query, limit)
	case "observation_context":
		result = s.runObservationContext(project, params.Arguments.Query, limit)
	case "recent_observations":
		result = s.runRecent(scopedProject, limit)
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
		result = s.runSessionStartContext(scopedProject, startLimit)
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

func (s *Server) runSearch(project, query, obsType string, limit, offset int, dateStartMs, dateEndMs int64, orderBy string) toolCallResult {
	results, err := s.st.Search(project, query, obsType, limit, offset, dateStartMs, dateEndMs, orderBy)
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
func (s *Server) runSessionStartContext(project string, limit int) toolCallResult {
	if project == "" {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text",
			Text: "no project to look up — the server has no current project (unusual outside a real cwd) and no \"project\" argument was given"}}}
	}
	recent, err := s.st.RecentByProject(project, limit)
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
		matches, err := s.st.Search(project, query, "", 1, 0, 0, 0, "")
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
// governing store.MaxIDsPerLookup and store.MaxTimelineDepth: facts and
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

// validateAddObservationSize checks add_observation's free-text arguments
// against the bounds above, returning a non-empty message identifying the
// first violation found (checked in the same order the fields are
// declared in toolCallParams) or "" if everything is within bounds.
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
func (s *Server) runAddObservation(project, title, subtitle, narrative string, facts, concepts []string) toolCallResult {
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
func (s *Server) runObservationContext(project, query string, limit int) toolCallResult {
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
	matches, err := s.st.SemanticSearch(project, vec, limit)
	if err != nil {
		return toolCallResult{IsError: true, Content: []toolContent{{Type: "text", Text: "observation_context failed: " + err.Error()}}}
	}
	if len(matches) == 0 {
		return toolCallResult{Content: []toolContent{{Type: "text", Text: "No embedded observations relevant to that query."}}}
	}
	return toolCallResult{Content: []toolContent{{Type: "text", Text: formatObservationContext(matches)}}}
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

// formatObservationContext mirrors cmd/claude-mem-go/prompt_context.go's
// formatPromptContext exactly, byte for byte — duplicated rather than
// imported, since mcpserver can't import package main (which itself
// imports mcpserver for cmdMCP; importing it back would be a cycle). The
// whole point of this tool is returning the identical ready-to-inject
// shape that hook already produces automatically, not a fresh format
// only coincidentally similar to it.
func formatObservationContext(matches []store.VectorMatch) string {
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

// formatSessionStartContext mirrors cmd/claude-mem-go/context.go's
// formatContext exactly, byte for byte — duplicated rather than imported,
// the same import-cycle constraint formatObservationContext's own doc
// comment explains (mcpserver can't import package main, which imports
// mcpserver for cmdMCP). The whole point of session_start_context is
// returning the identical text the real SessionStart hook injects, not a
// fresh format only coincidentally similar to it.
func formatSessionStartContext(recent []store.SearchResult) string {
	var b strings.Builder
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
