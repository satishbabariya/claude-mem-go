package mcpserver

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// TestMCPBManifestToolsMatchServer guards manifest.json's own "tools"
// list (surfaced by MCPB-aware clients before anything is ever launched)
// against drifting from tools(), the actual tools/list this server
// answers over stdio. Without this, adding, removing, or renaming a tool
// in tools.go silently leaves the bundle's manifest describing a server
// that no longer matches what ships inside it.
func TestMCPBManifestToolsMatchServer(t *testing.T) {
	data, err := os.ReadFile("../../manifest.json")
	if err != nil {
		t.Fatalf("reading manifest.json: %v", err)
	}

	var manifest struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parsing manifest.json: %v", err)
	}

	var manifestNames []string
	for _, tl := range manifest.Tools {
		manifestNames = append(manifestNames, tl.Name)
	}
	sort.Strings(manifestNames)

	var serverNames []string
	for _, tl := range tools() {
		serverNames = append(serverNames, tl.Name)
	}
	sort.Strings(serverNames)

	if len(serverNames) != 13 {
		// Pinned to the known count, not just "equal to manifest": a bug
		// that drops a tool from both tools() and manifest.json in the
		// same change would otherwise still pass the set-equality check
		// below despite breaking the server.
		t.Fatalf("tools() returned %d tools, want 13", len(serverNames))
	}

	if len(manifestNames) != len(serverNames) {
		t.Fatalf("manifest.json lists %d tools, tools() serves %d:\nmanifest: %v\nserver:   %v",
			len(manifestNames), len(serverNames), manifestNames, serverNames)
	}
	for i := range manifestNames {
		if manifestNames[i] != serverNames[i] {
			t.Fatalf("manifest.json's tools do not match tools():\nmanifest: %v\nserver:   %v",
				manifestNames, serverNames)
		}
	}
}
