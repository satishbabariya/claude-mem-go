package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/sqlite"
)

// fakeOllama stands in for a real Ollama server. Every reembed test below
// drives its behaviour through the CLAUDE_MEM_OLLAMA_BASE_URL variable
// added alongside these fixes — which is itself worth noting: before that
// variable existed, the address was hardcoded at all ten NewClient call
// sites and none of this was reachable from a test at all.
type fakeOllama struct {
	mu       sync.Mutex
	embeds   int
	dims     int
	delay    time.Duration
	failFrom func(n int) bool // given the 1-based embed-call number, fail it?
}

func (f *fakeOllama) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.embeds++
		n := f.embeds
		f.mu.Unlock()

		if f.delay > 0 {
			time.Sleep(f.delay)
		}

		// A 500 is what Embed treats as retryable, so each failing row
		// costs two calls here — the same shape as a real overloaded
		// Ollama, and the reason the counts asserted below are in pairs.
		if f.failFrom != nil && f.failFrom(n) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		vec := make([]float32, f.dims)
		for i := range vec {
			vec[i] = 0.01
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": vec})
	}))
	t.Cleanup(srv.Close)
	t.Setenv(embed.BaseURLEnvVar, srv.URL)
}

func (f *fakeOllama) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.embeds
}

// seedRowsNeedingEmbedding writes n observations with no embedding at all,
// which is what ObservationsNeedingEmbedding selects for.
func seedRowsNeedingEmbedding(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reembed.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("observation %d", i)
		if _, err := st.Insert(context.Background(), "s1", "reembed-proj", "Bash",
			memory.ContentHash("s1", "Bash", title, fmt.Sprint(i)),
			memory.Observation{Type: "discovery", Title: title}, 0); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	return path
}

// TestReembedStopsWhenTheEmbeddingServiceIsDown is the regression test for
// a measured, not theorized, failure mode. The old loop did
// `failed++; continue` with no ceiling. A *dead* Ollama is survivable —
// connection refused returns in ~3ms — but a *hung* one (accepts the
// connection, never answers) costs the full 30s HTTP timeout twice per
// row, once per Embed retry: timed against a real accept-and-never-reply
// socket at 60.02s. At that rate the 6,064 rows this project's own dev
// store needed would take 101 hours, silently. See maxConsecutiveFailures
// for the qualification about when that case can actually be reached —
// an already-hung server is caught earlier, by the dimension probe.
func TestReembedStopsWhenTheEmbeddingServiceIsDown(t *testing.T) {
	const seeded = 40
	// Call 1 is cmdReembed's own dimension probe and must succeed, or the
	// command exits before the loop this test is about ever runs.
	fake := &fakeOllama{dims: 768, failFrom: func(n int) bool { return n > 1 }}
	fake.start(t)

	dbPath := seedRowsNeedingEmbedding(t, seeded)
	if rc := cmdReembed([]string{"-db", dbPath, "-yes"}); rc != 1 {
		t.Fatalf("cmdReembed exit code = %d, want 1 — a tripped breaker is a failure, not a clean run", rc)
	}

	// 1 probe + maxConsecutiveFailures rows, each costing 2 calls because
	// a 500 is retryable. Anything close to `seeded` means the breaker
	// never engaged and the run worked through every remaining row — the
	// exact behaviour that turned into 101 hours.
	want := 1 + maxConsecutiveFailures*2
	if got := fake.calls(); got != want {
		t.Fatalf("fake Ollama received %d embed calls, want exactly %d "+
			"(1 probe + %d failing rows x 2 attempts). %d rows were seeded — a count near that "+
			"means the breaker never tripped.", got, want, maxConsecutiveFailures, seeded)
	}
}

// TestReembedBreakerResetsOnSuccess guards the other half of the breaker,
// and the more likely way to get this wrong: counting failures
// cumulatively rather than consecutively. A long, mostly-healthy run over
// thousands of rows will accumulate more than five isolated failures
// eventually, and stopping it partway through would be a worse bug than
// the one the breaker fixes.
func TestReembedBreakerResetsOnSuccess(t *testing.T) {
	const seeded = 30
	// Fail the first attempt of every third row, succeed otherwise. That
	// produces a steady drip of failures — well over maxConsecutiveFailures
	// in total — but never five in a row.
	fake := &fakeOllama{dims: 768, failFrom: func(n int) bool { return n > 1 && n%7 == 0 }}
	fake.start(t)

	dbPath := seedRowsNeedingEmbedding(t, seeded)
	rc := cmdReembed([]string{"-db", dbPath, "-yes"})

	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	remaining, err := st.ObservationsNeedingEmbedding(context.Background(), "", 768, 0, 1000)
	if err != nil {
		t.Fatalf("ObservationsNeedingEmbedding: %v", err)
	}
	// Every row is reachable here: the ones whose first attempt 500s are
	// retried by Embed and succeed on the second. So a correctly-resetting
	// breaker leaves nothing behind, and rc is 0.
	if len(remaining) != 0 {
		t.Fatalf("%d of %d rows still need embedding (exit code %d) — scattered failures should "+
			"never trip the breaker, only %d CONSECUTIVE ones", len(remaining), seeded, rc, maxConsecutiveFailures)
	}
	if rc != 0 {
		t.Fatalf("cmdReembed exit code = %d, want 0 — every row was embedded", rc)
	}
}

