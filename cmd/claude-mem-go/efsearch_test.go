package main

import (
	"strings"
	"testing"
)

// TestEfSearchRecallWarning covers the three states that matter, because
// the failure mode of a warning like this is crying wolf, not silence:
// a store big enough for the ANN approximation to bite AND running
// pgvector's default should warn, and the other two combinations must
// stay quiet.
func TestEfSearchRecallWarning(t *testing.T) {
	defaultDetails := map[string]string{"hnsw_ef_search": "40"}
	overridden := map[string]string{"hnsw_ef_search": "200"}

	cases := []struct {
		name     string
		details  map[string]string
		embedded int
		want     bool
	}{
		{"large store lowered to 40 warns", defaultDetails, efSearchRecallFloor, true},
		{"well past the floor warns", defaultDetails, 10 * efSearchRecallFloor, true},
		// Below the floor the planner still prefers an exact sequential
		// scan, so ef_search changes nothing and warning would be noise.
		{"small store stays quiet", defaultDetails, efSearchRecallFloor - 1, false},
		// The operator already made a choice; re-litigating it every
		// doctor run is exactly the nagging this must not do.
		{"configured override stays quiet", overridden, 10 * efSearchRecallFloor, false},
		// SQLite has no such GUC and reports no such detail.
		{"non-postgres store stays quiet", map[string]string{}, 10 * efSearchRecallFloor, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				reportEfSearchRecall(tc.details, tc.embedded)
			})
			got := strings.Contains(out, "ef_search")
			if got != tc.want {
				t.Fatalf("warned = %v, want %v\noutput: %q", got, tc.want, out)
			}
			if tc.want {
				// The whole point is that the operator learns what to DO,
				// so the remediation flag must actually appear.
				if !strings.Contains(out, "-hnsw-ef-search") {
					t.Errorf("warning omits the -hnsw-ef-search remediation flag:\n%s", out)
				}
				// A warning quoting a recall number nobody measured would
				// be worse than none; these come from bench/recall.
				if !strings.Contains(out, "80%") {
					t.Errorf("warning omits the measured default recall figure:\n%s", out)
				}
			}
		})
	}
}
