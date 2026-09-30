#!/usr/bin/env bash
# Ensure $CLAUDE_PLUGIN_ROOT/claude-mem-go exists and runs: reuse it if it
# already works, fetch a matching release binary if this checkout has one
# pinned, build it from the source the plugin already ships otherwise,
# and never block a session no matter how that goes.
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
# fetch or build does not land on the user's first prompt.
#
# Measured on this project: a genuinely cold build (empty module and
# build caches, dependencies downloaded) takes ~33s; a warm rebuild takes
# ~0.7s; a fetch attempt that fails outright (offline, DNS, 404) costs at
# most ~20s (see fetch_url's timeouts below) before falling through to
# that same build; and the common case — a binary that is already
# present and working — costs one `version` invocation and exits
# immediately.
#
# DECISION ORDER
#
#   1. Existing working binary (below) — cheapest path.
#   2. Release fetch (fetch_release) — reached only when step 1 fails,
#      and only when this checkout has a version and a pin for it. See
#      that function's own comments for what disqualifies it silently
#      versus what it reports.
#   3. Build from source (the original, unconditional fallback) —
#      reached when the fetch is skipped or fails outright.
#   4. Explain and exit 0 — final fallback, unchanged exit code.
#
# Every step exits 0 no matter what. Nothing here adds a path that exits
# non-zero or blocks a session.
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

# --- writability probe --------------------------------------------------
#
# Hoisted here, before either the fetch or the build path is attempted —
# not folded into the "is go installed" branch below, and not deferred
# until a write actually fails partway through one of those steps. A
# read-only $root with go present used to fall into the build attempt,
# fail with a raw permission error from `go build -o`, and print the
# generic "build FAILED" line, which gives an operator no reason to
# suspect the directory rather than the toolchain. Probing first means
# the message below names the real cause before go's presence, or a
# release fetch, is even considered.
#
# The probe exercises the exact permission both the fetch's extraction
# and the build's temp-file both need: create a file next to $bin, then
# remove it.
probe="$bin.probe.$$"
if ! ( : >"$probe" ) 2>/dev/null; then
	echo "claude-mem-go: $bin is missing or not runnable, and $root is not writable" >&2
	# Exit 0 deliberately, same reasoning as every other branch below:
	# memory degrading to "nothing captured" is bad, blocking the session
	# would be worse.
	exit 0
fi
rm -f "$probe"

# --- release fetch --------------------------------------------------------
#
# Tries to fetch a matching release archive instead of building one, and
# reports nothing on any path where it declines or fails — the fallback
# to build-from-source below is silent and total, by design (see the
# failure-mode comments inside fetch_release).

# resolve_version prints the version to fetch and returns 0, or returns 1
# with nothing printed if none could be determined.
#
# Pinned in .claude-plugin/plugin.json first — the path a normal install
# takes, since that manifest is what a real `claude plugin install`
# copies alongside the binary. The checkout's own tag is a fallback used
# ONLY when the manifest is unreadable or has no parseable "version"
# field, not a second source of truth to prefer. A dev checkout with no
# tag (or a manifest version that has no release yet) makes both lookups
# fail, and fetch_release below then has no version to look for a pin
# under — exactly the "no pin for this checkout" case, handled the same
# way as any other reason to skip the fetch.
resolve_version() {
	manifest="$root/.claude-plugin/plugin.json"
	if [ -r "$manifest" ]; then
		v=$(grep -m1 '"version"' "$manifest" 2>/dev/null |
			sed -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/')
		if [ -n "$v" ]; then
			printf '%s' "$v"
			return 0
		fi
	fi
	if command -v git >/dev/null 2>&1; then
		v=$(git -C "$root" describe --tags --exact-match 2>/dev/null | sed -E 's/^v//')
		if [ -n "$v" ]; then
			printf '%s' "$v"
			return 0
		fi
	fi
	return 1
}

# os_name/arch_name map `uname` to goreleaser's naming for this project's
# archives (see .goreleaser.yaml: raw GOOS/GOARCH, not renamed) — the same
# mapping docs/plugin-install.md's manual-install instructions use by
# hand. An unrecognized platform returns 1 with nothing printed:
# unsupported os/arch is a silent skip, not a reported failure, the same
# as it is today when no release asset would exist for it anyway.
os_name() {
	case "$(uname -s)" in
	Linux) printf 'linux' ;;
	Darwin) printf 'darwin' ;;
	*) return 1 ;;
	esac
}

arch_name() {
	case "$(uname -m)" in
	x86_64 | amd64) printf 'amd64' ;;
	aarch64 | arm64) printf 'arm64' ;;
	*) return 1 ;;
	esac
}

