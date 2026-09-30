package main

import (
	"fmt"
	"runtime/debug"
)

// buildVersionString formats build info the way `version` and `doctor`
// both print it. It's a pure function of *debug.BuildInfo, not something
// that reads build info itself, so it's testable without depending on how
// THIS test binary happens to have been built — `go test`'s own binary may
// or may not carry real vcs.* settings depending on how it was invoked,
// but buildVersionString's formatting logic doesn't care either way.
//
// There was no way to answer "what build is this" at all before this —
// no version flag, no commit correlation for a bug report. Go's own
// runtime/debug.ReadBuildInfo already carries the exact git commit (and
// whether the tree was dirty at build time) for anything built the normal
// way (`go build`, VCS stamping on by default since Go 1.18) — no ldflags
// wiring, no CI changes, no version file to keep in sync needed.
func buildVersionString(info *debug.BuildInfo) string {
	if info == nil {
		return "claude-mem-go (unknown build — no Go build info available)"
	}

	var revision, vcsTime string
	modified := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			vcsTime = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}

	if revision == "" {
		// The common way to land here: `go install .../cmd/claude-mem-go@latest`.
		// The module proxy hands the Go toolchain a bare module snapshot with
		// no local .git for it to stamp vcs.* settings from, so this is not a
		// bug in THIS build — it is an inherent gap in that install path. Say
		// so explicitly, and name the two install paths that do carry a real
		// stamp, rather than leaving an operator to guess why `doctor`'s
		// version line is unusable for staleness comparison.
		return fmt.Sprintf("claude-mem-go (unknown build — no VCS revision recorded; "+
			"this usually means the binary was installed via `go install .../@latest`, "+
			"which has no local .git to stamp — build from a git checkout with `go build` "+
			"or use a release binary for real version info, %s)", info.GoVersion)
	}

	short := revision
	if len(short) > 12 {
		short = short[:12]
	}
	dirty := ""
	if modified {
		dirty = "-dirty"
	}

	if vcsTime != "" {
		return fmt.Sprintf("claude-mem-go %s%s (built %s, %s)", short, dirty, vcsTime, info.GoVersion)
	}
	return fmt.Sprintf("claude-mem-go %s%s (%s)", short, dirty, info.GoVersion)
}

func cmdVersion(args []string) int {
	info, _ := debug.ReadBuildInfo()
	fmt.Println(buildVersionString(info))
	return 0
}

// currentBuildVersion is this binary's build string, used wherever one
// component needs to compare itself against another — notably the worker
// daemon, which is long-lived and can end up running code older than
// every short-lived hook process around it.
func currentBuildVersion() string {
	info, _ := debug.ReadBuildInfo()
	return buildVersionString(info)
}
