package logging

import (
	"bytes"
	"strings"
	"testing"
)

// TestWarnIsFilterableBetweenInfoAndError pins the level that gives the
// three-way distinction its point.
//
// This project's logging started with one implicit severity, then gained
// levels by reclassifying every message beginning "FAILED" to Errorf.
// That mechanical pass left WARN essentially unused and misfiled a whole
// class of message: failures worded in lower case ("embedding failed for
// observations.id=…", "saving embedding … failed") stayed at INFO,
// alongside genuinely routine output, even though something had gone
// wrong. Meanwhile they are NOT errors — the observation was stored, only
// its embedding was lost, which the code itself documents as "additive
// only".
//
// Nine such sites now log at Warn, so an operator can ask three different
// questions: what broke (ERROR), what degraded (WARN), and what happened
// (INFO).
func TestWarnIsFilterableBetweenInfoAndError(t *testing.T) {
	emit := func(l *Logger) {
		l.Infof("persisted session summary")
		l.Warnf("embedding failed for observations.id=7 (semantic search won't find it)")
		l.Errorf("FAILED opening store")
	}

	var atWarn bytes.Buffer
	emit(NewAtLevel(&atWarn, "", 0, Warn))
	out := atWarn.String()
	if strings.Contains(out, "persisted session summary") {
		t.Fatalf("routine INFO output survived a WARN threshold:\n%s", out)
	}
	if !strings.Contains(out, "WARN embedding failed") {
		t.Fatalf("a degradation was filtered out at WARN, which is the level it exists for:\n%s", out)
	}
	if !strings.Contains(out, "ERROR FAILED opening store") {
		t.Fatalf("an error was filtered out at WARN:\n%s", out)
	}

	// At ERROR, the degradation must be gone but the failure must remain —
	// that separation is the whole reason these are not both Errorf.
	var atError bytes.Buffer
	emit(NewAtLevel(&atError, "", 0, Error))
	out = atError.String()
	if strings.Contains(out, "embedding failed") {
		t.Fatalf("a WARN survived an ERROR threshold:\n%s", out)
	}
	if !strings.Contains(out, "FAILED opening store") {
		t.Fatalf("the real error was lost at ERROR level:\n%s", out)
	}
}
