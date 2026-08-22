package worker

import (
	"bytes"
	"strings"
	"testing"
)

// TestStatsReportsTheStoreRedacted covers the field that makes a
// worker/hooks split detectable at all.
//
// The daemon opens its store once, at start, and nothing re-reads
// $CLAUDE_MEM_DB afterwards — correctly, since a daemon switching
// databases underneath in-flight work would be worse. But that means a
// worker started before the variable changed keeps writing to the OLD
// store while every hook, every CLI command and `doctor` resolve the new
// one. Reproduced end to end: worker on store A, CLAUDE_MEM_DB then
// pointing at B, one PostToolUse event — the observation landed in A
// while `doctor -db B` reported "worker daemon reachable", "database
// reachable (B)" and "the store is empty". Every check green, memory in
// another file.
func TestStatsReportsTheStoreRedacted(t *testing.T) {
	d := &Daemon{DBPath: "postgres://user:hunter2@localhost:5432/mem?sslmode=disable"}
	s := d.Stats()

	if s.Store == "" {
		t.Fatal("Stats().Store is empty — the daemon's store is unknowable, which is the whole gap")
	}
	if strings.Contains(s.Store, "hunter2") {
		t.Fatalf("the password leaked into Stats().Store: %q. This is written to a "+
			"world-readable file in the user's home and served over plain HTTP by the metrics endpoint.", s.Store)
	}
	if !strings.Contains(s.Store, "localhost:5432") {
		t.Fatalf("Store = %q, want it still identifiable enough to compare against — "+
			"redaction must not make the value useless for spotting a mismatch", s.Store)
	}
}

// TestMetricsExposeTheStoreAsInfoLabel pins the Prometheus surface. An
// _info gauge with an identity label is the idiomatic way to answer
// "which store is this daemon writing to" from monitoring, rather than
// only from `doctor` on the same machine.
func TestMetricsExposeTheStoreAsInfoLabel(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMetrics(&buf, Stats{Store: "/home/u/.claude-mem-go/observations.db", Processed: 3}); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `claude_mem_go_worker_info{store="/home/u/.claude-mem-go/observations.db"} 1`) {
		t.Fatalf("no identity metric in the exposition:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE claude_mem_go_worker_info gauge") {
		t.Fatalf("identity metric is missing its TYPE line, which makes it invalid exposition:\n%s", out)
	}
	// The pre-existing counters must survive the addition.
	if !strings.Contains(out, "claude_mem_go_worker_processed_total 3") {
		t.Fatalf("existing metrics were broken by the addition:\n%s", out)
	}
}

// TestMetricsOmitTheInfoLabelWhenUnknown guards against emitting
// `store=""`, which would read as a real (empty) identity to a scraper
// rather than as "not reported".
func TestMetricsOmitTheInfoLabelWhenUnknown(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMetrics(&buf, Stats{Processed: 1}); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}
	if strings.Contains(buf.String(), "claude_mem_go_worker_info") {
		t.Fatalf("emitted an identity metric with no store to report:\n%s", buf.String())
	}
}
