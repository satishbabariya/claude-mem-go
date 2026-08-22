package worker

import (
	"bytes"
	"context"
	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/hook"
)

// TestHandleConnRejectsOversizedPayload is the regression test for the real
// gap hook.MaxPayloadBytes closes on the daemon side: before this bound
// existed, handleConn read a client's payload with io.ReadAll and no
// upper bound at all — the one long-lived daemon process every project on
// the machine shares would have had no defense against an abnormally
// large payload (a bug in a future client, not just hook.Forward's own
// client-side cap) ballooning its memory. Deliberately constructs an
// otherwise-empty *Daemon (no store, no session pool) — if the size check
// didn't actually stop this payload before it reached d.process, calling
// process on such a bare Daemon would panic or error loudly, which would
// fail this test too.
func TestHandleConnRejectsOversizedPayload(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.New(&logBuf, "", 0)}

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		d.handleConn(context.Background(), serverConn)
		close(done)
	}()

	oversized := bytes.Repeat([]byte("x"), hook.MaxPayloadBytes+1)
	go func() {
		clientConn.Write(oversized)
		clientConn.Close()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConn did not return in time — want it to reject and return promptly, not read indefinitely")
	}

	if !strings.Contains(logBuf.String(), "REJECTED") {
		t.Errorf("daemon log = %q, want a REJECTED message for a payload over hook.MaxPayloadBytes", logBuf.String())
	}
}

// TestHandleConnClosesStalledClientAfterReadDeadline is the regression
// test for a real, found-by-hand gap: before handleConnReadTimeout
// existed, nothing bounded how long handleConn would wait for a client to
// send its payload — only how many bytes it would accept once bytes
// started arriving. A client that connects and then never writes or
// closes (a stalled process, a bug in some future caller not going
// through hook.Forward) would leak this goroutine and its underlying FD
// for as long as the daemon runs, which is meant to be days. Shrinks
// handleConnReadTimeout to run this near-instantly rather than waiting
// out the real 30s production value, then proves handleConn actually
// returns once it elapses against a client that deliberately does
// nothing at all.
func TestHandleConnClosesStalledClientAfterReadDeadline(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.New(&logBuf, "", 0)}

	original := handleConnReadTimeout
	handleConnReadTimeout = 50 * time.Millisecond
	t.Cleanup(func() { handleConnReadTimeout = original })

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		d.handleConn(context.Background(), serverConn)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not return once the read deadline elapsed — a stalled client would leak this goroutine forever")
	}

	if !strings.Contains(logBuf.String(), "stalled client") {
		t.Errorf("daemon log = %q, want a message about the read deadline firing", logBuf.String())
	}
}
