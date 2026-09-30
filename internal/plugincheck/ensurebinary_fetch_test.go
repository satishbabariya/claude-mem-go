package plugincheck

// Tests for ensure-binary.sh's release-fetch step (fetch_release and its
// helpers). These run the REAL script (via runScript, defined in
// ensurebinary_test.go) against a local HTTP fixture server bound to
// 127.0.0.1:0, never the real GitHub Releases host, so nothing here
// depends on network reachability or flakes on it. See
// scripts/checksums/README.md and scripts/ensure-binary.sh's own
// comments for the design this exercises.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// --- fixtures --------------------------------------------------------

// writePluginManifest writes .claude-plugin/plugin.json, the file
// resolve_version reads first.
func writePluginManifest(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, ".claude-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// writePin writes scripts/checksums/<version>.txt — the local pin
// fetch_release verifies a download against. line is a full
// "<sha256>  <filename>" entry, goreleaser's checksums.txt format.
func writePin(t *testing.T, root, version, line string) {
	t.Helper()
	dir := filepath.Join(root, "scripts", "checksums")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir checksums dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, version+".txt"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write pin: %v", err)
	}
}

// fakePluginRootWithManifest builds on fakePluginRoot (a REAL, buildable
// Go module, so the build-fallback assertions below exercise a genuine
// `go build`, not a stub) and adds a plugin manifest pinning version.
func fakePluginRootWithManifest(t *testing.T, version string) string {
	t.Helper()
	root := fakePluginRoot(t)
	writePluginManifest(t, root, fmt.Sprintf(`{"name":"claude-mem-go","version":"%s"}`, version))
	return root
}

// gitInitAndTag commits everything currently in root and tags HEAD,
// for the checkout's-own-tag version-selection fallback test.
func gitInitAndTag(t *testing.T, root, tag string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	run("tag", tag)
}

// buildArchiveBytes builds a minimal but real .tar.gz containing a single
// "claude-mem-go" member with binScript as its content, and returns the
// archive bytes plus their sha256 — the same shape release_fetch expects
// (goreleaser's own archives also carry README/CHANGELOG/LICENSE
// alongside the binary, but fetch_release only ever reads the
// "claude-mem-go" member, via `tar -xzf ... -O claude-mem-go`, so those
// extra members add nothing this test needs).
func buildArchiveBytes(t *testing.T, binScript string) (archive []byte, sha256Hex string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := []byte(binScript)
	if err := tw.WriteHeader(&tar.Header{Name: "claude-mem-go", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// hostAssetOSArch returns the goreleaser-style os/arch names for the
// machine actually running the test, matching what the script's own
// os_name/arch_name would compute against the REAL, un-shimmed uname —
// so the happy-path/checksum/download tests exercise real platform
// detection rather than a fixed guess.
func hostAssetOSArch(t *testing.T) (string, string) {
	t.Helper()
	var osName string
	switch runtime.GOOS {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "darwin"
	default:
		t.Skipf("unsupported test host OS %s", runtime.GOOS)
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		t.Skipf("unsupported test host arch %s", runtime.GOARCH)
	}
	return osName, arch
}

// fixtureServer is a local HTTP fixture bound to 127.0.0.1:0 (an
// OS-assigned port, so parallel runs never collide) serving a fixed set
// of files by exact path, and counting how many requests it received —
// several tests below assert that count stays zero, since the whole
// point of skipping the fetch early (unsupported platform, no
// downloader, a read-only root) is that no network call is ever
// attempted.
type fixtureServer struct {
	*httptest.Server
	hits int32
}

func newFixtureServer(t *testing.T, files map[string][]byte) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.hits, 1)
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(fs.Server.Close)
	return fs
}

func (f *fixtureServer) Hits() int32 { return atomic.LoadInt32(&f.hits) }

// neededTools are the external commands (besides curl/wget, which each
// test controls deliberately) fetch_release and its build-fallback
// sibling need. pathWithout resolves each via the REAL PATH at test
// time and symlinks it into a fresh directory, so a test can hand the
// script a PATH containing exactly these tools — no more — regardless
// of how many other directories or binaries happen to be on the host.
var neededTools = []string{"uname", "grep", "sed", "awk", "head", "tar", "chmod", "mv", "rm", "git", "go", "sha256sum", "shasum"}

func pathWithout(t *testing.T, exclude ...string) string {
	t.Helper()
	dir := t.TempDir()
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}
	for _, name := range neededTools {
		if excluded[name] {
			continue
		}
		p, err := exec.LookPath(name)
		if err != nil {
			continue // not installed on this machine; the script must treat it as absent too
		}
		if err := os.Symlink(p, filepath.Join(dir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	return dir
}

// pathWithFakeUname builds a PATH with every needed tool EXCEPT the real
// uname, replaced by a fake one reporting unameS/unameM — the mechanism
// the design calls for to test the unsupported-platform row without
// depending on what the test runner's own OS/arch happen to be.
func pathWithFakeUname(t *testing.T, unameS, unameM string) string {
	t.Helper()
	dir := pathWithout(t, "uname")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n-s) echo %q ;;\n-m) echo %q ;;\nesac\n", unameS, unameM)
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake uname: %v", err)
	}
	return dir
}

