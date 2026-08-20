package hook

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
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
