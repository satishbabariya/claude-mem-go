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
		return fmt.Sprintf("claude-mem-go (unknown build — no vcs.revision recorded, %s)", info.GoVersion)
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