// pathWithFakeCurl builds a PATH with every needed tool plus a fake curl
// that records its full argument list to logFile and always fails
// (exit 1). Used only by the non-loopback-override test, to prove which
// URL the script actually asked for without needing a live request to
// succeed or fail over a real network connection — the property under
// test is the URL chosen, not the transfer outcome.
func pathWithFakeCurl(t *testing.T, logFile string) string {
	t.Helper()
	dir := pathWithout(t, "curl")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexit 1\n", shellSingleQuote(logFile))
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}
	return dir
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// assertBuiltFromSource confirms the script fell all the way through to
// building fakePluginRoot's own source (its main.go prints "fake
// version" — see ensurebinary_test.go) rather than installing anything
// from the fetch attempt.
func assertBuiltFromSource(t *testing.T, root, scriptOutput string) {
	t.Helper()
	bin := filepath.Join(root, "claude-mem-go")
	got, err := exec.Command(bin).CombinedOutput()
	if err != nil || !strings.Contains(string(got), "fake version") {
		t.Fatalf("did not fall through to building from source: %v / %q\nscript output:\n%s", err, got, scriptOutput)
	}
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash script; not exercised on windows")
	}
}

// --- the fixture-server test matrix -----------------------------------

// TestFetchReleaseHappyPath is the case every other row exists to fall
// back safely from: a version is pinned, this checkout has a matching
// local checksum, and a matching archive exists at the (fixture) base
// URL. The fetch must win over building from source.
func TestFetchReleaseHappyPath(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.9"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	archiveBytes, sum := buildArchiveBytes(t, "#!/bin/sh\necho fetched-fake-binary\nexit 0\n")
	fs := newFixtureServer(t, map[string][]byte{asset: archiveBytes})
	writePin(t, root, version, sum+"  "+asset)

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	bin := filepath.Join(root, "claude-mem-go")
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("fetched binary was not installed: %v\n%s", err, out)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("fetched binary is not executable (mode %s)", info.Mode().Perm())
	}
	got, err := exec.Command(bin).CombinedOutput()
	if err != nil || !strings.Contains(string(got), "fetched-fake-binary") {
		t.Fatalf("fetched binary does not run as expected: %v / %q", err, got)
	}
	if fs.Hits() == 0 {
		t.Fatal("fixture server never received a request")
	}
	if !strings.Contains(out, "fetched release") {
		t.Fatalf("no success message on stderr: %s", out)
	}
}

