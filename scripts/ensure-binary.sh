#!/usr/bin/env bash
# Ensure $CLAUDE_PLUGIN_ROOT/claude-mem-go exists and runs, building it
# from the source the plugin already ships if it does not.
#
# WHY THIS EXISTS
#
# hooks.json resolves every hook to "$CLAUDE_PLUGIN_ROOT/claude-mem-go",
# and that binary is gitignored — it is a build artifact, not source. A
# plugin installed from a git source therefore ships the complete Go
# source and no binary, so every hook fails with "No such file or
# directory" and nothing is ever captured.
#
# The failure is quiet in the worst way: `doctor` is a subcommand of the
# very binary that is missing, so the tool an operator would reach for to
# diagnose it cannot run either. Verified by unpacking `git archive HEAD`
# and invoking a hook against it.
#
# Runs at Setup rather than on the SessionStart hot path, matching real
# claude-mem's own Setup hook (which is there to `bun install` its
# runtime dependencies for the same class of reason): Setup fires once
# per Claude Code launch and allows a much longer timeout, so a one-time
# build does not land on the user's first prompt.
#
# Measured on this project: a genuinely cold build (empty module and
# build caches, dependencies downloaded) takes ~33s; a warm rebuild takes
# ~0.7s; and the common case — a binary that is already present and
# working — costs one `version` invocation and exits immediately.
set -u

root="${CLAUDE_PLUGIN_ROOT:-}"
if [ -z "$root" ]; then
	# Not running as an installed plugin. Nothing to ensure, and guessing
	# at a location would be worse than doing nothing.
	exit 0
fi

bin="$root/claude-mem-go"

# The same liveness test plugincheck.BinaryStatus uses, and for the same
# reason: a binary built for the wrong architecture stats perfectly and
# only fails when executed, so existence alone proves nothing.
if [ -x "$bin" ] && "$bin" version >/dev/null 2>&1; then
	exit 0
fi

if ! command -v go >/dev/null 2>&1; then
	echo "claude-mem-go: $bin is missing or not runnable, and Go is not installed to build it." >&2
	echo "claude-mem-go: install Go and restart Claude Code, or build it yourself:" >&2
	echo "claude-mem-go:   (cd \"$root\" && go build -o claude-mem-go ./cmd/claude-mem-go)" >&2
	# Exit 0 deliberately. Memory degrades to "nothing is captured", which
	# is bad — but blocking the user's session over it would be worse, and
	# `doctor` already reports an unusable plugin binary as a critical
	# failure once a working binary exists to run it.
	exit 0
fi

echo "claude-mem-go: building $bin (first run after install; ~30s cold, then cached)…" >&2
if (cd "$root" && go build -o claude-mem-go ./cmd/claude-mem-go) >&2; then
	echo "claude-mem-go: built successfully." >&2
else
	echo "claude-mem-go: build FAILED — hooks will not fire until this is resolved." >&2
fi
exit 0
