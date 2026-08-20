package worker

import (
	"bytes"
	"context"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"claude-mem-go/hook"
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
	d := &Daemon{Log: log.New(&logBuf, "", 0)}

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
