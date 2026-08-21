package mcpserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"claude-mem-go/embed"
	"claude-mem-go/store"
)

// testEmbedModel skips (not fails) a test when Ollama isn't reachable —
// this project's CI never runs it (see README's Testing section), and a
// missing local dependency should skip cleanly rather than break `go test
// ./...` for someone who hasn't started it, the same pattern
// postgres_test.go uses for a missing Postgres container.
func testEmbedModel(t *testing.T) string {
	t.Helper()
	const model = "nomic-embed-text"
	if err := embed.NewClient(model).Ping(); err != nil {
		t.Skipf("Ollama not reachable with model %q: %v", model, err)
	}
	return model
}

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
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatal("initialize response missing serverInfo")
	}
	// Regression test for a real staleness bug: serverInfo.version was
	// hardcoded to the literal "0.1.0" and never updated across several
	// real version bumps since. serverVersion() now derives it from Go's
	// own VCS build info instead, so this only asserts it's non-empty and
	// not that specific stale literal — the exact value legitimately
	// varies by build environment (a `go test` binary's vcs.revision
	// reflects the actual working tree, not a fixed string).
	if serverInfo["version"] == "" {
		t.Error("serverInfo.version is empty")
	}
	if serverInfo["version"] == "0.1.0" {
		t.Error("serverInfo.version is the old hardcoded literal \"0.1.0\" — it should be derived from the real build instead")
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
	if !found["recent_observations"] {
		t.Errorf("tools/list missing recent_observations, got %v", names)
	}
	if !found["session_observations"] {
		t.Errorf("tools/list missing session_observations, got %v", names)
	}
	if !found["file_observations"] {
		t.Errorf("tools/list missing file_observations, got %v", names)
	}
	if !found["add_observation"] {
		t.Errorf("tools/list missing add_observation, got %v", names)
	}
	if !found["get_observations"] {
		t.Errorf("tools/list missing get_observations, got %v", names)
	}
	if !found["timeline"] {
		t.Errorf("tools/list missing timeline, got %v", names)
	}
	if !found["important_workflow"] {
		t.Errorf("tools/list missing important_workflow, got %v", names)
	}
	if !found["session_start_context"] {
		t.Errorf("tools/list missing session_start_context, got %v", names)
	}
}

// TestToolsCallImportantWorkflowReturnsTheStaticGuidance is the
// regression test for important_workflow — matches real claude-mem's own
// tool of the same name and shape: a zero-dependency, static-text tool
// teaching the search -> timeline -> get_observations pattern, not
// something that looks anything up. No store seeding needed (the whole
// point is it never touches s.st), so this can assert the exact returned
// text directly rather than just "no error."
func TestToolsCallImportantWorkflowReturnsTheStaticGuidance(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"important_workflow","arguments":{}}}`,
	})
	got := toolCallText(t, resp[0])
	for _, want := range []string{"search_observations", "timeline", "get_observations", "3-Layer Pattern"} {
		if !strings.Contains(got, want) {
			t.Errorf("important_workflow output missing %q, got:\n%s", want, got)
		}
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

// TestToolsCallSearchObservationsFiltersByType confirms the "type"
// argument reaches Backend.Search and actually narrows results, at the
// real MCP protocol boundary — not just that store.Search's own filter
// works (already covered by a dedicated store-level test).
func TestToolsCallSearchObservationsFiltersByType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	discovery, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "a", "1"), store.Observation{Type: "discovery", Title: "sprocket rollout"}, 0)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	decision, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "b", "2"), store.Observation{Type: "decision", Title: "sprocket rollout plan approved"}, 0)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_observations","arguments":{"query":"sprocket","type":"decision"}}}`,
	})
	text := toolCallText(t, resp[0])
	// ID-based, not title-based: "sprocket rollout" is itself a prefix of
	// "sprocket rollout plan approved," so a substring check on the title
	// text alone can't distinguish which row(s) actually came back.
	if !strings.Contains(text, fmt.Sprintf("[%d]", decision.ID)) {
		t.Fatalf("search_observations(type=decision) = %q, want the decision row [%d] present", text, decision.ID)
	}
	if strings.Contains(text, fmt.Sprintf("[%d]", discovery.ID)) {
		t.Fatalf("search_observations(type=decision) = %q, want the discovery row [%d] excluded", text, discovery.ID)
	}
}

