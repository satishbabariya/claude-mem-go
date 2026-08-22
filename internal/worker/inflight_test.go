package worker

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"

	"github.com/satishbabariya/claude-mem-go/internal/hook"
	"github.com/satishbabariya/claude-mem-go/internal/observer"
	"github.com/satishbabariya/claude-mem-go/internal/transcript"
)

// TestParseInFlightQueryRoundTrips confirms the plain-text query protocol
// hook.QueryInFlight speaks and worker's handleConn parses agree on the
// exact wire format — the two sides are unexported constants in different
// packages, so nothing else enforces they stay in sync except this test
// and ParseInFlightQuery/QueryInFlight sharing hook package's one
// unexported prefix constant.
func TestParseInFlightQueryRoundTrips(t *testing.T) {
	sid, ok := hook.ParseInFlightQuery([]byte("INFLIGHT abc-123"))
	if !ok || sid != "abc-123" {
		t.Fatalf("ParseInFlightQuery(%q) = (%q, %v), want (%q, true)", "INFLIGHT abc-123", sid, ok, "abc-123")
	}
	if _, ok := hook.ParseInFlightQuery([]byte(`{"session_id":"s1"}`)); ok {
		t.Fatalf("ParseInFlightQuery must not treat a real (JSON) hook payload as a query")
	}
}

// blockingHandle is an observer.Handle whose Observe blocks until release
// is closed — lets a test hold a session "in flight" for as long as it
// needs to. Shared by this file's in-flight-query test and
// sessions_test.go's eviction-safety test; closed tracks whether Close
// was ever called (atomically, since it's read/written from different
// goroutines across those tests) for the latter's own assertions.
type blockingHandle struct {
	release chan struct{}
	closed  int32
}

func (b *blockingHandle) Observe(tc transcript.ToolCall) (observer.Turn, error) {
	<-b.release
	return observer.Turn{Observation: memory.Observation{Title: "done"}}, nil
}
func (b *blockingHandle) Close() error {
	atomic.StoreInt32(&b.closed, 1)
	return nil
}
func (b *blockingHandle) isClosed() bool { return atomic.LoadInt32(&b.closed) == 1 }

// TestInFlightQueryReflectsARealInProgressEvent is the direct regression
// test for the whole point of this package: the exact gap the Stop hook's
// waitForSessionObservations doc comment named as unresolved — no way to
// ask the worker directly whether it was still catching up on a session,
// only to infer it from watching a row count. Runs a real Unix socket
// (not net.Pipe — QueryInFlight's half-close needs a real *net.UnixConn)
// with an Observe call that blocks on command, and drives the whole
// exchange through the actual client functions (hook.Forward,
// hook.QueryInFlight), not reimplemented test-only wire code. Confirms an
// INFLIGHT query made WHILE that event is still being processed reports a
// nonzero count, and a query made AFTER it completes reports zero again.
func TestInFlightQueryReflectsARealInProgressEvent(t *testing.T) {
	var logBuf bytes.Buffer
	release := make(chan struct{})
	entry := &sessionEntry{handle: &blockingHandle{release: release}, lastUsed: time.Now()}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// A short, dedicated temp dir rather than t.TempDir(): this test's own
	// name is long enough that t.TempDir()'s path (which embeds the test
	// name) plus "w.sock" can exceed sockaddr_un's ~104-byte sun_path
	// limit on macOS/BSD, failing net.Listen with "invalid argument" —
	// confirmed by hand.
	sockDir, err := os.MkdirTemp("", "cmg")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(sockDir)
	socketPath := filepath.Join(sockDir, "w.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	d := &Daemon{Log: logging.New(&logBuf, "", 0), st: st}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{"s1": entry}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handleConn(ctx, conn)
		}
	}()

	payload := []byte(`{"session_id":"s1","cwd":"/proj","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)
	if _, err := hook.Forward(socketPath, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	// process's getInflight().inc happens before it ever reaches the
	// blocking Observe call, but there's an inherent (small, real) window
	// between Forward returning and that goroutine actually running —
	// poll briefly for the increment to land rather than asserting
	// instantly.
	deadline := time.Now().Add(2 * time.Second)
	for d.getInflight().count("s1") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := d.getInflight().count("s1"); n != 1 {
		t.Fatalf("in-flight count while Observe is blocked = %d, want 1", n)
	}

	n, err := hook.QueryInFlight(socketPath, "s1")
	if err != nil {
		t.Fatalf("QueryInFlight while blocked: %v", err)
	}
	if n != 1 {
		t.Fatalf("QueryInFlight while Observe is blocked = %d, want 1", n)
	}

	close(release)

	deadline = time.Now().Add(2 * time.Second)
	for d.getInflight().count("s1") != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := d.getInflight().count("s1"); n != 0 {
		t.Fatalf("in-flight count after Observe completed = %d, want 0", n)
	}

	n, err = hook.QueryInFlight(socketPath, "s1")
	if err != nil {
		t.Fatalf("QueryInFlight after completion: %v", err)
	}
	if n != 0 {
		t.Fatalf("QueryInFlight after completion = %d, want 0", n)
	}
}
