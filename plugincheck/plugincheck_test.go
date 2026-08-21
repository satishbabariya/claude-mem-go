package plugincheck

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPluginNameMatchesManifest is the drift guard for PluginName's own
// doc comment: the constant exists because doctor can't read the repo's
// manifest at runtime, but nothing else would ever notice if the two
// disagreed — the exact shape of silent staleness this project already
// found once in the MCP server's hardcoded serverInfo.version. Reads the
// REAL .claude-plugin/plugin.json, not a fixture.
func TestPluginNameMatchesManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("reading the real plugin manifest: %v", err)
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parsing the real plugin manifest: %v", err)
	}
	if manifest.Name != PluginName {
		t.Fatalf("PluginName = %q but .claude-plugin/plugin.json says %q — doctor's plugin-installed check would silently never match", PluginName, manifest.Name)
	}
}

// writeManifest writes an installed-plugins manifest with the given
// plugin keys, shaped exactly like the real file (captured from a real
// machine — see installedPluginsFile's own doc comment).
func writeManifest(t *testing.T, keys map[string][]Install) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "installed_plugins.json")
	raw, err := json.Marshal(installedPluginsFile{Version: 2, Plugins: keys})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestIsInstalledFindsThePluginRegardlessOfMarketplace(t *testing.T) {
	// The marketplace half deliberately differs from this repo's own
	// marketplace.json name: installing the same plugin from a
	// GitHub-hosted marketplace instead of the local one must still read
	// as installed.
	path := writeManifest(t, map[string][]Install{
		"some-other-plugin@claude-plugins-official": {{Scope: "user", Version: "1.0.0"}},
		"claude-mem-go@some-github-marketplace":     {{Scope: "user", Version: "0.3.0", InstallPath: "/somewhere"}},
	})

	installed, found := IsInstalled(path)
	if !installed {
		t.Fatal("IsInstalled = false, want true — the plugin is present under a differently-named marketplace")
	}
	if len(found) != 1 || found[0].Version != "0.3.0" || found[0].Scope != "user" {
		t.Fatalf("IsInstalled returned %+v, want the single user-scope 0.3.0 install", found)
	}
}

func TestIsInstalledReturnsEveryScope(t *testing.T) {
	path := writeManifest(t, map[string][]Install{
		"claude-mem-go@claude-mem-go-local": {
			{Scope: "user", Version: "0.3.0"},
			{Scope: "project", Version: "0.3.0"},
		},
	})

	installed, found := IsInstalled(path)
	if !installed || len(found) != 2 {
		t.Fatalf("IsInstalled = (%v, %+v), want true with both scopes reported", installed, found)
	}
}

func TestIsInstalledFalseWhenAbsent(t *testing.T) {
	path := writeManifest(t, map[string][]Install{
		"feature-dev@claude-plugins-official": {{Scope: "user"}},
	})
	if installed, _ := IsInstalled(path); installed {
		t.Fatal("IsInstalled = true for a manifest that doesn't list this plugin, want false")
	}
}

// TestIsInstalledToleratesAMissingOrBrokenManifest covers the real case
// of a machine that has never installed any plugin (no file at all), and
// a corrupt one — both are "not installed," not a crash or an error the
// caller has to special-case.
func TestIsInstalledToleratesAMissingOrBrokenManifest(t *testing.T) {
	if installed, _ := IsInstalled(filepath.Join(t.TempDir(), "does-not-exist.json")); installed {
		t.Error("IsInstalled = true for a nonexistent manifest, want false")
	}

	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if installed, _ := IsInstalled(broken); installed {
		t.Error("IsInstalled = true for an unparseable manifest, want false")
	}

	if installed, _ := IsInstalled(""); installed {
		t.Error("IsInstalled = true for an empty path, want false")
	}
}