// TestToolsCallSearchObservationsOffsetSkipsLeadingResults is the
// regression test for a real gap at the MCP protocol boundary: offset
// reaching store.Search correctly (already covered by a dedicated
// store-level test) doesn't by itself prove the MCP argument wiring
// works — "offset" has to actually be read from params.Arguments,
// clamped, and threaded through runSearch. Seeds two rows and confirms
// offset=1 excludes the first-ranked one, matching the un-offset call's
// own top result.
func TestToolsCallSearchObservationsOffsetSkipsLeadingResults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "a", "1"), store.Observation{Type: "discovery", Title: "gizmo rollout phase one"}, 0)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	second, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "b", "2"), store.Observation{Type: "discovery", Title: "gizmo rollout phase two"}, 0)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	unoffset := toolCallText(t, runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_observations","arguments":{"query":"gizmo","limit":1}}}`,
	})[0])
	offsetResp := toolCallText(t, runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_observations","arguments":{"query":"gizmo","limit":1,"offset":1}}}`,
	})[0])

	if unoffset == offsetResp {
		t.Fatalf("offset=1 returned the identical result as offset=0: %q — offset argument isn't reaching Search", unoffset)
	}
	// Whichever row ranked first without an offset must be the one excluded
	// once offset=1 skips it — the other seeded row must appear instead.
	var skipped, expected int64
	if strings.Contains(unoffset, fmt.Sprintf("[%d]", first.ID)) {
		skipped, expected = first.ID, second.ID
	} else {
		skipped, expected = second.ID, first.ID
	}
	if strings.Contains(offsetResp, fmt.Sprintf("[%d]", skipped)) {
		t.Fatalf("offset=1 result = %q, want the first-ranked row [%d] excluded", offsetResp, skipped)
	}
	if !strings.Contains(offsetResp, fmt.Sprintf("[%d]", expected)) {
		t.Fatalf("offset=1 result = %q, want the second-ranked row [%d] present", offsetResp, expected)
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

// toolCallText extracts the single text content block from a tools/call
// JSON-RPC response — the shape every one of this server's tool handlers
// returns on success.
func toolCallText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %v", resp)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tool call reported isError: %v", result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("got %d content blocks, want 1: %v", len(content), result)
	}
	return content[0].(map[string]any)["text"].(string)
}

func toolCallIsError(t *testing.T, resp map[string]any) bool {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %v", resp)
	}
	isErr, _ := result["isError"].(bool)
	return isErr
}

// TestToolsCallRecentObservationsScopesToServerProjectOrOverride locks in
// both recent_observations behaviors: it uses the server's current project
// by default (the same "recent" read path SessionStart's context injection
// already relies on, now reachable on demand), and an explicit "project"
// argument overrides that.
func TestToolsCallRecentObservationsScopesToServerProjectOrOverride(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Insert("s1", "proj-a", "Bash", store.ContentHash("s1", "Bash", "a", "1"),
		store.Observation{Type: "discovery", Title: "recent in project A"}, 0); err != nil {
		t.Fatalf("seed proj-a: %v", err)
	}
	if _, err := st.Insert("s1", "proj-b", "Bash", store.ContentHash("s1", "Bash", "b", "2"),
		store.Observation{Type: "discovery", Title: "recent in project B"}, 0); err != nil {
		t.Fatalf("seed proj-b: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recent_observations","arguments":{}}}`,
	})
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "project A") || strings.Contains(text, "project B") {
		t.Fatalf("recent_observations defaulted to the server project incorrectly: %q", text)
	}

	s2 := &Server{DBPath: dbPath, Project: "proj-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp2 := runLines(t, s2, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recent_observations","arguments":{"project":"proj-b"}}}`,
	})
	text2 := toolCallText(t, resp2[0])
	if !strings.Contains(text2, "project B") || strings.Contains(text2, "project A") {
		t.Fatalf("recent_observations with an explicit project override did not switch projects: %q", text2)
	}
}

