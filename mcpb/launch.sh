#!/bin/sh
# Entry point bundled inside the claude-mem-go .mcpb package.
#
# The MCPB manifest format's platform_overrides only keys on OS
# (win32/darwin/linux — confirmed against modelcontextprotocol/mcpb's own
# manifest schema, which has no architecture field anywhere), not CPU
# architecture. This repo ships four binaries per release (darwin/linux x
# amd64/arm64, matching .goreleaser.yaml's build matrix), so arch
# dispatch has to happen here, inside the one script manifest.json's
# darwin and linux platform_overrides entries both point "command" at —
# not as a manifest field that doesn't exist.
set -eu

os="$(uname -s)"
arch="$(uname -m)"

case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *)
    echo "claude-mem-go: unsupported architecture '$arch' (bundle has amd64, arm64)" >&2
    exit 1
    ;;
esac

case "$os" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    echo "claude-mem-go: unsupported OS '$os' (bundle has darwin, linux)" >&2
    exit 1
    ;;
esac

# Resolve relative to this script's own location, not $PWD — an MCP
# client launches this with whatever cwd it happens to have. bin/ is a
# sibling of mcpb/ at the bundle root (manifest.json's own level), not
# nested under mcpb/ alongside this script.
dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
bin="$dir/bin/claude-mem-go_${os}_${arch}"

if [ ! -x "$bin" ]; then
  echo "claude-mem-go: no bundled binary for ${os}/${arch} at $bin" >&2
  exit 1
fi

exec "$bin" "$@"
