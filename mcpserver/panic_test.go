package mcpserver

import (
	"bytes"
	"claude-mem-go/logging"
	"strings"
	"testing"

	"claude-mem-go/store"
)

// panickingBackend implements store.Backend with every method panicking
// except the ones needed to get a tools/call request to Search — a
// minimal fault-injection double, not a real store, purpose-built to
// prove Server.handle's recover() actually stops a panic from escaping
// rather than crashing the process.
type panickingBackend struct{}

func (panickingBackend) Insert(string, string, string, string, store.Observation, float64) (store.InsertResult, error) {
	panic("panickingBackend: Insert")
}
func (panickingBackend) CountByProject(string) (int, error) {
	panic("panickingBackend: CountByProject")
}
func (panickingBackend) Search(string, string, string, int, int, int64, int64, string) ([]store.SearchResult, error) {
	panic("panickingBackend: Search")
}
func (panickingBackend) SaveEmbedding(int64, []float32) error {
	panic("panickingBackend: SaveEmbedding")
}
func (panickingBackend) SemanticSearch(string, []float32, int) ([]store.VectorMatch, error) {
	panic("panickingBackend: SemanticSearch")
}
func (panickingBackend) RecentByProject(string, int) ([]store.SearchResult, error) {
	panic("panickingBackend: RecentByProject")
}
func (panickingBackend) BySessionID(string, int) ([]store.SearchResult, error) {
	panic("panickingBackend: BySessionID")
}
func (panickingBackend) ObservationsForFile(string, string, int) ([]store.SearchResult, error) {
	panic("panickingBackend: ObservationsForFile")
}
func (panickingBackend) ByIDs([]int64) ([]store.SearchResult, error) {
	panic("panickingBackend: ByIDs")
}
func (panickingBackend) Timeline(string, int64, int, int) ([]store.SearchResult, error) {
	panic("panickingBackend: Timeline")
}
func (panickingBackend) ObservationsNeedingEmbedding(string, int64, int64, int) ([]store.SearchResult, error) {
	panic("panickingBackend: ObservationsNeedingEmbedding")
}
func (panickingBackend) Prune(string, int64, bool) (int64, error) { panic("panickingBackend: Prune") }
func (panickingBackend) ExportAll(int64, int) ([]store.ExportRow, error) {
	panic("panickingBackend: ExportAll")
}
func (panickingBackend) ImportRow(store.ExportRow) (store.InsertResult, error) {
	panic("panickingBackend: ImportRow")
}
func (panickingBackend) Stats() (store.StoreStats, error) { panic("boom") }

func (panickingBackend) HealthDetails() (map[string]string, error) {
	panic("panickingBackend: HealthDetails")
}
func (panickingBackend) Close() error { panic("panickingBackend: Close") }

var _ store.Backend = panickingBackend{}

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