// TestFetchReleaseChecksumMismatchFallsBackToBuild covers a downloaded
// archive that does not match the pinned checksum — the one fetch-path
// failure the design calls out as security-relevant enough to log.
func TestFetchReleaseChecksumMismatchFallsBackToBuild(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.8"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	archiveBytes, _ := buildArchiveBytes(t, "#!/bin/sh\necho should-never-run\nexit 0\n")
	fs := newFixtureServer(t, map[string][]byte{asset: archiveBytes})
	// A well-formed but wrong pin: the served archive's real checksum is
	// deliberately NOT what gets written here.
	writePin(t, root, version, strings.Repeat("0", 64)+"  "+asset)

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if !strings.Contains(out, "failed checksum verification") {
		t.Fatalf("no checksum-mismatch message: %s", out)
	}
	assertBuiltFromSource(t, root, out)
}

// TestFetchReleasePartialDownloadFallsBackToBuild covers a truncated
// transfer that nonetheless completes as a syntactically valid HTTP
// response (correct Content-Length for the bytes actually sent) — the
// design's point that a partial download needs no separate detection
// mechanism, because it fails the checksum check by construction.
func TestFetchReleasePartialDownloadFallsBackToBuild(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.7"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	full, sum := buildArchiveBytes(t, "#!/bin/sh\necho should-never-run\nexit 0\n")
	truncated := full[:len(full)/2]
	fs := newFixtureServer(t, map[string][]byte{asset: truncated})
	// Pinned against the FULL archive, which the fixture never actually sends.
	writePin(t, root, version, sum+"  "+asset)

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if !strings.Contains(out, "failed checksum verification") {
		t.Fatalf("a truncated download did not trip the checksum check: %s", out)
	}
	assertBuiltFromSource(t, root, out)
}

// TestFetchReleaseAssetNotFoundFallsBackToBuildSilently covers a 404 on
// the release asset — treated the same as "no matching asset", not a
// security event, so no distinct message.
func TestFetchReleaseAssetNotFoundFallsBackToBuildSilently(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.6"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	fs := newFixtureServer(t, map[string][]byte{}) // nothing registered: every path 404s
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if strings.Contains(out, "checksum verification") {
		t.Fatalf("a plain 404 should not be reported as a checksum failure: %s", out)
	}
	if fs.Hits() == 0 {
		t.Fatal("expected a request that then 404s")
	}
	assertBuiltFromSource(t, root, out)
}

// TestFetchReleaseOfflineFallsBackToBuildSilently covers a closed port
// (connection refused) — the offline case.
func TestFetchReleaseOfflineFallsBackToBuildSilently(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.5"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)

	// Bind and immediately close: the port is refusing connections, the
	// same as no network reaching the release host, without depending
	// on any specific port number being free or closed on the host.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+closedURL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if strings.Contains(out, "checksum verification") {
		t.Fatalf("offline should not be reported as a checksum failure: %s", out)
	}
	assertBuiltFromSource(t, root, out)
}

// TestFetchReleaseDNSFailureFallsBackToBuildSilently covers a host that
// does not resolve. Uses the RFC 2606 .invalid TLD, guaranteed to never
// resolve, and the test-only allow-remote flag (a non-loopback host is
// the point of this test, not the override restriction itself — that
// is TestFetchReleaseNonLoopbackOverrideIsIgnored's job).
func TestFetchReleaseDNSFailureFallsBackToBuildSilently(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.41"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL=http://claude-mem-go-fixture-test.invalid:9999",
		"CLAUDE_MEM_GO_ALLOW_REMOTE_RELEASE_BASE_URL=1",
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if strings.Contains(out, "checksum verification") {
		t.Fatalf("a DNS failure should not be reported as a checksum failure: %s", out)
	}
	assertBuiltFromSource(t, root, out)
}

// TestFetchReleaseUnsupportedPlatformSkipsFetchEntirely covers an
// OS/arch this project never builds for. No message, and — the part
// that matters most — zero requests to the release host, proven with a
// fixture server that would otherwise happily serve a (mismatched)
// asset.
func TestFetchReleaseUnsupportedPlatformSkipsFetchEntirely(t *testing.T) {
	skipOnWindows(t)
	version := "9.9.1"
	root := fakePluginRootWithManifest(t, version)
	writePin(t, root, version, strings.Repeat("a", 64)+"  claude-mem-go_9.9.1_plan9_mips.tar.gz")
	fs := newFixtureServer(t, map[string][]byte{})
	fakeUnamePath := pathWithFakeUname(t, "Plan9", "mips")

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL,
		"PATH="+fakeUnamePath,
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if fs.Hits() != 0 {
		t.Fatalf("an unsupported platform must never contact the release host, got %d hits", fs.Hits())
	}
	assertBuiltFromSource(t, root, out)
}

