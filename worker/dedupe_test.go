package worker

import (
	"bytes"
	"claude-mem-go/logging"
	"testing"
	"time"

	"claude-mem-go/hook"
)

// TestParseDedupeQueryRoundTrips confirms the plain-text request/response
// protocol hook.CheckDuplicatePrompt speaks and worker's handleConn parses
// agree on the exact wire format, the same kind of check
// inflight_test.go's TestParseInFlightQueryRoundTrips does for INFLIGHT.
func TestParseDedupeQueryRoundTrips(t *testing.T) {
	sid, hash, ok := hook.ParseDedupeQuery([]byte("DEDUPE abc-123 deadbeef"))
	if !ok || sid != "abc-123" || hash != "deadbeef" {
		t.Fatalf("ParseDedupeQuery(%q) = (%q, %q, %v), want (%q, %q, true)", "DEDUPE abc-123 deadbeef", sid, hash, ok, "abc-123", "deadbeef")
	}
	if _, _, ok := hook.ParseDedupeQuery([]byte("PRIVATE abc-123 1")); ok {
		t.Fatal("ParseDedupeQuery must not treat a privacy marker as a dedupe query")
	}
	if _, _, ok := hook.ParseDedupeQuery([]byte(`{"session_id":"s1"}`)); ok {
		t.Fatal("ParseDedupeQuery must not treat a real (JSON) hook payload as a query")
	}
}

// TestCheckDuplicatePromptReflectsRealState drives the whole exchange
// through the actual client function (hook.CheckDuplicatePrompt) against
// a real worker daemon over a real socket, matching
// TestSetSessionPrivateThenQueryPrivateReflectsRealState's own technique
// (newTestSocket, defined in privacy_socket_test.go). Confirms: a hash
// seen for the first time is never a duplicate; the identical hash
// checked again immediately IS a duplicate; a genuinely different hash
// for the same session is not.
func TestCheckDuplicatePromptReflectsRealState(t *testing.T) {
	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0)}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{}}
	socketPath := newTestSocket(t, d)

	dup, err := hook.CheckDuplicatePrompt(socketPath, "s1", "hash-a")
	if err != nil {
		t.Fatalf("CheckDuplicatePrompt (first time): %v", err)
	}
	if dup {
		t.Fatal("first-ever prompt for a session reported as duplicate, want false")
	}

	dup, err = hook.CheckDuplicatePrompt(socketPath, "s1", "hash-a")
	if err != nil {
		t.Fatalf("CheckDuplicatePrompt (repeat): %v", err)
	}
	if !dup {
		t.Fatal("identical prompt hash checked again immediately reported as NOT duplicate, want true")
	}

	dup, err = hook.CheckDuplicatePrompt(socketPath, "s1", "hash-b")
	if err != nil {
		t.Fatalf("CheckDuplicatePrompt (different hash): %v", err)
	}
	if dup {
		t.Fatal("a genuinely different prompt hash for the same session reported as duplicate, want false")
	}

	// hash-b superseded hash-a as the session's current prompt above —
	// checking hash-a again now must NOT read as a duplicate of the
	// no-longer-current hash.
	dup, err = hook.CheckDuplicatePrompt(socketPath, "s1", "hash-a")
	if err != nil {
		t.Fatalf("CheckDuplicatePrompt (stale hash after a newer one superseded it): %v", err)
	}
	if dup {
		t.Fatal("a hash superseded by a later, different prompt reported as duplicate, want false")
	}
}

// TestSessionCacheCheckAndRecordPromptWindowExpiry confirms the dedupe
// window is actually enforced, not just "the same hash is always a
// duplicate forever" — the real bug this whole feature exists to avoid
// on the OTHER side (a sticky flag that never clears): a prompt
// genuinely repeated by the user after the window elapses must be
// treated as new, not silently swallowed as a duplicate of ancient
// history.
func TestSessionCacheCheckAndRecordPromptWindowExpiry(t *testing.T) {
	c := &sessionCache{}
	if c.checkAndRecordPrompt("s1", "hash-a") {
		t.Fatal("first check reported duplicate, want false")
	}

	c.dedupeMu.Lock()
	c.dedupe["s1"] = dedupeState{hash: "hash-a", firstSeen: time.Now().Add(-2 * promptDedupeWindow)}
	c.dedupeMu.Unlock()

	if c.checkAndRecordPrompt("s1", "hash-a") {
		t.Fatal("identical hash after the dedupe window elapsed reported as duplicate, want false")
	}
	// The expired entry should have been refreshed by the call above —
	// checking it again immediately now DOES count as a duplicate.
	if !c.checkAndRecordPrompt("s1", "hash-a") {
		t.Fatal("identical hash immediately after being freshly recorded reported as NOT duplicate, want true")
	}
}

// TestSessionCacheEvictStaleDedupeRemovesOldEntries mirrors
// TestSessionCacheEvictStalePrivacyRemovesOldEntries for the map bounding
// checkAndRecordPrompt's own state.
func TestSessionCacheEvictStaleDedupeRemovesOldEntries(t *testing.T) {
	c := &sessionCache{}
	c.checkAndRecordPrompt("stale", "hash-a")
	c.dedupeMu.Lock()
	c.dedupe["stale"] = dedupeState{hash: "hash-a", firstSeen: time.Now().Add(-2 * sessionIdleTimeout)}
	c.dedupeMu.Unlock()
	c.checkAndRecordPrompt("fresh", "hash-b")

	c.evictStaleDedupe()

	c.dedupeMu.Lock()
	_, staleStillPresent := c.dedupe["stale"]
	_, freshStillPresent := c.dedupe["fresh"]
	c.dedupeMu.Unlock()
	if staleStillPresent {
		t.Error("stale entry still present in the map after evictStaleDedupe")
	}
	if !freshStillPresent {
		t.Error("fresh entry was incorrectly evicted too")
	}
}
