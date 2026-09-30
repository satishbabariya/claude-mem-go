# Installing as a Claude Code plugin

Installing the plugin is what makes capture and recall automatic: it wires the
hooks, the MCP server, and the `/mem-*` skills. The CLI and MCP server also
work without it. Release binaries are produced as described in
[development.md](development.md#releases).

## Contents

- [Install and uninstall](#install-and-uninstall)
- [Manual install from a release binary](#manual-install-from-a-release-binary)
- [What gets wired](#what-gets-wired)
- [The self-healing binary](#the-self-healing-binary)
- [Verifying an install](#verifying-an-install)
- [Troubleshooting](#troubleshooting)

## Install and uninstall

```sh
go build -o claude-mem-go ./cmd/claude-mem-go
claude plugin validate .                                          # manifest sanity check

# From inside a project you want claude-mem-go active in:
claude plugin marketplace add /path/to/claude-mem-go --scope project
claude plugin install claude-mem-go@claude-mem-go-local --scope project
```

Use `--scope project` while evaluating: a user-scope install affects every
Claude Code session on the machine. Restart Claude Code after installing so
`Setup` and `SessionStart` fire. To remove it:

```sh
claude plugin uninstall claude-mem-go@claude-mem-go-local --scope project
claude plugin marketplace remove claude-mem-go-local
```

## Manual install from a release binary

No Go toolchain? Every tagged release publishes a checksummed binary for each
[supported platform](../README.md#requirements) — pick the one matching your
`uname -s`/`uname -m`, verify it against the release's `checksums.txt`, and
use it in place of `go build -o claude-mem-go ./cmd/claude-mem-go` above (or
standalone, without the plugin, for just the CLI and MCP server):

```sh
# Map uname to goreleaser's naming: darwin/linux, amd64/arm64.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
version=0.4.3   # the release tag without its leading "v" — check the Releases page for the latest

base="https://github.com/satishbabariya/claude-mem-go/releases/download/v${version}"
curl -sLO "${base}/claude-mem-go_${version}_${os}_${arch}.tar.gz"
curl -sLO "${base}/checksums.txt"

# Verify before extracting — never run an unverified download.
grep "claude-mem-go_${version}_${os}_${arch}.tar.gz" checksums.txt | shasum -a 256 -c -
#   (Linux without `shasum`: sha256sum --ignore-missing -c checksums.txt)

tar -xzf "claude-mem-go_${version}_${os}_${arch}.tar.gz" claude-mem-go
chmod +x claude-mem-go
./claude-mem-go doctor
```

To use this binary with the plugin (hooks, MCP server, `/mem-*` skills)
instead of building one, a Go toolchain is still not required for the binary
itself, but the plugin source (manifest, hooks, skills) still needs a local
checkout — `git clone` it, then copy the verified binary to
`$CLAUDE_PLUGIN_ROOT/claude-mem-go` (the path `scripts/ensure-binary.sh` would
otherwise build into) before running `claude plugin install` above; it will
find a working binary already in place and skip building.

There is no install script and no Setup-hook fetch of these binaries yet —
see [The self-healing binary](#the-self-healing-binary) for why the current
plugin install always builds from source, and CHANGELOG.md / the project's
issue tracker for that gap's status.

Uninstalling leaves `~/.claude-mem-go/` (store, logs, socket) in place; a
running worker daemon keeps running until stopped or the machine restarts.
For development, `claude --plugin-dir /path/to/claude-mem-go` runs the hooks
from the source tree without an install.

To use Postgres, set `CLAUDE_MEM_DB` in the environment Claude Code inherits;
the plugin passes no flags. See [postgres.md](postgres.md#setup).

## What gets wired

- **`.claude-plugin/plugin.json`** — the manifest (name `claude-mem-go`,
  version, MIT). `.claude-plugin/marketplace.json` defines the local
  marketplace `claude-mem-go-local` that the install command above names.
- **`hooks/hooks.json`** — six hook events, every command going through
  `scripts/run-hook.sh`:

  | Event | Matcher | Command | Timeout |
  |---|---|---|---|
  | `Setup` | `*` | `scripts/ensure-binary.sh` | 300s |
  | `SessionStart` | `startup\|clear\|compact` | `start`, then `context` | 15s, 10s |
  | `UserPromptSubmit` | — | `prompt-context` | 30s |
  | `PreToolUse` | `Read` | `file-context` | 10s |
  | `PostToolUse` | `*` | `hook` (async) | 120s |
  | `Stop` | — | `stop` (async) | 120s |

  A test reads `main.go`'s dispatch switch and fails if `hooks.json` names a
  subcommand that does not exist, since an unknown subcommand would print
  usage on stdout where Claude Code expects JSON.
- **`.mcp.json`** — the MCP server, `exec "$CLAUDE_PLUGIN_ROOT/claude-mem-go" mcp`
  over stdio.
- **`skills/`** — six skills, validated with `claude plugin validate --strict`
  and checked by a test against the server's real tool table (every tool name
  and parameter a skill uses must exist):
  - `/mem-search` — when to use `search_observations` vs.
    `semantic_search_observations`, and the full tool surface.
  - `/mem-doctor` — runs `doctor` inside a session and explains its findings.
    A test fails if any critical (`✘`) finding `doctor` can print is not
    explained in the skill.
  - `/mem-timeline` — a narrative project history, full or week-by-week,
    built from `search_observations` enumeration, `timeline`, and
    `get_observations`.
  - `/mem-prune`, `/mem-export`, `/mem-reembed` — surface `prune`,
    `export`/`import`, and `reembed`. `mem-prune` always runs the dry run,
    shows the count, and asks before adding `-yes`; `mem-reembed` does the
    same because each row costs an Ollama call.

## The self-healing binary

`hooks.json` and `.mcp.json` resolve `"$CLAUDE_PLUGIN_ROOT/claude-mem-go"`,
and that binary is gitignored: it is a build artifact. A plugin installed from
a git source therefore ships the complete Go source and no binary, every hook
fails with `No such file or directory`, Claude Code swallows the error, and
memory is silently dead — including `doctor`, which is a subcommand of the
missing binary. Two layers handle this:

- **`Setup` hook, `scripts/ensure-binary.sh`** — if `$CLAUDE_PLUGIN_ROOT/claude-mem-go`
  is missing or does not run (`version` fails — a wrong-architecture binary
  stats fine and fails only when executed), it builds from the shipped source.
  It builds to a temporary name and renames into place, because Go 1.26+
  refuses `go build -o X` onto an existing non-object file (the corrupt-binary
  case this script exists for) and `rename(2)` is atomic for a hook that
  fires mid-build. A cold build measures ~33s, a warm rebuild ~0.7s, and the
  common case is one `version` call. Without Go installed it prints the build
  command and exits 0. It runs at `Setup` with a 300s timeout because
  `SessionStart`'s hooks allow 10–15s. Outside a plugin install
  (`$CLAUDE_PLUGIN_ROOT` unset) it does nothing.
- **Hook wrapper, `scripts/run-hook.sh`** — `Setup` does not fire under
  `claude -p`, nor during `claude plugin install`, so every hook goes through
  a wrapper that probes `"$bin" version </dev/null` and `exec`s the binary on
  success. On failure the `context` hook returns an `additionalContext`
  payload telling the session to run
  `cd $CLAUDE_PLUGIN_ROOT && go build -o claude-mem-go ./cmd/claude-mem-go`
  and restart; every other hook appends a line to
  `~/.claude-mem-go/missing-binary.log` and exits 0. The wrapper deliberately
  does not build. It also detaches the `stop` hook after capturing stdin,
  because Claude Code tears hook processes down when a session ends (see
  [hooks.md](hooks.md#stop)).

Fetching a release asset instead of building is not wired: building from the
source the plugin already ships is the fix for the distribution in use.

## Verifying an install

```sh
./claude-mem-go doctor
```

`doctor` reports `✔ plugin "claude-mem-go" installed (scope=... version=...)`,
then `↳ binary OK` after executing the installed copy. Three related findings:

- `✘ plugin binary unusable` — critical only when the plugin is installed;
  names the path and the fix.
- `… note: the installed binary differs from this one` — you rebuilt but did
  not reinstall; the plugin cache holds its own copy that `go build` never
  updates. Informational: a stale binary still runs.
- `✘ the store is EMPTY, but the plugin is installed` — capture is configured
  and not working; check `~/.claude-mem-go/hook.log` and `worker.log`.

A plugin that is not installed is reported prominently but is not critical,
because `--plugin-dir`, the CLI, and the MCP server work without one. The
check reads `~/.claude/plugins/installed_plugins.json` and matches on the
plugin name alone, so an install from another marketplace still counts.

End to end, a fresh session should show injected context after a few tool
calls in an earlier session, and `./claude-mem-go stats` should show rows for
the project.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Nothing is ever captured; `doctor` says the store is empty but the plugin is installed | The plugin binary is missing or not runnable (`missing-binary.log` exists) | `cd $CLAUDE_PLUGIN_ROOT && go build -o claude-mem-go ./cmd/claude-mem-go`, restart Claude Code |
| `doctor` says the plugin is not installed | Installed at a different scope, or not at all | `claude plugin install ... --scope project` from inside the project; or ignore if using `--plugin-dir`/CLI only |
| Postgres is up but `doctor` reports the SQLite path | `CLAUDE_MEM_DB` is not set in the environment Claude Code inherits | Export it in your shell profile; `doctor` prints which of `-db`, `$CLAUDE_MEM_DB`, or the built-in path won |
| `✘ the worker daemon is writing to a DIFFERENT store than this command reads` | The daemon started before `CLAUDE_MEM_DB` changed, or two shells configure two stores | The next `SessionStart` replaces a daemon on the built-in path; for two explicit stores, stop the daemon by hand |
| Observations captured but `semantic-search` finds nothing | Ollama unreachable or model not pulled; rows unembedded | `ollama pull nomic-embed-text`; check `CLAUDE_MEM_OLLAMA_BASE_URL`; run `reembed -yes` |
| `hook.log` says `FAILED forwarding to worker` | No daemon running | Start a session (`start` spawns one) or run `claude-mem-go start`; check `start.log` |
| Third concurrent session captures nothing; `doctor` shows `pool=4/4` | All observer slots held by cached sessions | Raise `-max-concurrent` on `start`/`worker` |
| Session summaries never appear | `stop` is detached and logs only to `stop.log`; a `-p` session may end before it runs | Read `~/.claude-mem-go/stop.log`; `doctor` flags sessions without summaries |
| Hook output appears as usage text in the session | A hook names a subcommand that does not exist | Rebuild; `TestHooksJSONInvokesRealSubcommands` guards this |
| `start.log` says `did not become ready` with a socket path error | Socket path over the OS limit (104 bytes on macOS) | Use a shorter `-socket` path or home directory |
