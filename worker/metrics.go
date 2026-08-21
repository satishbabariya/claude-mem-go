package worker

import (
	"fmt"
	"io"
	"net/http"
)

// WriteMetrics writes s in Prometheus text exposition format — the one
// observability gap the JSON stats file (see stats.go) doesn't close on
// its own: a real monitoring stack (Prometheus, or anything that scrapes
// its wire format, which by now includes most of them) has no way to pull
// this daemon's activity into a dashboard or an alert rule without
// something speaking its format. The JSON file remains the source `doctor`
// reads directly; this is the same data, shaped for a scraper instead of a
// human or a CLI.
//
// A pure function of io.Writer + Stats, not something that reads Stats
// itself, so it's testable without spinning up an HTTP server or a real
// Daemon.
func WriteMetrics(w io.Writer, s Stats) error {
	lines := []struct {
		name, help, typ string
		value           float64
	}{
		{"claude_mem_go_worker_processed_total", "Observations persisted.", "counter", float64(s.Processed)},
		{"claude_mem_go_worker_duplicates_total", "Turns that hit an already-persisted content_hash.", "counter", float64(s.Duplicates)},
		{"claude_mem_go_worker_observer_errors_total", "Observer turns that failed.", "counter", float64(s.ObserverErrors)},
		{"claude_mem_go_worker_insert_errors_total", "Observations produced but the DB insert failed.", "counter", float64(s.InsertErrors)},
		{"claude_mem_go_worker_embed_errors_total", "Inserts that succeeded but embedding failed.", "counter", float64(s.EmbedErrors)},
		{"claude_mem_go_worker_cached_sessions", "Observer sessions currently cached.", "gauge", float64(s.CachedSessions)},
		{"claude_mem_go_worker_pool_in_flight", "Pool slots currently held.", "gauge", float64(s.PoolInFlight)},
		{"claude_mem_go_worker_pool_capacity", "Pool's maximum concurrent slots.", "gauge", float64(s.PoolCapacity)},
	}
	// Identity as a labelled _info gauge, the idiomatic Prometheus way to
	// expose "which thing is this". The store matters specifically: the
	// daemon opens it once at start and never re-reads $CLAUDE_MEM_DB, so
	// a worker started before that variable changed keeps writing to the
	// old store while everything else resolves the new one — reproduced
	// end to end, with `doctor` reporting every check green and the
	// observation landing in a different file. Already redacted upstream
	// (see Daemon.Stats), because a Postgres DSN carries a password and
	// this endpoint is plain HTTP.
	if s.Store != "" {
		if _, err := fmt.Fprintf(w,
			"# HELP claude_mem_go_worker_info Daemon identity; the store label is the database it writes to.\n"+
				"# TYPE claude_mem_go_worker_info gauge\n"+
				"claude_mem_go_worker_info{store=%q} 1\n", s.Store); err != nil {
			return err
		}
	}

	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %g\n", l.name, l.help, l.name, l.typ, l.name, l.value); err != nil {
			return err
		}
	}
	return nil
}

// MetricsHandler returns an http.Handler serving d.Stats() in Prometheus
// text format — the live equivalent of WriteMetrics(w, d.Stats()), for
// wiring into an *http.ServeMux (see Daemon.Run's optional metrics
// listener).
func MetricsHandler(d *Daemon) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := WriteMetrics(w, d.Stats()); err != nil {
			d.Log.Printf("FAILED writing metrics response: %v", err)
		}
	})
}