// TestFetchReleaseNoDownloaderSkipsFetchEntirely covers a PATH with
// neither curl nor wget — the fetch must be skipped without attempting
// anything, same as an unsupported platform.
func TestFetchReleaseNoDownloaderSkipsFetchEntirely(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.2"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)
	fs := newFixtureServer(t, map[string][]byte{})
	pathDir := pathWithout(t) // neededTools only — no curl, no wget

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL,
		"PATH="+pathDir,
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if fs.Hits() != 0 {
		t.Fatalf("no curl/wget must never contact the release host, got %d hits", fs.Hits())
	}
	assertBuiltFromSource(t, root, out)
}

// TestFetchReleaseReadOnlyRootSkipsFetchAndBuild covers the writability
// probe: a read-only $root must be diagnosed with a message distinct
// from "Go is not installed", before either the fetch or the build is
// attempted.
func TestFetchReleaseReadOnlyRootSkipsFetchAndBuild(t *testing.T) {
	skipOnWindows(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	osName, arch := hostAssetOSArch(t)
	version := "9.9.3"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)
	fs := newFixtureServer(t, map[string][]byte{})

	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	bin := filepath.Join(root, "claude-mem-go")
	want := fmt.Sprintf("%s is missing or not runnable, and %s is not writable", bin, root)
	if !strings.Contains(out, want) {
		t.Fatalf("missing the distinct read-only message %q: %s", want, out)
	}
	if strings.Contains(out, "Go is not installed") {
		t.Fatalf("a read-only root must not be confused with the Go-not-installed message: %s", out)
	}
	if fs.Hits() != 0 {
		t.Fatalf("a read-only root must be diagnosed before any fetch attempt, got %d hits", fs.Hits())
	}
	if _, err := os.Stat(bin); err == nil {
		t.Fatal("a binary should never appear under a read-only root")
	}
}

// TestFetchReleaseNonLoopbackOverrideIsIgnored covers the security
// property CLAUDE_MEM_GO_RELEASE_BASE_URL depends on: a non-loopback
// override, without the test-only allow flag, must never be honored —
// the real release host is used instead. Verified with a fake curl that
// records the URL it was asked to fetch (and always fails, so the
// script proceeds to build from source), rather than a real request to
// any host, loopback or not — this is a routing decision, not a
// transfer outcome, and testing it this way needs no network at all.
func TestFetchReleaseNonLoopbackOverrideIsIgnored(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.4"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)
	// Stands in for "the attacker-controlled host the override tried to
	// point at" — must receive zero requests.
	attacker := newFixtureServer(t, map[string][]byte{asset: []byte("should never be fetched")})

	logFile := filepath.Join(t.TempDir(), "fake-curl.log")
	pathDir := pathWithFakeCurl(t, logFile)

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL="+strings.Replace(attacker.URL, "127.0.0.1", "example.com", 1),
		"PATH="+pathDir,
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if attacker.Hits() != 0 {
		t.Fatalf("the rejected override's real target must never be contacted, got %d hits", attacker.Hits())
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fake curl was never invoked: %v\nscript output:\n%s", err, out)
	}
	if strings.Contains(string(logged), "example.com") {
		t.Fatalf("the rejected non-loopback override leaked into the request: %s", logged)
	}
	if !strings.Contains(string(logged), "github.com") {
		t.Fatalf("expected the real release host to be used instead, got: %s", logged)
	}
}

