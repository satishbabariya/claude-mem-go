package worker

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/logging"

	"github.com/satishbabariya/claude-mem-go/internal/hook"
	"github.com/satishbabariya/claude-mem-go/internal/sockettest"
)

// TestParsePrivacyMarkerRoundTrips confirms the plain-text setter protocol
// hook.SetSessionPrivate speaks and worker's handleConn parses agree on
// the exact wire format, the same kind of check inflight_test.go's
// TestParseInFlightQueryRoundTrips does for the INFLIGHT protocol.
func TestParsePrivacyMarkerRoundTrips(t *testing.T) {
	sid, private, ok := hook.ParsePrivacyMarker([]byte("PRIVATE abc-123 1"))
	if !ok || sid != "abc-123" || !private {
		t.Fatalf("ParsePrivacyMarker(%q) = (%q, %v, %v), want (%q, true, true)", "PRIVATE abc-123 1", sid, private, ok, "abc-123")
	}
	sid, private, ok = hook.ParsePrivacyMarker([]byte("PRIVATE abc-123 0"))
	if !ok || sid != "abc-123" || private {
		t.Fatalf("ParsePrivacyMarker(%q) = (%q, %v, %v), want (%q, false, true)", "PRIVATE abc-123 0", sid, private, ok, "abc-123")
	}
	if _, _, ok := hook.ParsePrivacyMarker([]byte(`{"session_id":"s1"}`)); ok {
		t.Fatal("ParsePrivacyMarker must not treat a real (JSON) hook payload as a marker")
	}
}

// TestParsePrivacyQueryRoundTrips is the same check for the (distinct)
// ISPRIVATE query prefix.
func TestParsePrivacyQueryRoundTrips(t *testing.T) {
	sid, ok := hook.ParsePrivacyQuery([]byte("ISPRIVATE abc-123"))
	if !ok || sid != "abc-123" {
		t.Fatalf("ParsePrivacyQuery(%q) = (%q, %v), want (%q, true)", "ISPRIVATE abc-123", sid, ok, "abc-123")
	}
	if _, ok := hook.ParsePrivacyQuery([]byte("PRIVATE abc-123 1")); ok {
		t.Fatal("ParsePrivacyQuery must not treat a privacy-SETTER marker as a query")
	}
}

// newTestSocket sets up a real Unix listener bound to a short temp path
// (not t.TempDir(), whose long path embedding the test name can exceed
// sockaddr_un's ~104-byte limit on macOS/BSD — confirmed by hand
// elsewhere in this package) accepting connections through
// d.handleConn, matching inflight_test.go's own harness.
func newTestSocket(t *testing.T, d *Daemon) string {
	t.Helper()
	socketPath := filepath.Join(sockettest.Dir(t), "w.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handleConn(ctx, conn)
		}
	}()
	return socketPath
}

