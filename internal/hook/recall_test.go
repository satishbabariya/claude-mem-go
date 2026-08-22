package hook

import "testing"

// TestParseRecallReport covers the discrimination this protocol depends
// on: every message kind shares one socket, so a parser that accepts
// something it shouldn't would route a hook payload into a counter.
func TestParseRecallReport(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSrc RecallSource
		want    int
		wantOK  bool
	}{
		{"a prompt report", "RECALL prompt 7", RecallPrompt, 7, true},
		{"a session report", "RECALL session 3", RecallSession, 3, true},
		{"a file report", "RECALL file 1", RecallFile, 1, true},
		{"zero results is the case that matters", "RECALL session 0", RecallSession, 0, true},
		// An unknown source must be rejected rather than folded into a
		// bucket it does not belong to — a miscounted source is worse
		// than an uncounted one, because it silently corrupts the rate
		// the doctor warning is judged on.
		{"unknown source rejected", "RECALL mcp 4", "", 0, false},
		{"legacy sourceless format rejected", "RECALL 7", "", 0, false},
		// A hook payload is always JSON, which is exactly why the prefix
		// convention works — but assert it rather than trusting it.
		{"a hook payload is not a report", `{"session_id":"s1"}`, "", 0, false},
		{"another message kind is not a report", "DEDUPE s1 abc", "", 0, false},
		{"the in-flight query is not a report", "INFLIGHT s1", "", 0, false},
		{"no count", "RECALL prompt", "", 0, false},
		{"not a number", "RECALL prompt many", "", 0, false},
		// Negative counts cannot come from len(), so one means a corrupt
		// or hostile sender; counting it would corrupt the metric.
		{"negative is rejected", "RECALL prompt -3", "", 0, false},
		{"trailing junk is rejected", "RECALL prompt 3 4", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, got, ok := ParseRecallReport([]byte(tc.raw))
			if ok != tc.wantOK || got != tc.want || src != tc.wantSrc {
				t.Fatalf("ParseRecallReport(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.raw, src, got, ok, tc.wantSrc, tc.want, tc.wantOK)
			}
		})
	}
}
