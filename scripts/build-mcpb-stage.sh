#!/bin/sh
# Shared by release.yml's build/verify jobs and ci.yml's reproducible-build
# job: lays out the .mcpb staging directory from goreleaser's just-built
# dist/*.tar.gz archives, then packs it via scripts/build-mcpb.
#
# One script, not copy-pasted into three workflow steps, so the three
# call sites can never drift out of sync with each other — the same
# reason release.yml's build/verify jobs already share the identical
# goreleaser invocation rather than each growing their own variant.
#
# Usage: build-mcpb-stage.sh <version>
#   <version> names the output file (dist/claude-mem-go_<version>.mcpb)
#   and is otherwise unrelated to goreleaser's own archive naming — the
#   four tar.gz archives are located by os/arch glob, not by this
#   version string, since a snapshot build's goreleaser Version differs
#   from the plain "X.Y.Z" this script is called with.
set -eu

version="${1:?usage: build-mcpb-stage.sh <version>}"

stage="${RUNNER_TEMP:-/tmp}/mcpb-stage"
rm -rf "$stage"
mkdir -p "$stage/mcpb" "$stage/bin"

cp manifest.json "$stage/manifest.json"
cp mcpb/launch.sh "$stage/mcpb/launch.sh"
chmod 755 "$stage/mcpb/launch.sh"

for os in darwin linux; do
  for arch in amd64 arm64; do
    # globbed, not built from a version string: goreleaser's Version
    # template var for a snapshot build (ci.yml's reproducible-build job)
    # is a pseudo-version, not this script's plain "<version>" argument.
    archive="$(ls dist/claude-mem-go_*_"${os}"_"${arch}".tar.gz)"
    extract_dir="${RUNNER_TEMP:-/tmp}/mcpb-extract-${os}-${arch}"
    rm -rf "$extract_dir"
    mkdir -p "$extract_dir"
    tar -xzf "$archive" -C "$extract_dir" claude-mem-go
    mv "$extract_dir/claude-mem-go" "$stage/bin/claude-mem-go_${os}_${arch}"
    chmod 755 "$stage/bin/claude-mem-go_${os}_${arch}"
  done
done

# The commit's own timestamp, not wall-clock "now" — the same
# reproducibility reasoning as .goreleaser.yaml's mod_timestamp (see that
# file's comments): two independent runs of this script against the
# same commit, seconds or minutes apart, must stamp every zip entry
# identically.
mtime="$(git log -1 --format=%cI HEAD)"

name="claude-mem-go_${version}.mcpb"
go run ./scripts/build-mcpb -root "$stage" -out "dist/$name" -mtime "$mtime"

# goreleaser's own checksum: step (.goreleaser.yaml) only covers the
# archives goreleaser itself produced; the .mcpb is built afterward, by
# this script, so it needs its own line appended in the same
# "<hash>  <filename>" format sha256sum/shasum both already use — run
# from inside dist/ so the recorded filename is bare, matching every
# other entry already in checksums.txt (and matching a release asset
# layout, where checksums.txt sits next to the files it describes, not
# one directory above them).
(cd dist && sha256sum "$name" >> checksums.txt)