// TestFetchReleaseUserinfoBypassIsRejected covers
// "127.0.0.1@evil.example": release_host must not treat the part before
// '@' as the host, since is_loopback_host would otherwise see the
// loopback-looking userinfo and accept an authority that curl actually
// connects to "evil.example" for. Same verification shape as
// TestFetchReleaseNonLoopbackOverrideIsIgnored: the attacker-controlled
// real target gets zero requests, and the URL fake curl was actually
// asked to fetch uses the real release host instead.
func TestFetchReleaseUserinfoBypassIsRejected(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.50"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)
	attacker := newFixtureServer(t, map[string][]byte{asset: []byte("should never be fetched")})

	logFile := filepath.Join(t.TempDir(), "fake-curl.log")
	pathDir := pathWithFakeCurl(t, logFile)

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL="+strings.Replace(attacker.URL, "127.0.0.1", "127.0.0.1@evil.example", 1),
		"PATH="+pathDir,
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if attacker.Hits() != 0 {
		t.Fatalf("the userinfo bypass's real target must never be contacted, got %d hits", attacker.Hits())
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fake curl was never invoked: %v\nscript output:\n%s", err, out)
	}
	if strings.Contains(string(logged), "evil.example") {
		t.Fatalf("the userinfo bypass leaked into the request: %s", logged)
	}
	if !strings.Contains(string(logged), "github.com") {
		t.Fatalf("expected the real release host to be used instead, got: %s", logged)
	}
}

// TestFetchReleaseGlobSuffixBypassIsRejected covers
// "127.x.evil.example": a naive glob check like "127.*.*.*" matches this
// (three wildcarded segments after "127."), even though it is a DNS name
// that can resolve anywhere. is_loopback_host must reject it because its
// second label ("x") is not an all-digit octet.
func TestFetchReleaseGlobSuffixBypassIsRejected(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.51"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)
	attacker := newFixtureServer(t, map[string][]byte{asset: []byte("should never be fetched")})

	logFile := filepath.Join(t.TempDir(), "fake-curl.log")
	pathDir := pathWithFakeCurl(t, logFile)

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL="+strings.Replace(attacker.URL, "127.0.0.1", "127.x.evil.example", 1),
		"PATH="+pathDir,
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if attacker.Hits() != 0 {
		t.Fatalf("the glob-suffix bypass's real target must never be contacted, got %d hits", attacker.Hits())
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fake curl was never invoked: %v\nscript output:\n%s", err, out)
	}
	if strings.Contains(string(logged), "evil.example") {
		t.Fatalf("the glob-suffix bypass leaked into the request: %s", logged)
	}
	if !strings.Contains(string(logged), "github.com") {
		t.Fatalf("expected the real release host to be used instead, got: %s", logged)
	}
}

// TestFetchReleaseDottedSuffixBypassIsRejected covers
// "127.0.0.1.evil.example": the same glob would also match a real
// loopback prefix followed by extra DNS labels. is_loopback_host must
// reject it because splitting on '.' yields six fields, not the four a
// dotted quad requires.
func TestFetchReleaseDottedSuffixBypassIsRejected(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.52"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)
	attacker := newFixtureServer(t, map[string][]byte{asset: []byte("should never be fetched")})

	logFile := filepath.Join(t.TempDir(), "fake-curl.log")
	pathDir := pathWithFakeCurl(t, logFile)

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL="+strings.Replace(attacker.URL, "127.0.0.1", "127.0.0.1.evil.example", 1),
		"PATH="+pathDir,
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if attacker.Hits() != 0 {
		t.Fatalf("the dotted-suffix bypass's real target must never be contacted, got %d hits", attacker.Hits())
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fake curl was never invoked: %v\nscript output:\n%s", err, out)
	}
	if strings.Contains(string(logged), "evil.example") {
		t.Fatalf("the dotted-suffix bypass leaked into the request: %s", logged)
	}
	if !strings.Contains(string(logged), "github.com") {
		t.Fatalf("expected the real release host to be used instead, got: %s", logged)
	}
}

