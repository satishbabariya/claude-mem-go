package worker

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"claude-mem-go/observer"
	"claude-mem-go/store"
	"claude-mem-go/transcript"
)

// recordingFakeHandle captures the transcript.ToolCall process() actually
// hands to Observe — the real point of insertion to check, since a
// privacy-tag leak here reaches both the observer LLM prompt and (via
// tc.ToolInput/ToolOutput's role in store.ContentHash) whatever gets
// persisted, not just one or the other.
type recordingFakeHandle struct {
	got transcript.ToolCall
}

func (f *recordingFakeHandle) Observe(tc transcript.ToolCall) (observer.Turn, error) {
	f.got = tc
	return observer.Turn{Observation: store.Observation{Type: "change", Title: "t"}}, nil
}
func (f *recordingFakeHandle) Close() error { return nil }

// TestProcessStripsPrivacyTagsBeforeObserving is the regression test for a
// real gap: real claude-mem's own PostToolUse capture path
// (src/services/worker/http/shared.ts) runs stripMemoryTags on
// JSON.stringify(toolInput)/JSON.stringify(toolResponse) before an
// observation is ever queued — this port had no equivalent anywhere, so a
// <private>...</private> block a user's tool output happened to contain
// would reach the observer LLM prompt, get summarized, and get persisted
// verbatim. Confirms both that the raw tag markup is gone from what
// Observe receives AND that the literal secret text inside the tag is
// gone too — a fix that stripped only the tag delimiters but left the
// content behind would fail this.
func TestProcessStripsPrivacyTagsBeforeObserving(t *testing.T) {
	handle := &recordingFakeHandle{}
	entry := &sessionEntry{handle: handle}

	var logBuf bytes.Buffer
	d := &Daemon{Log: log.New(&logBuf, "", 0)}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{"s1": entry}}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	d.st = st

	payload := []byte(`{"session_id":"s1","cwd":"/proj","hook_event_name":"PostToolUse","tool_name":"Bash",` +
		`"tool_input":{"command":"echo hi"},` +
		`"tool_response":{"stdout":"public output <private>AWS_SECRET=xyz123</private> more public"}}`)
	d.process(context.Background(), payload)

	if strings.Contains(handle.got.ToolOutput, "<private>") {
		t.Errorf("ToolOutput = %q, still contains a <private> tag", handle.got.ToolOutput)
	}
	if strings.Contains(handle.got.ToolOutput, "AWS_SECRET") {
		t.Errorf("ToolOutput = %q, the private secret itself leaked through", handle.got.ToolOutput)
	}
	if !strings.Contains(handle.got.ToolOutput, "public output") || !strings.Contains(handle.got.ToolOutput, "more public") {
		t.Errorf("ToolOutput = %q, lost non-private surrounding content", handle.got.ToolOutput)
	}
}