# release_host prints the host portion of a URL (scheme stripped, then up
# to the first '/', with userinfo and port removed). Pure string parsing,
# not a DNS resolution — deliberately: resolving the override's host would
# add a dependency this script otherwise has none of, and the property
# this function's caller actually needs (is the LITERAL host a loopback
# name) does not require it. A URL whose loopback-looking host is
# deliberately mis-resolved by the local resolver is a much narrower,
# already-privileged attack than what this check exists to stop (an
# arbitrary env var silently redirecting an outbound request to a
# non-loopback host).
#
# An authority containing '@' (userinfo, e.g. "127.0.0.1@evil.example")
# is rejected outright — printed as an empty host, which is_loopback_host
# always rejects — rather than parsed for the part after '@', so a
# loopback-looking prefix can never smuggle a non-loopback host in behind
# it.
release_host() {
	rest="${1#*://}"
	authority="${rest%%/*}"
	case "$authority" in
	*@*)
		printf ''
		return
		;;
	esac
	case "$authority" in
	\[*)
		# Bracketed IPv6 literal, e.g. "[::1]" or "[::1]:8080" — keep the
		# brackets so is_loopback_host can match the literal form used in
		# URLs.
		printf '%s' "${authority%%]*}]"
		;;
	*)
		printf '%s' "${authority%%:*}"
		;;
	esac
}

# is_loopback_host accepts only: the literal name "localhost"; "::1" or
# its bracketed URL form "[::1]"; or a strict dotted quad in 127/8 (four
# all-digit octets, each 0-255, first octet 127). Deliberately NOT a glob
# like "127.*.*.*" — "*" matches any characters including dots and
# letters, so that glob also matches a DNS name such as
# "127.x.evil.example" or "127.0.0.1.evil.example", and (combined with
# release_host not stripping userinfo) "127.0.0.1@evil.example". Each of
# those resolves or forwards to a host this check must reject.
is_loopback_host() {
	host="$1"
	case "$host" in
	localhost | ::1 | \[::1\])
		return 0
		;;
	esac
	case "$host" in
	127.*.*.*) ;;
	*) return 1 ;;
	esac
	oldifs="$IFS"
	IFS=.
	# shellcheck disable=SC2086 # word splitting on IFS=. is the point: it's how the four octets get split apart.
	set -- $host
	IFS="$oldifs"
	[ "$#" -eq 4 ] || return 1
	for octet in "$1" "$2" "$3" "$4"; do
		case "$octet" in
		'' | *[!0-9]*) return 1 ;;
		esac
		[ "$((10#$octet))" -le 255 ] || return 1
	done
	[ "$1" = 127 ] || return 1
	return 0
}

# fetch_url downloads $1 to $2 using whichever of curl/wget is on PATH,
# preferring curl. Both are given a total wall-clock budget, not just a
# connect budget: `--connect-timeout`/wget's per-phase timeouts alone
# bound only the TCP handshake (curl) or reset on every byte received
# (wget), so a connection that opens fine and then trickles would
# otherwise never trip them.
#
#   curl:  --max-time 20 bounds the whole request regardless of how the
#          slowness happens — a hard ceiling.
#   wget:  has no equivalent single flag; --timeout=N applies to each of
#          DNS/connect/read individually, and a still-moving transfer
#          resets the read-timeout clock on every byte. Its worst case is
#          therefore a multiple of N, not a hard N ceiling the way curl's
#          --max-time is — accepted rather than reached for GNU
#          coreutils' timeout(1), which is not guaranteed present on
#          macOS (BSD userland) and isn't worth a new dependency for a
#          tighter bound wget itself can't give.
#
# Compared to the Setup hook's own 300s timeout (hooks/hooks.json):
# worst-case latency here is a failed fetch attempt (~20s curl, ~15s
# wget across DNS/connect/read) followed by a cold build (~33s) — about
# 35-55s, still comfortably inside 300s, with the existing build's own
# margin essentially untouched.
fetch_url() {
	url="$1"
	out="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --connect-timeout 5 --max-time 20 -o "$out" "$url" 2>/dev/null
		return $?
	fi
	if command -v wget >/dev/null 2>&1; then
		wget -q --timeout=5 --tries=1 -O "$out" "$url" 2>/dev/null
		return $?
	fi
	return 127
}

# sha256_of prints the sha256 of $1 using whichever of sha256sum/shasum is
# on PATH — not assumed to be one specific name, since both ship with
# every OS this project targets but under different binary names.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
		return 0
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
		return 0
	fi
	return 1
}