// TestToolsCallRecentObservationsWithNoProjectIsAnError confirms the
// failure mode is a clear tool error, not a silent empty result that
// looks identical to "this project has no observations yet."
func TestToolsCallRecentObservationsWithNoProjectIsAnError(t *testing.T) {
	s, _ := newTestServer(t) // Project left unset
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recent_observations","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("recent_observations with no server project and no override: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallSessionObservationsReturnsOnlyThatSessionInOrder is the MCP
// surface for the exact read path the Stop hook's session summary uses
// (BySessionID) — oldest first, and scoped to one session, not a project.
func TestToolsCallSessionObservationsReturnsOnlyThatSessionInOrder(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Insert("session-x", "proj", "Bash", store.ContentHash("session-x", "Bash", "a", "1"),
		store.Observation{Type: "discovery", Title: "first thing in session x"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.Insert("session-x", "proj", "Bash", store.ContentHash("session-x", "Bash", "b", "2"),
		store.Observation{Type: "discovery", Title: "second thing in session x"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.Insert("session-y", "proj", "Bash", store.ContentHash("session-y", "Bash", "c", "3"),
		store.Observation{Type: "discovery", Title: "something in session y"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_observations","arguments":{"session_id":"session-x"}}}`,
	})
	text := toolCallText(t, resp[0])
	if strings.Contains(text, "session y") {
		t.Fatalf("session_observations for session-x leaked a session-y row: %q", text)
	}
	firstIdx := strings.Index(text, "first thing")
	secondIdx := strings.Index(text, "second thing")
	if firstIdx == -1 || secondIdx == -1 || firstIdx > secondIdx {
		t.Fatalf("session_observations not oldest-first: %q", text)
	}
}

func TestToolsCallSessionObservationsWithNoSessionIDIsAnError(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_observations","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("session_observations with no session_id: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallFileObservationsFindsMentionsScopedToProject is the on-
// demand MCP surface for the same lookup the PreToolUse file-context hook
// already runs automatically (ObservationsForFile) — exact match, scoped
// to a project, with the same server-project-or-override rule as
// recent_observations.
func TestToolsCallFileObservationsFindsMentionsScopedToProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Insert("s1", "proj-a", "Read", store.ContentHash("s1", "Read", "a", "1"),
		store.Observation{Type: "discovery", Title: "read main.go in A", FilesRead: []string{"main.go"}}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.Insert("s1", "proj-b", "Read", store.ContentHash("s1", "Read", "b", "2"),
		store.Observation{Type: "discovery", Title: "read main.go in B", FilesRead: []string{"main.go"}}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"file_observations","arguments":{"file_path":"main.go"}}}`,
	})
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "in A") || strings.Contains(text, "in B") {
		t.Fatalf("file_observations did not scope to the server project: %q", text)
	}
}

func TestToolsCallFileObservationsWithNoFilePathIsAnError(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"file_observations","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("file_observations with no file_path: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallGetObservationsReturnsFullDetailAndScopesToProject covers
// both of get_observations' real contracts at once: it must surface fields
// (narrative, facts) the list-shaped tools deliberately omit, and it must
// not leak another project's row just because the caller happened to guess
// its ID — the same cross-project scoping search_observations/
// semantic_search_observations were fixed for earlier this project.
func TestToolsCallGetObservationsReturnsFullDetailAndScopesToProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ra, err := st.Insert("s1", "proj-a", "Bash", store.ContentHash("s1", "Bash", "a", "1"),
		store.Observation{Type: "discovery", Title: "in A", Narrative: "the full story", Facts: []string{"fact one"}}, 0)
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}
	rb, err := st.Insert("s1", "proj-b", "Bash", store.ContentHash("s1", "Bash", "b", "2"),
		store.Observation{Type: "discovery", Title: "in B", Narrative: "someone else's story"}, 0)
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_observations","arguments":{"ids":[%d,%d]}}}`, ra.ID, rb.ID),
	})
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "the full story") || !strings.Contains(text, "fact one") {
		t.Fatalf("get_observations did not surface narrative/facts: %q", text)
	}
	if strings.Contains(text, "someone else's story") {
		t.Fatalf("get_observations leaked proj-b's row across the project boundary: %q", text)
	}
}

func TestToolsCallGetObservationsRequiresNonEmptyIDs(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_observations","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("get_observations with no ids: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallGetObservationsWithTooManyIDsIsACleanToolError is the
// real-driver-limit regression test at the MCP protocol boundary: a
// caller sending more IDs than store.MaxIDsPerLookup must get back a
// normal JSON-RPC tool-error result (isError=true, a readable message),
// not a raw SQL driver error leaking through or the server crashing —
// confirmed against the real store.Store.ByIDs error path, not a mock.
func TestToolsCallGetObservationsWithTooManyIDsIsACleanToolError(t *testing.T) {
	s, _ := newTestServer(t)

	ids := make([]string, store.MaxIDsPerLookup+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%d", i+1)
	}
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_observations","arguments":{"ids":[%s]}}}`, strings.Join(ids, ","))

	resp := runLines(t, s, []string{req})
	if len(resp) != 1 {
		t.Fatalf("got %d responses, want 1", len(resp))
	}
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("get_observations with %d ids (limit %d): want isError=true, got %v", len(ids), store.MaxIDsPerLookup, resp[0])
	}
	// Not toolCallText — that helper asserts isError is false, since every
	// other caller uses it only on the success path. Read the error
	// content directly instead.
	result := resp[0]["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	text := content["text"].(string)
	if !strings.Contains(text, "exceeds") {
		t.Errorf("error text = %q, want it to mention the limit being exceeded, not a raw driver error", text)
	}
}

// TestToolsCallTimelineWithDirectAnchorReturnsSurroundingContext seeds a
// known sequence and confirms timeline returns the anchor plus its real
// neighbors, marked so the anchor is identifiable in the output — the
// real "get context around one result" gap this tool closes, distinct
// from recent_observations (which answers "what's recent," not "what
// surrounds this specific one").
func TestToolsCallTimelineWithDirectAnchorReturnsSurroundingContext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var anchorID int64
	for _, title := range []string{"first", "second", "third", "fourth", "fifth"} {
		res, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", title, "x"),
			store.Observation{Type: "discovery", Title: title}, 0)
		if err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		if title == "third" {
			anchorID = res.ID
		}
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"timeline","arguments":{"anchor":%d,"depth_before":1,"depth_after":1}}}`, anchorID),
	})
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "second") || !strings.Contains(text, "third") || !strings.Contains(text, "fourth") {
		t.Fatalf("timeline around \"third\" = %q, want second/third/fourth present", text)
	}
	if strings.Contains(text, "first") || strings.Contains(text, "fifth") {
		t.Fatalf("timeline with depth_before=1/depth_after=1 = %q, want first/fifth excluded (too far from the anchor)", text)
	}
	if !strings.Contains(text, "→") {
		t.Errorf("timeline output = %q, want the anchor row marked with →", text)
	}
}

// TestToolsCallTimelineResolvesAnchorFromQuery confirms the "no anchor
// given, find one via query" convenience path real claude-mem's own
// timeline tool offers the identical way.
func TestToolsCallTimelineResolvesAnchorFromQuery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "a", "1"),
		store.Observation{Type: "discovery", Title: "unrelated observation about kites"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.Insert("s1", "proj", "Bash", store.ContentHash("s1", "Bash", "b", "2"),
		store.Observation{Type: "discovery", Title: "rate limiting middleware added"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"timeline","arguments":{"query":"rate limiting"}}}`,
	})
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "rate limiting middleware added") {
		t.Fatalf("timeline resolved via query = %q, want the matched observation present as the anchor", text)
	}
}

