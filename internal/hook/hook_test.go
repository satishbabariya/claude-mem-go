package hook

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForwardDeliversExactBytes(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	const payload = `{"session_id":"s1","tool_name":"Bash"}`
	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			received <- ""
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	n, err := Forward(socketPath, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Forward reported %d bytes sent, want %d", n, len(payload))
	}

	got := <-received
	if got != payload {
		t.Fatalf("daemon side received %q, want %q — bytes were altered in transit", got, payload)
	}
}

// TestForwardRejectsOversizedPayloadWithoutSending is the regression test
// for the real gap MaxPayloadBytes closes: before it existed, Forward read
// and forwarded a hook payload of ANY size — an abnormally large
// tool_response (a Bash command that cats a huge file) would have gone
// straight to the shared worker daemon with no bound at all. This confirms
// two things at once: Forward returns an error instead of silently
// succeeding, and — the part a size check alone wouldn't prove — nothing
// was actually written to the socket at all.
func TestForwardRejectsOversizedPayloadWithoutSending(t *testing.T) {
	// A short, manually-made temp dir, not t.TempDir(): that helper embeds
	// this (unusually long) test function's own name into the path, and a
	// unix socket path has a real OS-enforced length limit (macOS/BSD's
	// sun_path) this test's own name was long enough to blow past.
	dir, err := os.MkdirTemp("", "hook")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	receivedAnything := make(chan bool, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		n, _ := conn.Read(buf)
		receivedAnything <- n > 0
	}()

	oversized := strings.NewReader(strings.Repeat("x", MaxPayloadBytes+1))
	n, err := Forward(socketPath, oversized)
	if err == nil {
		t.Fatal("Forward with an oversized payload: want an error, got nil")
	}
	if n != 0 {
		t.Errorf("Forward reported %d bytes sent for a rejected payload, want 0", n)
	}

	select {
	case gotData := <-receivedAnything:
		if gotData {
			t.Error("the daemon side received bytes despite Forward rejecting the payload as oversized")
		}
	case <-time.After(200 * time.Millisecond):
		// Nothing arrived at all within a generous window — the expected
		// outcome, since Forward must never have dialed the socket.
	}
}

func TestForwardAcceptsPayloadExactlyAtTheLimit(t *testing.T) {
	dir, err := os.MkdirTemp("", "hook")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
	}()

	exact := strings.NewReader(strings.Repeat("x", MaxPayloadBytes))
	n, err := Forward(socketPath, exact)
	if err != nil {
		t.Fatalf("Forward at exactly MaxPayloadBytes: want success, got %v", err)
	}
	if n != MaxPayloadBytes {
		t.Errorf("Forward reported %d bytes sent, want exactly %d", n, MaxPayloadBytes)
	}
}

func TestForwardFailsCleanlyWhenNothingListens(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "nonexistent.sock")
	_, err := Forward(socketPath, strings.NewReader("payload"))
	if err == nil {
		t.Fatal("Forward against an unreachable socket: want an error, got nil")
	}
}

type erroringReader struct{}

func (erroringReader) Read([]byte) (int, error) { return 0, errors.New("simulated read failure") }

func TestForwardPropagatesReadError(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	_, err = Forward(socketPath, erroringReader{})
	if err == nil {
		t.Fatal("Forward with a failing reader: want an error, got nil")
	}
}

// TestSendersRejectUnparseableSessionIDs: the plain-text protocol splits
// on whitespace, so a session ID (or prompt hash) containing any would
// produce a message the worker's parsers silently drop. Every sender must
// refuse such input with an error before dialing — the listener here
// records any connection so "nothing was sent" is asserted, not assumed.
func TestSendersRejectUnparseableSessionIDs(t *testing.T) {
	dir, err := os.MkdirTemp("", "hooksid")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	socketPath := filepath.Join(dir, "w.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	connected := make(chan struct{}, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connected <- struct{}{}
			c.Close()
		}
	}()

	for _, bad := range []string{"", "has space", "tab\there", "new\nline"} {
		if err := SetSessionPrivate(socketPath, bad, true); err == nil {
			t.Errorf("SetSessionPrivate(%q): want error, got nil", bad)
		}
		if _, err := QueryPrivate(socketPath, bad); err == nil {
			t.Errorf("QueryPrivate(%q): want error, got nil", bad)
		}
		if _, err := QueryInFlight(socketPath, bad); err == nil {
			t.Errorf("QueryInFlight(%q): want error, got nil", bad)
		}
		if _, err := CheckDuplicatePrompt(socketPath, bad, "abc"); err == nil {
			t.Errorf("CheckDuplicatePrompt(%q, ok): want error, got nil", bad)
		}
		if _, err := CheckDuplicatePrompt(socketPath, "ok", bad); err == nil {
			t.Errorf("CheckDuplicatePrompt(ok, %q): want error, got nil", bad)
		}
	}
	select {
	case <-connected:
		t.Fatal("a sender dialed the worker despite an unparseable session id — the message would have been silently dropped server-side")
	case <-time.After(50 * time.Millisecond):
	}
	if !validSessionID("0f4c2a9e-1234-4bcd-9abc-0123456789ab") {
		t.Fatal("a real UUID session id must be accepted")
	}
}