// TestFetchReleaseBracketedIPv6LoopbackOverrideIsHonored covers the other
// direction: "[::1]" (the URL literal form of the IPv6 loopback address)
// must be RECOGNIZED as loopback and the override honored, not rejected
// alongside the bypasses above. Verified the same way as
// TestFetchReleaseNonLoopbackOverrideIsIgnored — a fake curl records the
// URL it was asked to fetch — since a real fixture server bound to
// "[::1]" would make this test depend on IPv6 being available on the
// runner.
func TestFetchReleaseBracketedIPv6LoopbackOverrideIsHonored(t *testing.T) {
	skipOnWindows(t)
	osName, arch := hostAssetOSArch(t)
	version := "9.9.53"
	root := fakePluginRootWithManifest(t, version)
	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	writePin(t, root, version, strings.Repeat("a", 64)+"  "+asset)
	fs := newFixtureServer(t, map[string][]byte{})

	logFile := filepath.Join(t.TempDir(), "fake-curl.log")
	pathDir := pathWithFakeCurl(t, logFile)

	fixturePort := fs.URL[strings.LastIndex(fs.URL, ":")+1:]
	override := "http://[::1]:" + fixturePort

	out, code := runScript(t, root,
		"CLAUDE_MEM_GO_RELEASE_BASE_URL="+override,
		"PATH="+pathDir,
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if fs.Hits() != 0 {
		t.Fatalf("expected the fake curl to intercept the request before any real connection, got %d hits", fs.Hits())
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("fake curl was never invoked: %v\nscript output:\n%s", err, out)
	}
	if strings.Contains(string(logged), "github.com") {
		t.Fatalf("a real loopback override must not fall back to the release host: %s", logged)
	}
	if !strings.Contains(string(logged), "[::1]") {
		t.Fatalf("expected the bracketed IPv6 override to be used, got: %s", logged)
	}
}

// TestFetchReleaseVersionFallsBackToCheckoutTag covers version
// selection's fallback path: a manifest that parses but has no
// "version" field (not merely a missing file — this proves the
// fallback triggers on "no usable version in the manifest", not just
// "no manifest") sends resolve_version to the checkout's own exact tag
// instead.
func TestFetchReleaseVersionFallsBackToCheckoutTag(t *testing.T) {
	skipOnWindows(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	osName, arch := hostAssetOSArch(t)
	root := fakePluginRoot(t)
	writePluginManifest(t, root, `{"name":"claude-mem-go"}`)
	version := "9.9.42"
	gitInitAndTag(t, root, "v"+version)

	asset := fmt.Sprintf("claude-mem-go_%s_%s_%s.tar.gz", version, osName, arch)
	archiveBytes, sum := buildArchiveBytes(t, "#!/bin/sh\necho fetched-fake-binary\nexit 0\n")
	fs := newFixtureServer(t, map[string][]byte{asset: archiveBytes})
	writePin(t, root, version, sum+"  "+asset)

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	got, err := exec.Command(filepath.Join(root, "claude-mem-go")).CombinedOutput()
	if err != nil || !strings.Contains(string(got), "fetched-fake-binary") {
		t.Fatalf("did not fetch using the checkout's own tag as the version: %v / %q\nscript output:\n%s", err, got, out)
	}
}

// TestFetchReleaseSkipsWhenNoPinExists covers a checkout with a resolved
// version but no committed pin for it — a checkout that predates the pin
// PR merging, or (per docs/plugin-install.md) an exact-tag checkout
// whose pin hasn't reached the default branch yet. Must fall straight
// through to build, exactly like "no matching asset", without ever
// consulting the release host.
func TestFetchReleaseSkipsWhenNoPinExists(t *testing.T) {
	skipOnWindows(t)
	version := "9.9.43"
	root := fakePluginRootWithManifest(t, version)
	// Deliberately no scripts/checksums/9.9.43.txt.
	fs := newFixtureServer(t, map[string][]byte{})

	out, code := runScript(t, root, "CLAUDE_MEM_GO_RELEASE_BASE_URL="+fs.URL)
	if code != 0 {
		t.Fatalf("exit %d, want 0. Output:\n%s", code, out)
	}
	if fs.Hits() != 0 {
		t.Fatalf("no pin means no fetch attempt at all, got %d hits", fs.Hits())
	}
	assertBuiltFromSource(t, root, out)
}