func TestToolsCallTimelineRequiresAnchorOrQuery(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"timeline","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("timeline with neither anchor nor query: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallAddObservationPersistsAndIsFindable is add_observation's
// core contract: the write actually lands, scoped to the server's current
// project, and is findable through the existing read tools afterward —
// the same store, not a side channel.
func TestToolsCallAddObservationPersistsAndIsFindable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"decided to use Postgres for scale","narrative":"team agreed local SQLite wasn't enough","facts":["fact one"],"concepts":["architecture"]}}}`,
	})
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "Remembered") {
		t.Fatalf("add_observation response = %q, want it to confirm the observation was remembered", text)
	}

	s2 := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp2 := runLines(t, s2, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recent_observations","arguments":{}}}`,
	})
	recentText := toolCallText(t, resp2[0])
	if !strings.Contains(recentText, "decided to use Postgres for scale") {
		t.Fatalf("recent_observations after add_observation = %q, want the manually-added row to show up", recentText)
	}
}

// TestToolsCallAddObservationIsIdempotentWithinASession confirms calling
// add_observation twice with the same title/narrative in the same server
// process (same SessionID) is a no-op the second time, not a duplicate —
// the same content-hash idempotency automatic capture already relies on.
// TestToolsCallAddObservationIsSemanticallySearchable is the regression
// test for a real gap in add_observation's first version: it inserted the
// row but never embedded it, unlike automatic capture (worker.process),
// which always does when an embed model is configured. That made a
// manually-added observation a second-class citizen — findable by
// keyword search, invisible to semantic search. Uses a real Ollama call,
// skipping cleanly if it isn't reachable (see testEmbedModel).
func TestToolsCallAddObservationIsSemanticallySearchable(t *testing.T) {
	model := testEmbedModel(t)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", EmbedModel: model, Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"switched the database to Postgres for production scale","narrative":"local SQLite could not keep up with concurrent writes"}}}`,
	})
	text := toolCallText(t, resp[0])
	if strings.Contains(text, "embedding failed") {
		t.Fatalf("add_observation reported an embedding failure even though Ollama is reachable: %q", text)
	}

	s2 := &Server{DBPath: dbPath, Project: "proj", EmbedModel: model, Log: log.New(&bytes.Buffer{}, "", 0)}
	resp2 := runLines(t, s2, []string{
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"semantic_search_observations","arguments":{"query":"why did we move off of sqlite"}}}`,
	})
	semanticText := toolCallText(t, resp2[0])
	if !strings.Contains(semanticText, "Postgres for production scale") {
		t.Fatalf("semantic_search_observations after add_observation = %q, want the manually-added row to be found by meaning, not just keywords", semanticText)
	}
}

func TestToolsCallAddObservationIsIdempotentWithinASession(t *testing.T) {
	s, dbPath := newTestServer(t)
	s.Project = "proj"

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"remember this exact thing","narrative":"same every time"}}}`
	resp := runLines(t, s, []string{call})
	if toolCallIsError(t, resp[0]) {
		t.Fatalf("first add_observation call: want success, got %v", resp[0])
	}

	s2 := &Server{DBPath: dbPath, Project: "proj", SessionID: s.SessionID, Log: log.New(&bytes.Buffer{}, "", 0)}
	resp2 := runLines(t, s2, []string{call})
	text2 := toolCallText(t, resp2[0])
	if !strings.Contains(text2, "Already remembered") {
		t.Fatalf("second identical add_observation call in the same session = %q, want it recognized as already remembered", text2)
	}
}

