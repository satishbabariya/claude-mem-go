#!/usr/bin/env bash
# Wrapper every hook in hooks.json goes through: exec the plugin binary
# when it is usable, and make its absence VISIBLE when it is not.
#
# WHY A WRAPPER
#
# The binary is gitignored — it is a build artifact, not source — so a
# plugin installed from a git source ships the complete Go source and no
# binary. Every hook then fails with "No such file or directory", and
# that error goes nowhere the user will ever look: Claude Code swallows a
# failed hook command, so memory silently captures nothing forever. Worse,
# `doctor` is a subcommand of the very binary that is missing, so the tool
# an operator would reach for cannot run either.
#
# A Setup hook is the natural home for fixing this (see ensure-binary.sh,
# which does exactly that and is wired up), but it cannot be the whole
# answer here: Setup was observed NOT firing for an installed plugin in
# `claude -p`, nor during `claude plugin install`. So the wrapper covers
# the case regardless of whether Setup ever ran.
#
# It deliberately does NOT build. SessionStart's hooks allow 10-15s and a
# cold build of this project measures ~33s, so building here would just
# time out — the same "wrong architectural home" reasoning real
# claude-mem records for moving its own install step off the SessionStart
# path and onto Setup.
#
# Cost on the healthy path is one bash startup plus an exec, on hooks
# that already open a database or spawn a model call.
set -u

root="${CLAUDE_PLUGIN_ROOT:-}"
bin="$root/claude-mem-go"
cmd="${1:-}"

if [ -n "$root" ] && [ -x "$bin" ]; then
	exec "$bin" "$@"
fi

# Degraded path: the binary is missing or not executable.
#
# SessionStart's `context` hook is the one place a hook can put text in
# front of the user, via hookSpecificOutput.additionalContext. Use it, so
# "memory is silently dead" becomes something the session actually says
# rather than something only a log knows.
if [ "$cmd" = "context" ]; then
	cat <<-JSON
		{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"claude-mem-go is installed but its binary is missing, so no memory is being captured or recalled this session. Tell the user to run: (cd '$root' && go build -o claude-mem-go ./cmd/claude-mem-go) and restart Claude Code."}}
	JSON
	exit 0
fi

# Every other hook: leave a diagnosis where `doctor`-less debugging can
# still find it, and exit 0 so a broken install degrades to "no memory"
# rather than blocking the user's tool calls.
logdir="${HOME:-/tmp}/.claude-mem-go"
mkdir -p "$logdir" 2>/dev/null
{
	printf '%s hook %q could not run: %s is missing or not executable.\n' \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$cmd" "$bin"
	printf '  fix: (cd %q && go build -o claude-mem-go ./cmd/claude-mem-go)\n' "$root"
} >>"$logdir/missing-binary.log" 2>/dev/null
exit 0
