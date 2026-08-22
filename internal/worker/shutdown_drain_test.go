package worker

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"

	"github.com/satishbabariya/claude-mem-go/internal/observer"
	"github.com/satishbabariya/claude-mem-go/internal/pool"
	"github.com/satishbabariya/claude-mem-go/internal/transcript"
)

// blockingHandleWithValidType is inflight_test.go's blockingHandle but
// returning an Observation with a real Type from memory.ValidObservationTypes
// — needed here (unlike blockingHandle's other uses, which only check
// in-flight counts) because this test confirms the drained turn's
// observation actually reaches the store, and store.Insert rejects an
// empty/invalid type outright.
type blockingHandleWithValidType struct {
	release chan struct{}
}

func (b *blockingHandleWithValidType) Observe(tc transcript.ToolCall) (observer.Turn, error) {
	<-b.release
	return observer.Turn{Observation: memory.Observation{Type: "discovery", Title: "done"}}, nil
}
func (b *blockingHandleWithValidType) Close() error { return nil }

// TestDispatchProcessIsDrainedBeforeShutdown is the real regression test
// for the gap sessionCache.closeAll's own doc comment names but
// deliberately doesn't attempt: a process() goroutine for a session
// closeAll's map snapshot could never have known about (because it's
// still inside getOrCreate, registering itself, when the snapshot is
// taken) must still be waited on before shutdown proceeds — otherwise
// its own Insert call can race a store that's about to close, silently
// dropping the observation.
//
// Simulates exactly that: dispatchProcess is called directly (the same
// thing handleConn does for a real payload) against a session that has
// NEVER been registered in d.sessions.byID — mirroring a brand-new
// session whose connection was accepted a moment before shutdown began.
// A blockingHandle holds the simulated turn in flight until released,
// so waitForProcessDrain's own wait can be observed directly: it must
// NOT return while the turn is still running, and the eventual
// persisted row proves the drain actually let real work finish rather
// than merely not crashing.
func TestDispatchProcessIsDrainedBeforeShutdown(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	release := make(chan struct{})
	handle := &blockingHandleWithValidType{release: release}
	p := pool.New(2)
	d := &Daemon{
		Log: logging.New(&bytes.Buffer{}, "", 0),
		st:  st,
	}
	d.sessions = newSessionCache(p, func(ctx context.Context) (observer.Handle, error) {
		return handle, nil
	})

	payload := []byte(`{"session_id":"brand-new-session","cwd":"/proj","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)
	d.dispatchProcess(context.Background(), payload)

	drainDone := make(chan struct{})
	go func() {
		d.waitForProcessDrain()
		close(drainDone)
	}()

	select {
	case <-drainDone:
		t.Fatal("waitForProcessDrain returned while the dispatched process() goroutine was still genuinely in flight (Observe hasn't been released yet)")
	case <-time.After(200 * time.Millisecond):
		// Expected: still draining.
	}

	close(release)

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForProcessDrain did not return after the in-flight turn was released")
	}

	results, err := st.BySessionID("brand-new-session", 10)
	if err != nil {
		t.Fatalf("BySessionID: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("BySessionID(brand-new-session) = %d rows, want exactly 1 — the drained turn's observation should have been persisted before waitForProcessDrain returned", len(results))
	}
}

// TestWaitForProcessDrainReturnsImmediatelyWithNothingInFlight is the
// non-regression counterpart: a daemon with no dispatched work at all
// must not wait out processDrainGracePeriod for no reason.
func TestWaitForProcessDrainReturnsImmediatelyWithNothingInFlight(t *testing.T) {
	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0)}
	start := time.Now()
	d.waitForProcessDrain()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitForProcessDrain with nothing in flight took %s, want near-instant", elapsed)
	}
}
