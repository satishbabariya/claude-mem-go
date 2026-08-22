package mcpserver

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestSkillsOnlyReferenceRealToolsAndParameters guards the skill surface
// against the drift that already happened once in this project.
//
// The mem-doctor skill was found describing a version of `doctor` that no
// longer existed, because nothing compared the two. The same hazard
// applies more sharply to MCP tool calls: a skill is an instruction Claude
// acts on, so a tool name or parameter that no longer exists does not
// produce a documentation nit — it produces a failed tool call in a real
// session, at the moment the user asked for something.
//
// This compares every tool call written in every skill against the server
// that actually serves them. It validates only calls whose name IS a real
// tool, so ordinary prose containing parentheses cannot produce a false
// failure.
func TestSkillsOnlyReferenceRealToolsAndParameters(t *testing.T) {
	tools := toolsByName(t)
	if len(tools) < 8 {
		t.Fatalf("discovered only %d tools from the server definition — the extraction is probably "+
			"broken, which would leave this guard passing forever while checking nothing", len(tools))
	}

	skills, err := filepath.Glob(filepath.Join("..", "..", "skills", "*", "SKILL.md"))
	if err != nil || len(skills) == 0 {
		t.Fatalf("no skills found to check (%v)", err)
	}

	call := regexp.MustCompile(`\b([a-z_]{4,})\(([^)]*)\)`)
	arg := regexp.MustCompile(`([a-zA-Z_]+)\s*[=:]`)

	checked := 0
	for _, path := range skills {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range call.FindAllStringSubmatch(string(b), -1) {
			name, args := m[1], m[2]
			params, ok := tools[name]
			if !ok {
				continue // not a tool call, just prose
			}
			checked++
			for _, a := range arg.FindAllStringSubmatch(args, -1) {
				p := a[1]
				if _, ok := params[p]; !ok {
					t.Errorf("%s calls %s(%s) with parameter %q, which that tool does not accept.\n"+
						"  accepted: %v\n"+
						"  A skill is acted on, so this is a failed tool call in a real session, not a typo.",
						filepath.Base(filepath.Dir(path)), name, args, p, sortedKeys(params))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no tool calls were found in any skill — either the skills stopped showing examples, " +
			"or the matcher broke; both make this guard meaningless")
	}
	t.Logf("validated %d tool call(s) across %d skills", checked, len(skills))
}

// toolsByName reads the server's real tool table — tools() and each
// InputSchema's "properties" — so the guard tracks the live surface rather
// than a regex over source text that a file split or refactor could
// silently turn into "0 tools found".
func toolsByName(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, td := range tools() {
		params := map[string]bool{}
		if props, ok := td.InputSchema["properties"].(map[string]any); ok {
			for name := range props {
				params[name] = true
			}
		}
		out[td.Name] = params
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

var _ = strings.TrimSpace
