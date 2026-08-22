package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseLevelAcceptsTheNamesRealClaudeMemUses(t *testing.T) {
	cases := map[string]Level{
		"DEBUG": Debug, "debug": Debug,
		"INFO": Info, "Info": Info,
		"WARN": Warn, "WARNING": Warn,
		"ERROR": Error, "SILENT": Silent,
		"  info  ": Info,
	}
	for in, want := range cases {
		got, ok := ParseLevel(in)
		if !ok || got != want {
			t.Errorf("ParseLevel(%q) = %v/%v, want %v/true", in, got, ok, want)
		}
	}
}

// TestParseLevelFallsBackRatherThanSilencing is the safety property that
// matters most here. A typo in $CLAUDE_MEM_LOG_LEVEL must not turn the
// logs off — that would be a configuration mistake which hides its own
// evidence, and the logs are the only diagnosis these fire-and-forget
// hooks have.
func TestParseLevelFallsBackRatherThanSilencing(t *testing.T) {
	for _, in := range []string{"", "banana", "TRACE", "3"} {
		got, ok := ParseLevel(in)
		if ok {
			t.Errorf("ParseLevel(%q) reported ok, want a rejected value", in)
		}
		if got != DefaultLevel {
			t.Errorf("ParseLevel(%q) = %v, want the %v default", in, got, DefaultLevel)
		}
		if got == Silent {
			t.Fatalf("ParseLevel(%q) silenced logging — a typo must never hide its own evidence", in)
		}
	}
}

func TestLoggerFiltersBelowItsLevel(t *testing.T) {
	var buf bytes.Buffer
	l := NewAtLevel(&buf, "", 0, Info)

	l.Debugf("routine chatter")
	l.Infof("ordinary")
	l.Warnf("odd")
	l.Errorf("broken")

	out := buf.String()
	if strings.Contains(out, "routine chatter") {
		t.Fatalf("a Debug message survived an Info threshold:\n%s", out)
	}
	for _, want := range []string{"INFO ordinary", "WARN odd", "ERROR broken"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestPrintfIsInfoNotUnfilterable pins a decision that is easy to get
// backwards. Every pre-existing call site in this project uses Printf and
// predates levels; if Printf bypassed the filter it would outrank Errorf
// by being unsuppressable, inverting the hierarchy this package exists to
// create.
func TestPrintfIsInfoNotUnfilterable(t *testing.T) {
	var buf bytes.Buffer
	l := NewAtLevel(&buf, "", 0, Error)
	l.Printf("legacy call site")
	if strings.Contains(buf.String(), "legacy call site") {
		t.Fatalf("Printf bypassed an Error threshold — it must mean Info:\n%s", buf.String())
	}

	buf.Reset()
	l2 := NewAtLevel(&buf, "", 0, Info)
	l2.Printf("legacy call site")
	if !strings.Contains(buf.String(), "INFO legacy call site") {
		t.Fatalf("Printf did not log at Info:\n%s", buf.String())
	}
}

func TestSilentSuppressesEverythingIncludingErrors(t *testing.T) {
	var buf bytes.Buffer
	l := NewAtLevel(&buf, "", 0, Silent)
	l.Debugf("d")
	l.Infof("i")
	l.Warnf("w")
	l.Errorf("e")
	if buf.Len() != 0 {
		t.Fatalf("SILENT still emitted:\n%s", buf.String())
	}
}

// TestLevelIsAPlainLeadingWord keeps `grep FAILED` — the habit this
// project's logs were already read with — working unchanged, while making
// `grep ERROR` work too.
func TestLevelIsAPlainLeadingWord(t *testing.T) {
	var buf bytes.Buffer
	l := NewAtLevel(&buf, "", 0, Debug)
	l.Errorf("FAILED opening store at %s", "/tmp/x.db")
	out := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(out, "ERROR FAILED opening store") {
		t.Fatalf("got %q, want the level as a bare leading word followed by the original message", out)
	}
}

// TestNilSafety matters because loggers here are built from a file that
// may fail to open; a logging call must never be the thing that panics a
// hook.
func TestNilSafety(t *testing.T) {
	var l *Logger
	l.Errorf("must not panic")
	l2 := &Logger{}
	l2.Errorf("must not panic either")
}

func TestLevelFromEnvReadsTheSharedVariableName(t *testing.T) {
	t.Setenv(LevelEnvVar, "ERROR")
	if got := LevelFromEnv(); got != Error {
		t.Fatalf("LevelFromEnv() = %v, want Error from $%s", got, LevelEnvVar)
	}
	t.Setenv(LevelEnvVar, "")
	if got := LevelFromEnv(); got != DefaultLevel {
		t.Fatalf("LevelFromEnv() with the var unset = %v, want %v", got, DefaultLevel)
	}
}