// TestIsInstalledMatchesAKeyWithNoMarketplaceSuffix locks in the
// deliberate fallback in IsInstalled: a key that doesn't carry the
// documented "@marketplace" suffix is compared whole rather than
// skipped, so an unexpected manifest variant reads as installed instead
// of silently reporting a false negative.
func TestIsInstalledMatchesAKeyWithNoMarketplaceSuffix(t *testing.T) {
	path := writeManifest(t, map[string][]Install{
		"claude-mem-go": {{Scope: "user", Version: "0.3.0"}},
	})
	if installed, _ := IsInstalled(path); !installed {
		t.Fatal("IsInstalled = false for a bare (no-@) key matching the plugin name, want true")
	}
}

// buildTestBinary compiles this project's own binary into dir under the
// name BinaryStatus looks for — a REAL build, not a shell-script stand-in,
// so the exec probe is exercised against the same kind of artifact a real
// install contains (a script would pass a probe that a wrong-architecture
// binary should fail).
func buildTestBinary(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, PluginName), "./cmd/claude-mem-go")
	cmd.Dir = ".."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the test binary: %v\n%s", err, out)
	}
}

// TestBinaryStatusOnARealBuild is the positive case: a real compiled
// binary at the install path reports its own version string.
func TestBinaryStatusOnARealBuild(t *testing.T) {
	dir := t.TempDir()
	buildTestBinary(t, dir)

	version, err := BinaryStatus(Install{InstallPath: dir})
	if err != nil {
		t.Fatalf("BinaryStatus on a real build: %v", err)
	}
	if !strings.Contains(version, PluginName) {
		t.Fatalf("BinaryStatus returned %q, want the binary's own version string mentioning %q", version, PluginName)
	}
}

// TestBinaryStatusCatchesAMissingBinary is the exact real-world failure
// this whole check exists for: the plugin is installed and well-formed,
// but the gitignored binary every hook invokes was never built into it.
func TestBinaryStatusCatchesAMissingBinary(t *testing.T) {
	_, err := BinaryStatus(Install{InstallPath: t.TempDir()})
	if err == nil {
		t.Fatal("BinaryStatus with no binary present: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "go build") {
		t.Fatalf("error = %v, want it to name the remediation (`go build ...`)", err)
	}
}

// TestBinaryStatusCatchesANonExecutableBinary covers the shape a naive
// archive extraction produces — present, right name, no execute bit.
func TestBinaryStatusCatchesANonExecutableBinary(t *testing.T) {
	dir := t.TempDir()
	buildTestBinary(t, dir)
	path := filepath.Join(dir, PluginName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	_, err := BinaryStatus(Install{InstallPath: dir})
	if err == nil {
		t.Fatal("BinaryStatus on a non-executable binary: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("error = %v, want it to say the binary is not executable", err)
	}
}

// TestBinaryStatusCatchesAnUnrunnableBinary covers the wrong-architecture
// / corrupt-build case: something that stats perfectly as an executable
// regular file but cannot actually run. This is precisely why the check
// execs rather than only stat-ing.
func TestBinaryStatusCatchesAnUnrunnableBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PluginName)
	if err := os.WriteFile(path, []byte("\x7fELF this is not a runnable mach-o binary"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := BinaryStatus(Install{InstallPath: dir})
	if err == nil {
		t.Fatal("BinaryStatus on a corrupt binary: want an error, got nil — a stat-only check would have passed this")
	}
	if !strings.Contains(err.Error(), "could not be executed") {
		t.Fatalf("error = %v, want it to report an execution failure", err)
	}
}

func TestBinaryStatusRejectsAnEmptyInstallPath(t *testing.T) {
	if _, err := BinaryStatus(Install{}); err == nil {
		t.Fatal("BinaryStatus with no installPath: want an error, got nil")
	}
}

// TestBinaryStatusRejectsADirectory covers an installPath containing a
// DIRECTORY named like the binary — stats fine, is not runnable.
func TestBinaryStatusRejectsADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, PluginName), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	_, err := BinaryStatus(Install{InstallPath: dir})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("BinaryStatus with a directory in the binary's place = %v, want a not-a-regular-file error", err)
	}
}
