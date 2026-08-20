package worker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteMetricsProducesValidPrometheusFormat(t *testing.T) {
	s := Stats{
		Processed:      3,
		Duplicates:     1,
		ObserverErrors: 2,
		InsertErrors:   0,
		EmbedErrors:    4,
		CachedSessions: 5,
		PoolInFlight:   1,
		PoolCapacity:   2,
	}
	var buf strings.Builder
	if err := WriteMetrics(&buf, s); err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"# HELP claude_mem_go_worker_processed_total",
		"# TYPE claude_mem_go_worker_processed_total counter",
		"claude_mem_go_worker_processed_total 3",
		"claude_mem_go_worker_duplicates_total 1",
		"claude_mem_go_worker_observer_errors_total 2",
		"claude_mem_go_worker_insert_errors_total 0",
		"claude_mem_go_worker_embed_errors_total 4",
		"claude_mem_go_worker_cached_sessions 5",
		"claude_mem_go_worker_pool_in_flight 1",
		"claude_mem_go_worker_pool_capacity 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestMetricsHandlerServesRealDaemonStats(t *testing.T) {
	d := &Daemon{Log: nopLogger()}
	d.counters.processed.Add(42)

	srv := httptest.NewServer(MetricsHandler(d))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain (Prometheus exposition format)", ct)
	}

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, "claude_mem_go_worker_processed_total 42") {
		t.Errorf("metrics response = %q, want it to reflect the real daemon's counter (42)", body)
	}
}
