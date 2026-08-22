package mcpserver

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// panickingBackend implements memory.Backend with every method panicking
// except the ones needed to get a tools/call request to Search — a
// minimal fault-injection double, not a real store, purpose-built to
// prove Server.handle's recover() actually stops a panic from escaping
// rather than crashing the process.
type panickingBackend struct{}

func (panickingBackend) Insert(context.Context, string, string, string, string, memory.Observation, float64) (memory.InsertResult, error) {
	panic("panickingBackend: Insert")
}
func (panickingBackend) CountByProject(context.Context, string) (int, error) {
	panic("panickingBackend: CountByProject")
}
func (panickingBackend) Search(context.Context, string, string, string, int, int, int64, int64, string) ([]memory.SearchResult, error) {
	panic("panickingBackend: Search")
}
func (panickingBackend) SaveEmbedding(context.Context, int64, []float32) error {
	panic("panickingBackend: SaveEmbedding")
}
func (panickingBackend) SemanticSearch(context.Context, string, []float32, int) ([]memory.VectorMatch, error) {
	panic("panickingBackend: SemanticSearch")
}
func (panickingBackend) RecentByProject(context.Context, string, int) ([]memory.SearchResult, error) {
	panic("panickingBackend: RecentByProject")
}
func (panickingBackend) BySessionID(context.Context, string, string, int) ([]memory.SearchResult, error) {
	panic("panickingBackend: BySessionID")
}
func (panickingBackend) ObservationsForFile(context.Context, string, string, int) ([]memory.SearchResult, error) {
	panic("panickingBackend: ObservationsForFile")
}
func (panickingBackend) ByIDs(context.Context, []int64) ([]memory.SearchResult, error) {
	panic("panickingBackend: ByIDs")
}
func (panickingBackend) Timeline(context.Context, string, int64, int, int) ([]memory.SearchResult, error) {
	panic("panickingBackend: Timeline")
}
func (panickingBackend) ObservationsNeedingEmbedding(context.Context, string, int64, int64, int) ([]memory.SearchResult, error) {
	panic("panickingBackend: ObservationsNeedingEmbedding")
}
func (panickingBackend) Prune(context.Context, string, int64, bool) (int64, error) {
	panic("panickingBackend: Prune")
}
func (panickingBackend) RepairFilePaths(context.Context, string, bool) (int64, error) {
	panic("panickingBackend: RepairFilePaths")
}
func (panickingBackend) ExportAll(context.Context, int64, int) ([]memory.ExportRow, error) {
	panic("panickingBackend: ExportAll")
}
func (panickingBackend) ImportRow(context.Context, memory.ExportRow) (memory.InsertResult, error) {
	panic("panickingBackend: ImportRow")
}
func (panickingBackend) Stats(ctx context.Context) (memory.StoreStats, error) { panic("boom") }
func (panickingBackend) InsertPrompt(context.Context, string, string, string) (int64, error) {
	panic("panickingBackend: InsertPrompt")
}
func (panickingBackend) SearchPrompts(context.Context, string, string, int, int) ([]memory.PromptResult, error) {
	panic("panickingBackend: SearchPrompts")
}
func (panickingBackend) PromptsBySession(context.Context, string, string, int) ([]memory.PromptResult, error) {
	panic("panickingBackend: PromptsBySession")
}
func (panickingBackend) ExportPrompts(context.Context, int64, int) ([]memory.PromptRow, error) {
	panic("panickingBackend: ExportPrompts")
}
func (panickingBackend) ImportPrompt(context.Context, memory.PromptRow) (bool, error) {
	panic("panickingBackend: ImportPrompt")
}

func (panickingBackend) HealthDetails(ctx context.Context) (map[string]string, error) {
	panic("panickingBackend: HealthDetails")
}
func (panickingBackend) Close() error { panic("panickingBackend: Close") }

var _ memory.Backend = panickingBackend{}

// TestHandlePanicRecoverySurvivesAndReturnsCleanError is the regression
// test for a real, severe gap found this iteration: neither this server
// nor the worker daemon had ANY panic recovery, anywhere. A single panic
// in a Backend method (a real, reproducible one existed in
// SemanticSearch's own limit handling before this iteration fixed it)
// would have crashed the entire process — this MCP server's whole stdio
// session, since handle runs synchronously in Run's scanner loop, not a
// spawned goroutine. Proves the recover() backstop itself works,
// independent of any specific bug: a fake Backend that panics on every
// call, exercised through the real handle()/handleToolCall() path,
// confirms the process survives and a real client gets a clean
// isError-shaped JSON-RPC error instead of the connection just dying.
func TestHandlePanicRecoverySurvivesAndReturnsCleanError(t *testing.T) {
	var logBuf bytes.Buffer
	s := &Server{Project: "proj", Log: logging.New(&logBuf, "", 0)}
	s.st = panickingBackend{} // bypass Run()/backend.Open — this is a fault-injection double, not a real store

	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      []byte("1"),
		Method:  "tools/call",
		Params:  []byte(`{"name":"search_observations","arguments":{"query":"anything"}}`),
	}

	var resp *rpcResponse
	done := make(chan struct{})
	func() {
		defer close(done)
		resp = s.handle(req)
	}()
	<-done // reaching this line at all proves handle() did not crash the process

	if resp == nil {
		t.Fatal("handle() returned nil after a panic — want a real JSON-RPC error response")
	}
	if resp.Error == nil {
		t.Fatalf("handle() response after a panic = %+v, want a JSON-RPC error, not a normal result", resp)
	}
	if !strings.Contains(resp.Error.Message, "internal error") {
		t.Errorf("error message = %q, want it to mention an internal error", resp.Error.Message)
	}
	if !strings.Contains(logBuf.String(), "PANIC recovered") {
		t.Errorf("log output = %q, want a PANIC recovered line", logBuf.String())
	}
}
