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
	# The Stop hook is detached rather than exec'd, because otherwise its
	# work never finishes in a headless session.
	#
	# Measured, not assumed: a probe plugin whose Stop hook merely slept
	# and then wrote a file produced NOTHING after `claude -p` exited —
	# not at 25 seconds, and not even at 1. Claude Code tears the hook
	# process down when the session ends, and `-p` sessions end the moment
	# the answer is printed. The session summary needs both a settle wait
	# and a real model call, so it never survived: two full soak sessions
	# captured observations but produced no summary at all, and every
	# `-p`-based verification in this project has silently been missing
	# them.
	#
	# The same probe showed backgrounded work DOES outlive the session, so
	# the fix is to background it. That matches what this hook already is:
	# hooks.json marks it async, nothing reads its stdout, and its own doc
	# comment calls it fire-and-forget. The work stays bounded by the wait
	# budget it already had, so this detaches a finite job, not an
	# unbounded one.
	#
	# Only `stop`. PostToolUse forwards to the daemon and exits in
	# milliseconds — the long work there happens inside the worker, which
	# is already a detached process.
	if [ "$cmd" = "stop" ]; then
		# Read the payload BEFORE detaching. The hook's input arrives on
		# stdin, which is a pipe from Claude Code that dies with the
		# session — so a detached child inheriting it races the teardown,
		# and pointing it at /dev/null instead just makes it read EOF.
		# Both were observed: the first attempt at this detached with
		# </dev/null and the daemon logged "FAILED parsing hook payload:
		# EOF" every time. Capturing here, while the parent is still
		# alive, and replaying it into the child is what makes the
		# detachment safe.
		payload=$(cat)
		( printf '%s' "$payload" | "$bin" "$@" >/dev/null 2>&1 & ) &
		exit 0
	fi
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