func TestToolsCallAddObservationRequiresTitle(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("add_observation with no title: want isError=true, got %v", resp[0])
	}
}

func TestToolsCallAddObservationRequiresAProject(t *testing.T) {
	s, _ := newTestServer(t) // Project left unset
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"something"}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("add_observation with no server project: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallAddObservationRejectsOversizedTitle is the regression test
// for a real gap add_observation had until now: title/subtitle/narrative/
// facts/concepts arrived from MCP tool-call arguments with no length check
// at all, unlike every other external-input surface in this codebase
// (hook.MaxPayloadBytes bounds the hook socket, store.MaxIDsPerLookup
// bounds get_observations' id list). A caller sending a title far past
// maxObservationTitleBytes must get a clean isError result naming the
// limit, not a row silently accepted and stored forever.
func TestToolsCallAddObservationRejectsOversizedTitle(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	oversized := strings.Repeat("a", maxObservationTitleBytes+1)
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"%s"}}}`, oversized)

	resp := runLines(t, s, []string{req})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("add_observation with a %d-byte title (limit %d): want isError=true, got %v", len(oversized), maxObservationTitleBytes, resp[0])
	}
	result := resp[0]["result"].(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "exceeds") {
		t.Errorf("error text = %q, want it to mention the limit being exceeded", text)
	}
}

// TestToolsCallAddObservationRejectsOversizedNarrative confirms the same
// bound applies to narrative, the field that also feeds
// embed.ObservationText's embedding request — an unbounded narrative
// wouldn't just bloat the stored row, it would ship arbitrarily large text
// into a single Ollama call on every add_observation.
func TestToolsCallAddObservationRejectsOversizedNarrative(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	oversized := strings.Repeat("b", maxObservationNarrativeBytes+1)
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"ok","narrative":"%s"}}}`, oversized)

	resp := runLines(t, s, []string{req})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("add_observation with a %d-byte narrative (limit %d): want isError=true, got %v", len(oversized), maxObservationNarrativeBytes, resp[0])
	}
}

