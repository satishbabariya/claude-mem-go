package worker

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/hook"
	"github.com/satishbabariya/claude-mem-go/internal/logging"
	"github.com/satishbabariya/claude-mem-go/internal/pool"
)

// TestReportRecallReachesTheDaemonOverARealSocket exercises the whole
// path — a client dialing the real socket, handleConn routing the
// message, and the counters landing in a snapshot — rather than calling
// recordRecall directly. The routing is the part that can break: every
// message kind shares this socket, and a report that failed to match
// would be silently treated as a hook payload and dropped.
func TestReportRecallReachesTheDaemonOverARealSocket(t *testing.T) {
	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0)}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{}, pool: pool.New(2)}
	socketPath := newTestSocket(t, d)

	reports := []struct {
		src hook.RecallSource
		n   int
	}{
		{hook.RecallPrompt, 5}, {hook.RecallPrompt, 0},
		{hook.RecallSession, 3}, {hook.RecallSession, 0},
		{hook.RecallFile, 0}, {hook.RecallFile, 0},
	}
	for _, r := range reports {
		if err := hook.ReportRecall(socketPath, r.src, r.n); err != nil {
			t.Fatalf("ReportRecall(%s, %d): %v", r.src, r.n, err)
		}
	}

	// ReportRecall is one-way by design: it returns once the write lands,
	// which is strictly before the daemon has read and counted it. Reading
	// the counters immediately races the server and loses the last report
	// — observed, not hypothesized, when this test was first written
	// without the wait. Polling matches how the privacy-marker test
	// handles the same property of the same socket.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if all, _ := d.counters.snapshot(0, 0, 0).RecallTotals(); all == int64(len(reports)) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	got := d.counters.snapshot(0, 0, 0)
	for _, want := range []struct {
		src             string
		searches, empty int64
	}{
		{"prompt", 2, 1},
		{"session", 2, 1},
		{"file", 2, 2},
	} {
		var found bool
		for _, nr := range got.RecallAll() {
			if nr.Source != want.src {
				continue
			}
			found = true
			if nr.Stat.Searches != want.searches || nr.Stat.Empty != want.empty {
				t.Errorf("recall[%s] = %+v, want searches=%d empty=%d", want.src, nr.Stat, want.searches, want.empty)
			}
		}
		if !found {
			t.Errorf("RecallAll() never reported source %q — a read path that reports nowhere is unobservable", want.src)
		}
	}

	// The file path must NOT feed the alarming rate: it is legitimately
	// empty most of the time, and averaging it in would make the doctor
	// warning fire on healthy installs.
	alarmAll, alarmEmpty := got.RecallAlarming()
	if alarmAll != 4 || alarmEmpty != 2 {
		t.Errorf("RecallAlarming() = (%d, %d), want (4, 2) — file-context must be excluded", alarmAll, alarmEmpty)
	}
	if all, empty := got.RecallTotals(); all != 6 || empty != 4 {
		t.Errorf("RecallTotals() = (%d, %d), want (6, 4)", all, empty)
	}
}

// TestRecallReportDoesNotTouchLastActivity pins a distinction that is
// easy to get wrong and impossible to notice once wrong: last_activity
// means "the CAPTURE pipeline is alive". Letting a read update it would
// make a daemon whose writes have completely stalled look healthy, which
// inverts the meaning of the one field an operator checks first.
func TestRecallReportDoesNotTouchLastActivity(t *testing.T) {
	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0)}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{}, pool: pool.New(2)}
	socketPath := newTestSocket(t, d)

	if before := d.counters.snapshot(0, 0, 0).LastActivityAt; before != "" {
		t.Fatalf("LastActivityAt = %q before anything happened, want empty", before)
	}
	if err := hook.ReportRecall(socketPath, hook.RecallPrompt, 4); err != nil {
		t.Fatalf("ReportRecall: %v", err)
	}
	// Wait for the report to actually be COUNTED before asserting what it
	// did not do. Without this the assertion passes for the wrong reason —
	// the report simply hadn't been processed yet — which is the classic
	// way a negative assertion becomes a tautology.
	deadline := time.Now().Add(2 * time.Second)
	for d.counters.recallPrompt.searches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.counters.recallPrompt.searches.Load() == 0 {
		t.Fatal("the recall report was never processed — the negative assertion below would be meaningless")
	}
	if after := d.counters.snapshot(0, 0, 0).LastActivityAt; after != "" {
		t.Errorf("LastActivityAt = %q after only a recall report, want it still empty — "+
			"a read must not make a stalled capture path look alive", after)
	}
}

