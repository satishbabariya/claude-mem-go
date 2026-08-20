package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func buildInfoWith(settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{GoVersion: "go1.25.0", Settings: settings}
}

func TestBuildVersionStringWithFullVCSInfo(t *testing.T) {
	info := buildInfoWith(
		debug.BuildSetting{Key: "vcs", Value: "git"},
		debug.BuildSetting{Key: "vcs.revision", Value: "6b386b0e4a6e26b5c25fb77c85283d5098723774"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-08-20T15:04:15Z"},
		debug.BuildSetting{Key: "vcs.modified", Value: "false"},
	)
	got := buildVersionString(info)
	if !strings.Contains(got, "6b386b0e4a6e") {
		t.Errorf("got %q, want it to contain the 12-char short revision", got)
	}
	if strings.Contains(got, "-dirty") {
		t.Errorf("got %q, want no -dirty marker when vcs.modified=false", got)
	}
	if !strings.Contains(got, "2026-08-20T15:04:15Z") {
		t.Errorf("got %q, want the build time", got)
	}
	if !strings.Contains(got, "go1.25.0") {
		t.Errorf("got %q, want the Go version", got)
	}
}

func TestBuildVersionStringMarksModifiedTreeAsDirty(t *testing.T) {
	info := buildInfoWith(
		debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567890"},
		debug.BuildSetting{Key: "vcs.modified", Value: "true"},
	)
	got := buildVersionString(info)
	if !strings.Contains(got, "-dirty") {
		t.Errorf("got %q, want a -dirty marker when vcs.modified=true", got)
	}
}

func TestBuildVersionStringTruncatesLongRevision(t *testing.T) {
	info := buildInfoWith(
		debug.BuildSetting{Key: "vcs.revision", Value: "abcdef1234567890abcdef1234567890"},
	)
	got := buildVersionString(info)
	if strings.Contains(got, "abcdef1234567890abcdef1234567890") {
		t.Errorf("got %q, want the full 32-char revision truncated to a short form", got)
	}
	if !strings.Contains(got, "abcdef123456") {
		t.Errorf("got %q, want it to contain the 12-char prefix of the revision", got)
	}
}

func TestBuildVersionStringHandlesMissingBuildInfo(t *testing.T) {
	got := buildVersionString(nil)
	if !strings.Contains(got, "claude-mem-go") {
		t.Errorf("got %q, want it to still identify itself even with no build info", got)
	}
}

func TestBuildVersionStringHandlesNoVCSRevision(t *testing.T) {
	// A binary built with -trimpath or outside a VCS checkout has build
	// info but no vcs.revision — must degrade gracefully, not panic or
	// print an empty revision.
	got := buildVersionString(buildInfoWith())
	if !strings.Contains(got, "claude-mem-go") || !strings.Contains(got, "go1.25.0") {
		t.Errorf("got %q, want it to still report the Go version without a revision", got)
	}
}