// TestToolsCallAddObservationRejectsTooManyFacts confirms the facts array
// is bounded by count, not just by each item's own length — matching the
// "no legitimate caller needs more than a page" reasoning store.
// MaxIDsPerLookup already applies to get_observations' id list.
func TestToolsCallAddObservationRejectsTooManyFacts(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	facts := make([]string, maxObservationFactsCount+1)
	for i := range facts {
		facts[i] = fmt.Sprintf("%q", fmt.Sprintf("fact %d", i))
	}
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"ok","facts":[%s]}}}`, strings.Join(facts, ","))

	resp := runLines(t, s, []string{req})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("add_observation with %d facts (limit %d): want isError=true, got %v", len(facts), maxObservationFactsCount, resp[0])
	}
}

// TestToolsCallAddObservationRejectsOversizedFactItem confirms each fact
// is also bounded individually — a caller under the count limit could
// otherwise still smuggle an arbitrarily large blob into a single "fact".
func TestToolsCallAddObservationRejectsOversizedFactItem(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	oversized := strings.Repeat("c", maxObservationFactBytes+1)
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"ok","facts":["%s"]}}}`, oversized)

	resp := runLines(t, s, []string{req})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("add_observation with a %d-byte fact (limit %d): want isError=true, got %v", len(oversized), maxObservationFactBytes, resp[0])
	}
}

