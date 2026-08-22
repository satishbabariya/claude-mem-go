// Package logging adds severity levels to the plain *log.Logger this
// project has used everywhere, without changing how any existing call
// site is written.
//
// Real claude-mem has had leveled logging from the start —
// src/utils/logger.ts defines LogLevel{DEBUG,INFO,WARN,ERROR,SILENT} and
// reads CLAUDE_MEM_LOG_LEVEL — and this port had no level concept at all
// across 97 log call sites. Everything was one implicit severity, so
// there was no way to quiet routine chatter or to ask for more detail,
// and nothing distinguished "this is fine" from "this failed" except an
// informal prefix convention.
//
// That convention is real and was measured before this was written: of
// the call sites, 23 already began "FAILED", 16 "skip:", 7 "no ". And
// the ratio it hides is the actual cost — hook.log in a real install held
// 76 routine "forwarded N bytes" lines against 2 genuine failures. The
// information to filter on was present; it just was not machine-readable.
//
// Deliberately NOT structured JSON. Real claude-mem's logger emits
// leveled text, these files are read directly by humans (and by `doctor`,
// which quotes them), and switching format would break every existing
// habit for no parity gain.
package logging

import (
	"io"
	"log"
	"os"
	"strings"
)

// Level mirrors real claude-mem's LogLevel exactly, including SILENT, so
// operators moving between the two configure the same values.
type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
	Silent
)

// LevelEnvVar is the same name real claude-mem reads, so a single
// setting configures either implementation.
const LevelEnvVar = "CLAUDE_MEM_LOG_LEVEL"

// DefaultLevel is Info, matching real claude-mem's own default.
const DefaultLevel = Info

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Info:
		return "INFO"
	case Warn:
		return "WARN"
	case Error:
		return "ERROR"
	case Silent:
		return "SILENT"
	}
	return "INFO"
}

// ParseLevel maps a name to a Level, case-insensitively. An unrecognized
// or empty value yields DefaultLevel and ok=false — the caller decides
// whether that is worth reporting, and logging a complaint about the log
// level from inside the logger is its own trap.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return Debug, true
	case "INFO":
		return Info, true
	case "WARN", "WARNING":
		return Warn, true
	case "ERROR":
		return Error, true
	case "SILENT", "OFF", "NONE":
		return Silent, true
	}
	return DefaultLevel, false
}

// LevelFromEnv reads $CLAUDE_MEM_LOG_LEVEL, falling back to DefaultLevel.
func LevelFromEnv() Level {
	lvl, _ := ParseLevel(os.Getenv(LevelEnvVar))
	return lvl
}

// Logger is a *log.Logger with a level.
//
// It EMBEDS rather than wraps, so every existing `l.Printf(...)` call
// site keeps compiling untouched — 97 of them — and picks up level
// filtering for free. Printf is deliberately overridden below to mean
// Info rather than "always print": an unclassified message is ordinary
// operational output, which is exactly what Info is for.
type Logger struct {
	*log.Logger
	level Level
}

// New matches log.New's signature so the swap is mechanical at every
// construction site, including tests.
func New(w io.Writer, prefix string, flag int) *Logger {
	return &Logger{Logger: log.New(w, prefix, flag), level: LevelFromEnv()}
}

// NewAtLevel is New with an explicit level, for tests and for callers
// that get their level from somewhere other than the environment.
func NewAtLevel(w io.Writer, prefix string, flag int, lvl Level) *Logger {
	return &Logger{Logger: log.New(w, prefix, flag), level: lvl}
}

// Level reports the threshold this logger is filtering at.
func (l *Logger) Level() Level { return l.level }

// SetLevel changes the threshold. Not concurrency-safe against active
// logging, and not meant to be: it exists for construction-time wiring
// and tests, not for flipping levels on a live daemon.
func (l *Logger) SetLevel(lvl Level) { l.level = lvl }

func (l *Logger) logAt(lvl Level, format string, args ...any) {
	if l == nil || l.Logger == nil || lvl < l.level || l.level == Silent {
		return
	}
	// The level is emitted as a bare leading word so existing habits keep
	// working: `grep FAILED` still matches, and `grep ERROR` now does too.
	l.Logger.Printf(lvl.String()+" "+format, args...)
}

// Debugf is for output that is routine and high-frequency enough to be
// noise by default — the per-tool-call chatter that buried real failures
// at roughly 38 to 1 in a real install's hook.log.
func (l *Logger) Debugf(format string, args ...any) { l.logAt(Debug, format, args...) }

// Infof is ordinary operational output, and what an unclassified Printf
// means.
func (l *Logger) Infof(format string, args ...any) { l.logAt(Info, format, args...) }

// Warnf is for something unexpected that did not stop the operation.
func (l *Logger) Warnf(format string, args ...any) { l.logAt(Warn, format, args...) }

// Errorf is for an operation that failed.
func (l *Logger) Errorf(format string, args ...any) { l.logAt(Error, format, args...) }

// Printf overrides the embedded method so pre-existing call sites are
// treated as Info rather than bypassing the level filter entirely. Every
// one of them predates levels and is ordinary operational output; having
// them silently outrank Errorf by being unfilterable would invert the
// hierarchy this package exists to create.
func (l *Logger) Printf(format string, args ...any) { l.logAt(Info, format, args...) }
