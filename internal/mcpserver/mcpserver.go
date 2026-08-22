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
	"runtime/debug"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"github.com/satishbabariya/claude-mem-go/internal/memory"

	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
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
	Log          *logging.Logger

	st memory.Backend
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
			s.Log.Errorf("FAILED marshaling response: %v", err)
			continue
		}
		if _, err := w.Write(append(out, '\n')); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return scanner.Err()
}

// RequestTimeout bounds one JSON-RPC request's backend work. handle runs
// synchronously in Run's scanner loop, so a single stuck query would
// otherwise block every later request on the session — including
// trivially fast ones like tools/list — with nothing to cut it short.
// Postgres had a statement_timeout as a backstop; SQLite had nothing.
const RequestTimeout = 30 * time.Second

func (s *Server) handle(req rpcRequest) (resp *rpcResponse) {
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	// handle runs synchronously in Run's scanner loop, not a spawned
	// goroutine — an unrecovered panic here doesn't just fail one tool
	// call, it terminates the entire process (Go's default for a panic
	// that unwinds past main), ending the whole MCP session and, if this
	// was mid-write, potentially the whole `claude` session using it.
	// Found not hypothetically: a real, reproducible panic existed in
	// sqlite.Store.SemanticSearch (a negative limit slicing out of
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
		return s.handleToolCall(ctx, req)
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