// TestToolsCallAddObservationAcceptsFieldsWithinBounds is the
// non-regression counterpart: normal-sized title/subtitle/narrative/
// facts/concepts, well within every bound above, must still succeed —
// proving this is a real bound, not an accidental block on legitimate
// calls (the same shape TestToolsCallAddObservationPersistsAndIsFindable
// already covers, asserted again here explicitly against the bounds).
func TestToolsCallAddObservationAcceptsFieldsWithinBounds(t *testing.T) {
	s, _ := newTestServer(t)
	s.Project = "proj"
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_observation","arguments":{"title":"a normal title","subtitle":"a normal subtitle","narrative":"a normal, reasonably detailed narrative paragraph","facts":["fact one","fact two"],"concepts":["architecture"]}}}`

	resp := runLines(t, s, []string{req})
	if toolCallIsError(t, resp[0]) {
		t.Fatalf("add_observation with normal-sized fields: want success, got error %v", resp[0])
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

// TestToolsCallObservationContextReturnsThePromptContextHookFormat is the
// direct regression test for the whole point of this tool: it must return
// the SAME ready-to-inject text cmd/claude-mem-go/prompt_context.go's
// UserPromptSubmit hook produces automatically for the same query — not a
// list of results like semantic_search_observations returns for a caller
// to interpret. Seeds a real embedded observation (a real Ollama call, not
// mocked), queries by meaning rather than keyword overlap the same way
// TestToolsCallAddObservationIsSemanticallySearchable does, and checks the
// response against the exact header and "- Title — Subtitle" line shape
// formatPromptContext produces, so a future divergence between the two
// formatters (this one is a duplicate, not a shared import — see
// formatObservationContext's own doc comment for why) would be caught here
// rather than silently drifting.
func TestToolsCallObservationContextReturnsThePromptContextHookFormat(t *testing.T) {
	model := testEmbedModel(t)

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	o := store.Observation{Type: "decision", Title: "switched the database to Postgres for production scale", Subtitle: "local SQLite could not keep up with concurrent writes"}
	res, err := st.Insert("s1", "proj", "manual", store.ContentHash("s1", "manual", "a", "b"), o, 0)
	if err != nil {
		t.Fatalf("seed Insert: %v", err)
	}
	text := embed.ObservationText(o.Title, o.Subtitle, o.Narrative, o.Facts)
	vec, err := embed.NewClient(model).Embed(text)
	if err != nil {
		t.Fatalf("seed Embed: %v", err)
	}
	if err := st.SaveEmbedding(res.ID, vec); err != nil {
		t.Fatalf("seed SaveEmbedding: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", EmbedModel: model, Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"observation_context","arguments":{"query":"why did we move off of sqlite"}}}`,
	})
	got := toolCallText(t, resp[0])

	want := "Memory relevant to what you just asked:\n\n" +
		"- switched the database to Postgres for production scale — local SQLite could not keep up with concurrent writes"
	if got != want {
		t.Fatalf("observation_context =\n%q\nwant exactly (matching formatPromptContext's own shape):\n%q", got, want)
	}
}

// TestToolsCallObservationContextRequiresAQuery matches every other
// required-argument check in this file (e.g.
// TestToolsCallAddObservationRequiresTitle) — no live Ollama call needed
// since this must fail before ever reaching the embedding step.
func TestToolsCallObservationContextRequiresAQuery(t *testing.T) {
	s, _ := newTestServer(t)
	s.EmbedModel = "nomic-embed-text"
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"observation_context","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("observation_context with no query: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallSessionStartContextReturnsTheContextHookFormat is
// session_start_context's counterpart to
// TestToolsCallObservationContextReturnsThePromptContextHookFormat: locks
// in that this tool's output is byte-for-byte identical to what
// cmd/claude-mem-go/context.go's formatContext produces for the real
// SessionStart hook, not merely similar to it — formatSessionStartContext
// is a duplicate, not a shared import (see its own doc comment for why),
// so a future divergence between the two formatters would be caught here
// rather than silently drifting.
func TestToolsCallSessionStartContextReturnsTheContextHookFormat(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	o := store.Observation{Type: "decision", Title: "switched the database to Postgres for production scale", Subtitle: "local SQLite could not keep up with concurrent writes"}
	if _, err := st.Insert("s1", "proj", "manual", store.ContentHash("s1", "manual", "a", "b"), o, 0); err != nil {
		t.Fatalf("seed Insert: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start_context","arguments":{}}}`,
	})
	got := toolCallText(t, resp[0])

	want := "Relevant memory from previous sessions in this project:\n\n" +
		"- switched the database to Postgres for production scale — local SQLite could not keep up with concurrent writes"
	if got != want {
		t.Fatalf("session_start_context =\n%q\nwant exactly (matching formatContext's own shape):\n%q", got, want)
	}
}