# fetch_release attempts the whole fetch-verify-extract sequence and
# returns 0 only once a fetched binary is in place at $bin AND proven to
# run. Any other outcome returns 1 and leaves $bin untouched, so the
# caller's fallback to build-from-source is always safe.
#
# CHECKSUM VERIFICATION, AND WHY IT IS NOT TRUST-ON-FIRST-USE
#
# The expected checksum comes from this checkout's own copy of
# scripts/checksums/<version>.txt — never from anything fetched from the
# release host at runtime. That file is committed to the default branch
# only AFTER release.yml's build and verify jobs have independently
# rebuilt the tagged commit on separate runners and confirmed their
# outputs are byte-identical (see scripts/checksums/README.md for the
# full mechanism and why the pin cannot be committed before the tag
# exists). So the value a download is checked against was authored and
# reviewable before that release's assets ever existed, by CI jobs
# running before publish rather than by whatever produced the download —
# a compromised release asset now has to also match a value that
# predates it, not merely match itself the way trust-on-first-use would.
#
# If no pin file exists for the resolved version — a checkout that
# predates the pin PR merging, or an exact-tag checkout whose pin PR
# hasn't landed on the default branch yet (see
# docs/plugin-install.md#the-self-healing-binary) — the fetch is skipped
# entirely, same as "no matching asset": the absence of a pin is never
# treated as "skip verification and trust the download anyway."
#
# CLAUDE_MEM_GO_RELEASE_BASE_URL exists only so tests can point this at a
# local fixture server instead of the real GitHub Releases host. It is
# safe to let it override the host at all ONLY because verification
# checks the download against the locally pinned checksum above, not
# against anything fetched from whatever host is configured — pointing
# it at an attacker-controlled host cannot make a malicious binary pass
# verification, since the pin was committed before the override was ever
# consulted. That said, an arbitrary env var silently redirecting an
# outbound network call is still an SSRF-shaped surface independent of
# what verification catches — the request itself (headers, timing, the
# mere fact that a plugin's Setup hook reached some internal host) is
# observable even when the response can never be trusted. So the
# override is honored only when its authority has no userinfo and its
# host is strictly loopback (127.0.0.0/8, "::1"/"[::1]", or "localhost" —
# see is_loopback_host's own comment for why this is not a glob), or when
# the test-only CLAUDE_MEM_GO_ALLOW_REMOTE_RELEASE_BASE_URL=1 is also
# set; any other value is treated as absent and the real release host is
# used instead.
fetch_release() {
	version=$(resolve_version) || return 1
	[ -n "$version" ] || return 1

	pin="$root/scripts/checksums/$version.txt"
	[ -r "$pin" ] || return 1

	os=$(os_name) || return 1
	arch=$(arch_name) || return 1

	if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
		return 1
	fi
	if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
		return 1
	fi

	asset="claude-mem-go_${version}_${os}_${arch}.tar.gz"
	# Exact filename match on column 2, not `grep -F "$asset" "$pin"` — a
	# substring match would also hit a longer name sharing the same
	# prefix, such as a future "$asset.sbom" sidecar entry.
	expected=$(awk -v a="$asset" '$2==a{print $1; exit}' "$pin" 2>/dev/null)
	[ -n "$expected" ] || return 1

	base="https://github.com/satishbabariya/claude-mem-go/releases/download/v${version}"
	override="${CLAUDE_MEM_GO_RELEASE_BASE_URL:-}"
	if [ -n "$override" ]; then
		host=$(release_host "$override")
		if is_loopback_host "$host" || [ "${CLAUDE_MEM_GO_ALLOW_REMOTE_RELEASE_BASE_URL:-}" = "1" ]; then
			base="$override"
		fi
		# Any other value: silently ignored, falls back to the real
		# release host set above — see this function's doc comment for
		# why that is still safe.
	fi

	archive="$root/.claude-mem-go-fetch.$$.tar.gz"
	if ! fetch_url "${base%/}/$asset" "$archive"; then
		rm -f "$archive"
		return 1
	fi

	actual=$(sha256_of "$archive") || {
		rm -f "$archive"
		return 1
	}
	if [ "$actual" != "$expected" ]; then
		# The one fetch-path failure that gets a message: a checksum
		# mismatch (or the truncated-download case, which fails this
		# same check by construction) is security-relevant in a way a
		# plain network hiccup is not, and worth someone noticing in
		# logs.
		echo "claude-mem-go: downloaded binary failed checksum verification, discarding" >&2
		rm -f "$archive"
		return 1
	fi

	tmp="$bin.build.$$"
	if ! tar -xzf "$archive" -O claude-mem-go >"$tmp" 2>/dev/null; then
		rm -f "$archive" "$tmp"
		return 1
	fi
	rm -f "$archive"
	chmod +x "$tmp"

	# Re-run the liveness check before declaring success, same reasoning
	# as the build path below: a structurally valid but wrong-architecture
	# or otherwise broken extracted binary must fall through to
	# build-from-source rather than get installed and exit 0 on a dead
	# binary.
	if ! "$tmp" version >/dev/null 2>&1; then
		rm -f "$tmp"
		return 1
	fi

	mv -f "$tmp" "$bin"
	echo "claude-mem-go: fetched release v$version binary ($os/$arch)." >&2
	return 0
}

if fetch_release; then
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
# Build to a temporary name and rename into place rather than building
# straight onto $bin. Two reasons:
#
#  - Go >= 1.26 refuses `go build -o X` when X already exists and is not
#    a Go object file ("build output already exists and is not an object
#    file"), so a corrupt or foreign file at $bin could never be repaired
#    by building over it — exactly the "present but not runnable" case
#    this script exists to fix.
#  - rename(2) is atomic, so a hook that fires while a concurrent Setup
#    is mid-build sees either the old binary or the new one, never a
#    half-written file.
tmp="$bin.build.$$"
if (cd "$root" && go build -o "$tmp" ./cmd/claude-mem-go) >&2 && mv -f "$tmp" "$bin"; then
	echo "claude-mem-go: built successfully." >&2
else
	rm -f "$tmp"
	echo "claude-mem-go: build FAILED — hooks will not fire until this is resolved." >&2
fi
exit 0
