// Package plugincheck answers one question doctor previously had no way
// to ask: is this project actually installed as a Claude Code plugin?
//
// It matters more than any other single check. Every automatic capture
// path in this project — SessionStart, UserPromptSubmit, PreToolUse,
// PostToolUse, Stop — runs only because hooks/hooks.json is wired in by a
// real plugin installation. Without it, the binary still works fine for
// CLI use (search, export, prune) and as an MCP server, but nothing is
// ever captured automatically: no observations, no memory, the entire
// point of the project silently inert. Before this, doctor would report
// "All critical checks passed" in exactly that state.
//
// Real claude-mem's own doctor (src/npx-cli/commands/doctor.ts) has the
// same check and marks it required — see IsInstalled's doc comment for
// why this port surfaces it prominently but not as a hard failure.
package plugincheck

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// PluginName is this plugin's name as Claude Code knows it — the "name"
// field of .claude-plugin/plugin.json. Kept as a constant rather than
// read from that manifest at runtime because doctor runs as an installed
// binary that may live anywhere, with no reliable path back to the repo
// it was built from. TestPluginNameMatchesManifest reads the real
// manifest and fails if these ever drift apart, so this can't silently
// go stale the way the MCP server's own hardcoded serverInfo.version
// once did.
const PluginName = "claude-mem-go"

// Install is one recorded installation of a plugin — Claude Code allows
// the same plugin at more than one scope (user and project), so
// IsInstalled returns every match rather than just the first.
type Install struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	Version     string `json:"version"`
}

// installedPluginsFile is the on-disk shape of
// ~/.claude/plugins/installed_plugins.json, captured from a real file on
// a machine with 18 plugins installed rather than guessed: a version
// number plus a map keyed by "<plugin-name>@<marketplace-name>", each
// value an array of installs (one per scope).
type installedPluginsFile struct {
	Version int                  `json:"version"`
	Plugins map[string][]Install `json:"plugins"`
}

// DefaultManifestPath is where Claude Code records installed plugins.
func DefaultManifestPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
}

// IsInstalled reports whether PluginName appears in the installed-plugins
// manifest at path, and every scope it's installed at.
//
// Matching is on the plugin-name half of each "<name>@<marketplace>" key,
// not the whole key: the same plugin installed from a different
// marketplace (this repo's own .claude-plugin/marketplace.json when
// developing, a GitHub-hosted one otherwise) is still installed, and a
// health check that only recognized one specific marketplace would report
// a false negative for a perfectly working setup.
//
// A missing or unparseable manifest is reported as "not installed" with
// no error — a machine that has never installed any plugin simply has no
// such file, which is a real answer to the question, not a failure to
// answer it.
func IsInstalled(path string) (bool, []Install) {
	if path == "" {
		return false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	var f installedPluginsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return false, nil
	}
	var found []Install
	for key, installs := range f.Plugins {
		name, _, ok := strings.Cut(key, "@")
		if !ok {
			// A key with no "@" isn't the documented shape; compare the
			// whole thing rather than skipping it, so an unexpected
			// variant can still match instead of silently reading as
			// "not installed."
			name = key
		}
		if name == PluginName {
			found = append(found, installs...)
		}
	}
	return len(found) > 0, found
}
