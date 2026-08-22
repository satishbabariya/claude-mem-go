package hook

import "testing"

// TestParseRecallReport covers the discrimination this protocol depends
// on: every message kind shares one socket, so a parser that accepts
// something it shouldn't would route a hook payload into a counter.
func TestParseRecallReport(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   int
		wantOK bool
	}{
		{"a real report", "RECALL 7", 7, true},
		{"zero results is the case that matters", "RECALL 0", 0, true},
		// A hook payload is always JSON, which is exactly why the prefix
		// convention works — but assert it rather than trusting it.
		{"a hook payload is not a report", `{"session_id":"s1"}`, 0, false},
		{"another message kind is not a report", "DEDUPE s1 abc", 0, false},
		{"the in-flight query is not a report", "INFLIGHT s1", 0, false},
		{"no count", "RECALL ", 0, false},
		{"not a number", "RECALL many", 0, false},
		// Negative counts cannot come from len(), so one means a corrupt
		// or hostile sender; counting it would corrupt the metric.
		{"negative is rejected", "RECALL -3", 0, false},
		{"trailing junk is rejected", "RECALL 3 4", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRecallReport([]byte(tc.raw))
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("ParseRecallReport(%q) = (%d, %v), want (%d, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
