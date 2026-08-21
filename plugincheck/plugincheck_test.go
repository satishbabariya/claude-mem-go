package plugincheck

import (
	"encoding/json"
	"os"
	"path/filepath"
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
