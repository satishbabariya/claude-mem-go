package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"claude-mem-go/store"
)

// newTestServer opens a fresh sqlite db seeded with one observation, so
// tests exercise real store.Search rather than a mock.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	o := store.Observation{Type: "discovery", Title: "claude-mem-go MCP server implemented", Subtitle: "stdio JSON-RPC"}
	if _, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "a", "b"), o, 0); err != nil {
		t.Fatalf("seed Insert: %v", err)
	}
	st.Close() // Server.Run opens its own handle

	return &Server{DBPath: dbPath, Log: log.New(&bytes.Buffer{}, "", 0)}, dbPath
}

// runLines feeds each line as one JSON-RPC message and returns every
// response line the server wrote back, parsed.
func runLines(t *testing.T, s *Server, lines []string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := s.Run(in, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var responses []map[string]any
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("response line is not valid JSON: %v (%s)", err, line)
		}
		responses = append(responses, m)
	}
	return responses
}

func TestInitializeHandshake(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
	})
	if len(resp) != 1 {
		t.Fatalf("got %d responses, want 1", len(resp))
	}
	result, ok := resp[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize response has no result: %v", resp[0])
	}
	if result["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %v", result["protocolVersion"], protocolVersion)
	}
	if _, ok := result["serverInfo"]; !ok {
		t.Error("initialize response missing serverInfo")
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	s, _ := newTestServer(t)
	// A notification has no "id" field at all — the real client sends
	// notifications/initialized this way after the initialize handshake.
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
	})
	if len(resp) != 0 {
		t.Fatalf("server responded to a notification (%d lines) — MCP forbids this", len(resp))
	}
}

func TestToolsListIncludesSearchTools(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	})
	if len(resp) != 1 {
		t.Fatalf("got %d responses, want 1", len(resp))
	}
	result := resp[0]["result"].(map[string]any)
	toolsList, ok := result["tools"].([]any)
	if !ok || len(toolsList) == 0 {
		t.Fatalf("tools/list returned no tools: %v", result)
	}
	var names []string
	for _, tl := range toolsList {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["search_observations"] {
		t.Errorf("tools/list missing search_observations, got %v", names)
	}
	if !found["semantic_search_observations"] {
		t.Errorf("tools/list missing semantic_search_observations, got %v", names)
	}
}

func TestToolsCallSearchObservationsFindsSeededRow(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_observations","arguments":{"query":"claude-mem-go"}}}`,
	})
	if len(resp) != 1 {
		t.Fatalf("got %d responses, want 1", len(resp))
	}
	result := resp[0]["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tool call reported isError: %v", result)
	}
	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("got %d content blocks, want 1", len(content))
	}
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "MCP server implemented") {
		t.Fatalf("search result text = %q, want it to mention the seeded observation", text)
	}
}

// TestToolsCallLimitIsCappedRegardlessOfCallerValue seeds well over 100 rows
// and confirms a caller-supplied limit far above 100 still returns at most
// 100 — the same "max 100" bound real claude-mem's own mem-search skill
// documents, and worth enforcing here since an MCP tool's arguments come
// from whatever's calling the server, not necessarily a careful human.
func TestToolsCallLimitIsCappedRegardlessOfCallerValue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 105; i++ {
		o := store.Observation{Type: "discovery", Title: "bulk seeded observation about widgets"}
		hash := store.ContentHash("s1", "Bash", string(rune('a'+i%26)), string(rune(i)))
		if _, err := st.Insert("s1", "proj", "Bash", hash, o, 0); err != nil {
			t.Fatalf("seed Insert %d: %v", i, err)
		}
	}
	st.Close()

	s := &Server{DBPath: dbPath, Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_observations","arguments":{"query":"widgets","limit":99999}}}`,
	})
	result := resp[0]["result"].(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)

	lines := strings.Count(text, "[")
	if lines > 100 {
		t.Fatalf("search_observations with limit=99999 returned %d results, want capped at 100", lines)
	}
	if lines == 0 {
		t.Fatal("expected at least some results from the 105 seeded rows")
	}
}

func TestToolsCallUnknownToolIsProtocolError(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`,
	})
	if len(resp) != 1 {
		t.Fatalf("got %d responses, want 1", len(resp))
	}
	if _, ok := resp[0]["error"]; !ok {
		t.Fatalf("unknown tool call: want a JSON-RPC error, got %v", resp[0])
	}
}

func TestUnknownMethodIsMethodNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":4,"method":"totally/unsupported"}`,
	})
	if len(resp) != 1 {
		t.Fatalf("got %d responses, want 1", len(resp))
	}
	errObj, ok := resp[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown method: want a JSON-RPC error, got %v", resp[0])
	}
	if code, _ := errObj["code"].(float64); int(code) != -32601 {
		t.Errorf("error code = %v, want -32601 (method not found)", errObj["code"])
	}
}

// TestSearchObservationsScopesToServerProject is the regression test for a
// real cross-project leak: the underlying store is one shared database
// across every project ever recorded on the machine (see
// store.DefaultDBPath), so an MCP client working on project A must not see
// project B's memory just because both happen to share a keyword. Server.
// Project (set from the server process's cwd in cmd's cmdMCP) is what
// enforces that boundary by default.
func TestSearchObservationsScopesToServerProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	oA := store.Observation{Type: "discovery", Title: "widgets pipeline rewritten in project-a"}
	if _, err := st.Insert("s1", "project-a", "Bash", store.ContentHash("s1", "Bash", "a", "1"), oA, 0); err != nil {
		t.Fatalf("seed project-a: %v", err)
	}
	oB := store.Observation{Type: "discovery", Title: "widgets pipeline rewritten in project-b"}
	if _, err := st.Insert("s1", "project-b", "Bash", store.ContentHash("s1", "Bash", "b", "2"), oB, 0); err != nil {
		t.Fatalf("seed project-b: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "project-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_observations","arguments":{"query":"widgets"}}}`,
	})
	text := resp[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "project-a") {
		t.Fatalf("search scoped to project-a found nothing from it: %q", text)
	}
	if strings.Contains(text, "project-b") {
		t.Fatalf("search scoped to project-a leaked a project-b result: %q", text)
	}

	// all_projects:true is the explicit escape hatch — it must see both.
	s2 := &Server{DBPath: dbPath, Project: "project-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp2 := runLines(t, s2, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_observations","arguments":{"query":"widgets","all_projects":true}}}`,
	})
	text2 := resp2[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text2, "project-a") || !strings.Contains(text2, "project-b") {
		t.Fatalf("all_projects:true should see both projects, got: %q", text2)
	}
}

func TestMalformedLineIsSkippedNotFatal(t *testing.T) {
	s, _ := newTestServer(t)
	// One garbage line between two valid requests must not kill the session
	// — a real client could send something this server doesn't expect, and
	// dying on it would take down every subsequent request too.
	resp := runLines(t, s, []string{
		`not json at all`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`,
	})
	if len(resp) != 1 {
		t.Fatalf("got %d responses after a malformed line, want 1 (the valid request should still be answered)", len(resp))
	}
}