// TestSetSessionPrivateThenQueryPrivateReflectsRealState drives the whole
// exchange through the actual client functions (hook.SetSessionPrivate,
// hook.QueryPrivate) against a real worker daemon over a real socket, not
// reimplemented test-only wire code — confirms a session marked private
// reads back as private, and a LATER marker superseding it to false
// (the case that matters most: this is what stops the flag going stale
// once a session's next prompt isn't private) reads back as false too.
func TestSetSessionPrivateThenQueryPrivateReflectsRealState(t *testing.T) {
	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0)}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{}}
	socketPath := newTestSocket(t, d)

	if err := hook.SetSessionPrivate(socketPath, "s1", true); err != nil {
		t.Fatalf("SetSessionPrivate(true): %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !d.sessions.isPrivate("s1") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	private, err := hook.QueryPrivate(socketPath, "s1")
	if err != nil {
		t.Fatalf("QueryPrivate after marking private: %v", err)
	}
	if !private {
		t.Fatal("QueryPrivate after marking private = false, want true")
	}

	if err := hook.SetSessionPrivate(socketPath, "s1", false); err != nil {
		t.Fatalf("SetSessionPrivate(false): %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for d.sessions.isPrivate("s1") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	private, err = hook.QueryPrivate(socketPath, "s1")
	if err != nil {
		t.Fatalf("QueryPrivate after clearing: %v", err)
	}
	if private {
		t.Fatal("QueryPrivate after a later non-private marker superseded it = true, want false — the flag went stale")
	}

	// A session that was never marked at all must read back as not
	// private — the absent-signal-defaults-to-allow guarantee
	// isPrivate's own doc comment describes.
	private, err = hook.QueryPrivate(socketPath, "never-seen")
	if err != nil {
		t.Fatalf("QueryPrivate for an unseen session: %v", err)
	}
	if private {
		t.Fatal("QueryPrivate for a session with no marker ever sent = true, want false")
	}
}

// TestProcessSkipsWhenSessionMarkedPrivate is the regression test for the
// real gap this file exists to close: real claude-mem's own
// PrivacyCheckValidator suppresses PostToolUse observation generation for
// an entire turn once its prompt stripped to nothing — this port had
// tag-stripping on individual fields (see privacy_test.go) but nothing
// carried that suppression across to a private turn's tool calls at all.
// Uses a bare *Daemon with a nil byID map, the same technique
// excludeproject_test.go's TestProcessSkipsExcludedProjectBeforeAnyRealWork
// uses to prove the check happens BEFORE process() ever reaches
// d.sessions.getOrCreate (which would otherwise panic on this
// intentionally nil-backed cache) — proving privacy is checked early, not
// just that it exists somewhere.
func TestProcessSkipsWhenSessionMarkedPrivate(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.New(&logBuf, "", 0)}
	d.sessions = &sessionCache{}
	d.sessions.setPrivate("s1", true)

	payload := []byte(`{"session_id":"s1","cwd":"/proj","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.process(context.Background(), payload)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not return")
	}

	if !strings.Contains(logBuf.String(), "skip: session s1 marked private") {
		t.Errorf("log output = %q, want a privacy-skip message", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "PANIC recovered") {
		t.Errorf("log output = %q, want no panic — the privacy check must happen before d.sessions.getOrCreate is ever touched", logBuf.String())
	}
}

// TestProcessDoesNotSkipWhenNotMarkedPrivate is the non-regression
// counterpart: a session with no privacy flag set at all (the common
// case — most turns aren't private) must not be skipped, confirmed by
// letting process() reach the same nil-handle-triggered panic-recovery
// path TestProcessPanicRecoverySurvives already exercises without any
// privacy state involved.
func TestProcessDoesNotSkipWhenNotMarkedPrivate(t *testing.T) {
	var logBuf bytes.Buffer
	d := &Daemon{Log: logging.New(&logBuf, "", 0)}
	d.sessions = &sessionCache{}

	payload := []byte(`{"session_id":"s1","cwd":"/proj","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{},"tool_response":{}}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.process(context.Background(), payload)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not return")
	}

	if strings.Contains(logBuf.String(), "skip: session") && strings.Contains(logBuf.String(), "marked private") {
		t.Errorf("log output = %q, a session with no privacy flag set must not be skipped as private", logBuf.String())
	}
}

// TestSessionCacheEvictStalePrivacyRemovesOldEntries confirms the map
// bounding this file's setPrivate relies on to avoid growing forever
// across a long-lived daemon's whole runtime.
func TestSessionCacheEvictStalePrivacyRemovesOldEntries(t *testing.T) {
	c := &sessionCache{}
	c.setPrivate("stale", true)
	c.priv["stale"] = privacyFlag{private: true, setAt: time.Now().Add(-2 * sessionIdleTimeout)}
	c.setPrivate("fresh", true)

	c.evictStalePrivacy()

	if c.isPrivate("stale") {
		t.Error("stale entry should have been evicted, but isPrivate still reports true (map entry, and thus the flag itself, may not have been removed)")
	}
	c.privMu.Lock()
	_, staleStillPresent := c.priv["stale"]
	_, freshStillPresent := c.priv["fresh"]
	c.privMu.Unlock()
	if staleStillPresent {
		t.Error("stale entry still present in the map after evictStalePrivacy")
	}
	if !freshStillPresent {
		t.Error("fresh entry was incorrectly evicted too")
	}
}
