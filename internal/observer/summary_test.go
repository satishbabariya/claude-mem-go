package observer

import (
	"strings"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/store"
)

func TestBuildSummaryPromptIncludesEveryObservationInOrder(t *testing.T) {
	observations := []store.SearchResult{
		{Observation: store.Observation{Type: "discovery", Title: "Found the bug", Subtitle: "in the parser"}},
		{Observation: store.Observation{Type: "change", Title: "Fixed the parser"}},
	}
	prompt := BuildSummaryPrompt(SelectSummaryWindow(observations, 0))

	if !strings.Contains(prompt, "Found the bug") || !strings.Contains(prompt, "in the parser") {
		t.Error("prompt does not mention the first observation's title/subtitle")
	}
	if !strings.Contains(prompt, "Fixed the parser") {
		t.Error("prompt does not mention the second observation's title")
	}
	if strings.Index(prompt, "Found the bug") > strings.Index(prompt, "Fixed the parser") {
		t.Error("observations appear out of order in the prompt")
	}
	if !strings.Contains(prompt, "<type>summary</type>") {
		t.Error("prompt does not ask for type=summary — would produce an observation indistinguishable from a per-tool-call one")
	}
}

func TestBuildSummaryPromptHandlesNoSubtitle(t *testing.T) {
	observations := []store.SearchResult{
		{Observation: store.Observation{Type: "discovery", Title: "Just a title"}},
	}
	prompt := BuildSummaryPrompt(SelectSummaryWindow(observations, 0))
	if strings.Contains(prompt, "Just a title — ") {
		t.Error("prompt should not print a dangling separator for an empty subtitle")
	}
}

func obsN(n int) []store.SearchResult {
	out := make([]store.SearchResult, n)
	for i := range out {
		out[i] = store.SearchResult{Observation: store.Observation{
			Type: "change", Title: fmtTitle(i + 1)}}
	}
	return out
}

func fmtTitle(n int) string { return "step " + itoa(n) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// TestSelectSummaryWindowKeepsTheEndOfTheSession is the regression test
// for the real defect. BySessionID orders oldest-first, so the old plain
// LIMIT kept the FIRST N observations and silently dropped the rest — the
// end of the session, which is where its conclusions are. Reproduced
// against a real 150-observation session at the default cap of 50: the
// summary described routine early edits and contained no trace of the
// decision recorded 50 times in the tail.
func TestSelectSummaryWindowKeepsTheEndOfTheSession(t *testing.T) {
	w := SelectSummaryWindow(obsN(150), 50)

	if w.Total != 150 {
		t.Fatalf("Total = %d, want the TRUE session size 150 — the prompt depends on this to avoid claiming the fragment is the whole session", w.Total)
	}
	if len(w.Observations) != 50 {
		t.Fatalf("window size = %d, want 50", len(w.Observations))
	}
	// The last observation of the session must survive. Under the old
	// behaviour this was the single most important thing that did not.
	last := w.Observations[len(w.Observations)-1]
	if last.Observation.Title != "step 150" {
		t.Fatalf("last windowed observation = %q, want %q — the end of the session is exactly what a summary must not lose",
			last.Observation.Title, "step 150")
	}
	if w.Observations[0].Observation.Title != "step 1" {
		t.Fatalf("first windowed observation = %q, want %q — the opening says what the session set out to do",
			w.Observations[0].Observation.Title, "step 1")
	}
	if got := w.Positions[len(w.Positions)-1]; got != 150 {
		t.Fatalf("last position = %d, want 150 (true position, not window index)", got)
	}
}

func TestSelectSummaryWindowLeavesAShortSessionUntouched(t *testing.T) {
	w := SelectSummaryWindow(obsN(7), 50)
	if len(w.Observations) != 7 || w.Total != 7 {
		t.Fatalf("short session was altered: shown=%d total=%d, want 7/7", len(w.Observations), w.Total)
	}
	for i, p := range w.Positions {
		if p != i+1 {
			t.Fatalf("Positions[%d] = %d, want %d", i, p, i+1)
		}
	}
}

// TestBuildSummaryPromptDeclaresTruncation covers the half of the bug that
// made the summary actively wrong rather than merely incomplete. The old
// prompt opened "Here are the observations recorded during this session" —
// an unqualified claim that the slice was the whole session — and the
// model duly asserted "an extensive series of 50 sequential edits" for a
// 150-observation session.
func TestBuildSummaryPromptDeclaresTruncation(t *testing.T) {
	prompt := BuildSummaryPrompt(SelectSummaryWindow(obsN(150), 50))

	if !strings.Contains(prompt, "150 observations") {
		t.Fatalf("prompt does not tell the model the session's true size; it will describe the fragment as the whole.\n%s", prompt[:400])
	}
	if !strings.Contains(prompt, "omitted") {
		t.Fatalf("prompt does not say anything was omitted.\n%s", prompt[:400])
	}
	if strings.Contains(prompt, "Here are the observations recorded during this session, in order:") {
		t.Fatal("truncated prompt still uses the unqualified whole-session wording — that phrasing is what licensed the false count")
	}
	// The elision marker must appear between head and tail so the model
	// can see WHERE the gap is, not just that one exists.
	if !strings.Contains(prompt, "observations omitted …") {
		t.Fatalf("no inline elision marker between the head and tail blocks.\n%s", prompt[:600])
	}
	// True positions, not 1..50.
	if !strings.Contains(prompt, "150. [change] step 150") {
		t.Fatalf("prompt does not number observations by their true session position.\n%s", prompt[:600])
	}
}

// TestBuildSummaryPromptStaysQuietWhenNothingWasDropped is the
// counterweight: a warning on every session would train the model to
// hedge summaries that are in fact complete.
func TestBuildSummaryPromptStaysQuietWhenNothingWasDropped(t *testing.T) {
	prompt := BuildSummaryPrompt(SelectSummaryWindow(obsN(5), 50))
	if strings.Contains(prompt, "omitted") || strings.Contains(prompt, "You are being shown") {
		t.Fatalf("a complete session's prompt claims truncation:\n%s", prompt[:400])
	}
}