// TestRecallCountersAppearInPrometheusOutput guards the last link in the
// chain: a counter that never reaches the exposition format is not
// observable, which was the entire point of adding it.
func TestRecallCountersAppearInPrometheusOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMetrics(&buf, Stats{
		RecallPrompt:  RecallStat{Searches: 9, Empty: 2},
		RecallSession: RecallStat{Searches: 4, Empty: 0},
		RecallFile:    RecallStat{Searches: 7, Empty: 6},
	}); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`claude_mem_go_recall_searches_total{source="prompt"} 9`,
		`claude_mem_go_recall_empty_total{source="prompt"} 2`,
		`claude_mem_go_recall_searches_total{source="session"} 4`,
		`claude_mem_go_recall_empty_total{source="file"} 6`,
		// One HELP/TYPE pair per metric NAME, not per series — emitting
		// them per label value is malformed exposition that scrapers
		// reject.
		"# TYPE claude_mem_go_recall_searches_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q:\n%s", want, out)
		}
	}
}

// TestRecallReportReachesTheStatsFile guards the gap that the unit tests
// above could not see and that only showed up end to end: doctor reads
// the stats FILE, not the live daemon, and every other counter reaches
// that file via the capture path's own deferred recordStats. A recall
// report arriving on a machine that is reading but not writing would
// otherwise never be persisted, and doctor would report recall=0/0
// indefinitely while the metrics endpoint showed the true numbers.
func TestRecallReportReachesTheStatsFile(t *testing.T) {
	statsPath := filepath.Join(t.TempDir(), "stats.json")
	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0), StatsPath: statsPath}
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{}, pool: pool.New(2)}
	socketPath := newTestSocket(t, d)

	if err := hook.ReportRecall(socketPath, hook.RecallSession, 0); err != nil {
		t.Fatalf("ReportRecall: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, err := ReadStatsFile(statsPath)
		if err == nil && s.RecallSession.Searches == 1 {
			if got := s.RecallSession.Empty; got != 1 {
				t.Fatalf("stats file recall[session].Empty = %d, want 1", got)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	s, err := ReadStatsFile(statsPath)
	t.Fatalf("recall never reached the stats file within 2s (read err=%v, stats=%+v) — "+
		"doctor reads this file, so the counter would be invisible there", err, s)
}

// TestHandleConnSurvivesAPanicInDispatch pins the backstop added
// alongside the recall protocol. handleConn runs as a bare goroutine off
// the accept loop, so an unrecovered panic there does not fail one
// request — it kills the daemon, and with it every project on the
// machine. process() had this protection; the protocol dispatch that runs
// before it did not, and that dispatch keeps growing.
//
// A nil pool is used as the trigger because it is a real reachable
// panic — Stats() walks into the pool, and the recall branch now calls
// recordStats — rather than a synthetic panic injected into the code
// under test.
func TestHandleConnSurvivesAPanicInDispatch(t *testing.T) {
	statsPath := filepath.Join(t.TempDir(), "stats.json")
	d := &Daemon{Log: logging.New(&bytes.Buffer{}, "", 0), StatsPath: statsPath}
	// Deliberately NO pool: Stats() will panic when the recall branch
	// reaches it.
	d.sessions = &sessionCache{byID: map[string]*sessionEntry{}}
	socketPath := newTestSocket(t, d)

	if err := hook.ReportRecall(socketPath, hook.RecallPrompt, 0); err != nil {
		t.Fatalf("ReportRecall: %v", err)
	}
	// Give the panicking handler time to run and be recovered.
	time.Sleep(50 * time.Millisecond)

	// The daemon must still be serving. A crashed one fails this.
	if err := hook.ReportRecall(socketPath, hook.RecallPrompt, 1); err != nil {
		t.Fatalf("daemon stopped serving after a panic in handleConn: %v", err)
	}
}