// TestToolsCallSessionStartContextDefaultLimitMatchesRealHook confirms
// this tool's own default limit is 5 — cmd/claude-mem-go/context.go's
// real SessionStart hook default — not the 10 every other list-shaped
// tool here defaults to, since returning what the real hook would
// actually inject is the whole point.
func TestToolsCallSessionStartContextDefaultLimitMatchesRealHook(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 7; i++ {
		if _, err := st.Insert("s1", "proj", "Bash",
			store.ContentHash("s1", "Bash", fmt.Sprintf("cmd-%d", i), "out"),
			store.Observation{Type: "discovery", Title: fmt.Sprintf("observation %d", i)}, 0); err != nil {
			t.Fatalf("seed Insert %d: %v", i, err)
		}
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start_context","arguments":{}}}`,
	})
	text := toolCallText(t, resp[0])
	if got := strings.Count(text, "- observation "); got != 5 {
		t.Fatalf("session_start_context with no explicit limit returned %d observations, want 5 (real hook default), got:\n%s", got, text)
	}
}

// TestToolsCallSessionStartContextScopesToServerProjectOrOverride mirrors
// TestToolsCallRecentObservationsScopesToServerProjectOrOverride.
func TestToolsCallSessionStartContextScopesToServerProjectOrOverride(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.Insert("s1", "proj-a", "Bash", store.ContentHash("s1", "Bash", "a", "1"),
		store.Observation{Type: "discovery", Title: "recent in project A"}, 0); err != nil {
		t.Fatalf("seed proj-a: %v", err)
	}
	if _, err := st.Insert("s1", "proj-b", "Bash", store.ContentHash("s1", "Bash", "b", "2"),
		store.Observation{Type: "discovery", Title: "recent in project B"}, 0); err != nil {
		t.Fatalf("seed proj-b: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "proj-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start_context","arguments":{}}}`,
	})
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "project A") || strings.Contains(text, "project B") {
		t.Fatalf("session_start_context defaulted to the server project incorrectly: %q", text)
	}

	s2 := &Server{DBPath: dbPath, Project: "proj-a", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp2 := runLines(t, s2, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start_context","arguments":{"project":"proj-b"}}}`,
	})
	text2 := toolCallText(t, resp2[0])
	if !strings.Contains(text2, "project B") || strings.Contains(text2, "project A") {
		t.Fatalf("session_start_context with an explicit project override did not switch projects: %q", text2)
	}
}

// TestToolsCallSessionStartContextWithNoProjectIsAnError mirrors
// TestToolsCallRecentObservationsWithNoProjectIsAnError.
func TestToolsCallSessionStartContextWithNoProjectIsAnError(t *testing.T) {
	s, _ := newTestServer(t) // Project left unset
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start_context","arguments":{}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("session_start_context with no server project and no override: want isError=true, got %v", resp[0])
	}
}

// TestToolsCallSessionStartContextEmptyProjectSaysSo confirms the
// zero-observations case is a clear, distinct message rather than an
// empty tool response — the real hook would inject nothing at all here,
// but an MCP tool call still needs to say that explicitly.
func TestToolsCallSessionStartContextEmptyProjectSaysSo(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()

	s := &Server{DBPath: dbPath, Project: "empty-proj", Log: log.New(&bytes.Buffer{}, "", 0)}
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start_context","arguments":{}}}`,
	})
	if toolCallIsError(t, resp[0]) {
		t.Fatalf("session_start_context for a project with zero observations: want a normal (non-error) informational result, got %v", resp[0])
	}
	text := toolCallText(t, resp[0])
	if !strings.Contains(text, "No prior observations") {
		t.Fatalf("session_start_context for an empty project = %q, want a clear no-observations message", text)
	}
}

// TestToolsCallObservationContextDisabledWithoutEmbedModel confirms this
// tool degrades the same explicit way semantic_search_observations does
// when the server has no embed model configured, rather than a nil-Ollama
// panic or a confusing unrelated error — newTestServer's Server leaves
// EmbedModel at its zero value.
func TestToolsCallObservationContextDisabledWithoutEmbedModel(t *testing.T) {
	s, _ := newTestServer(t)
	resp := runLines(t, s, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"observation_context","arguments":{"query":"anything"}}}`,
	})
	if !toolCallIsError(t, resp[0]) {
		t.Fatalf("observation_context with no embed model configured: want isError=true, got %v", resp[0])
	}
}