// TestReembedEmbedsEveryRowWhenHealthy is the baseline the two tests above
// are measured against: without it, a breaker that fired on the very first
// row would pass the "stops early" test perfectly.
func TestReembedEmbedsEveryRowWhenHealthy(t *testing.T) {
	const seeded = 12
	fake := &fakeOllama{dims: 768}
	fake.start(t)

	dbPath := seedRowsNeedingEmbedding(t, seeded)
	if rc := cmdReembed([]string{"-db", dbPath, "-yes"}); rc != 0 {
		t.Fatalf("cmdReembed exit code = %d, want 0", rc)
	}
	if got, want := fake.calls(), 1+seeded; got != want {
		t.Fatalf("fake Ollama received %d embed calls, want %d (1 probe + %d rows, no retries)", got, want, seeded)
	}
}

// TestReembedDryRunMakesNoEmbedCallsAtAll pins the property the mem-reembed
// skill's "dry-run first" instruction depends on: the dry run must be free.
// If it embedded as it counted, the advice to always run it first would be
// advice to always pay the full cost twice.
func TestReembedDryRunMakesNoEmbedCallsAtAll(t *testing.T) {
	fake := &fakeOllama{dims: 768}
	fake.start(t)

	dbPath := seedRowsNeedingEmbedding(t, 25)
	if rc := cmdReembed([]string{"-db", dbPath}); rc != 0 {
		t.Fatalf("cmdReembed dry run exit code = %d, want 0", rc)
	}
	// Exactly the probe, and nothing else.
	if got := fake.calls(); got != 1 {
		t.Fatalf("dry run made %d embed calls, want exactly 1 (the dimension probe) — "+
			"a dry run that embeds is not a dry run", got)
	}
}

// captureStdout swaps os.Stdout for a pipe, runs fn, and returns what was
// written. Progress output is the whole point of the feature under test,
// so asserting on it means actually reading it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

// TestReembedReportsProgressDuringALongRun covers the other half of the
// same measured problem. Even with the breaker in place, a healthy run
// over this project's own 6,064-row backlog takes about three minutes at
// the ~33 rows/s observed here, and the old code printed absolutely
// nothing until it finished — indistinguishable, from the outside, from a
// hang. That is exactly the state the breaker exists to catch, so the
// operator needs to be able to see which one they are in.
func TestReembedReportsProgressDuringALongRun(t *testing.T) {
	// The real interval is 5s; reproducing that honestly would mean a test
	// that takes 5+ seconds. Shrinking the interval and slowing the server
	// exercises the identical code path in milliseconds.
	orig := progressInterval
	progressInterval = 5 * time.Millisecond
	t.Cleanup(func() { progressInterval = orig })

	fake := &fakeOllama{dims: 768, delay: 3 * time.Millisecond}
	fake.start(t)
	dbPath := seedRowsNeedingEmbedding(t, 30)

	var rc int
	out := captureStdout(t, func() { rc = cmdReembed([]string{"-db", dbPath, "-yes"}) })
	if rc != 0 {
		t.Fatalf("cmdReembed exit code = %d, want 0", rc)
	}

	progressLines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "re-embedded") && strings.Contains(line, "rows/s") {
			progressLines++
		}
	}
	if progressLines == 0 {
		t.Fatalf("no progress lines in reembed output over 30 slow rows — a long run must not be "+
			"silent, since silence is indistinguishable from the hang the circuit breaker exists for.\nGot:\n%s", out)
	}
}

// TestReembedPrintsNoProgressForATrivialRun is the counterweight: progress
// output that fires on every run would turn a two-row catch-up into noise.
// Time-based intervals are what make both behaviours fall out of one rule.
func TestReembedPrintsNoProgressForATrivialRun(t *testing.T) {
	fake := &fakeOllama{dims: 768}
	fake.start(t)
	dbPath := seedRowsNeedingEmbedding(t, 3)

	out := captureStdout(t, func() { cmdReembed([]string{"-db", dbPath, "-yes"}) })
	if strings.Contains(out, "rows/s") {
		t.Fatalf("a 3-row run printed progress output, want only the final summary:\n%s", out)
	}
}
