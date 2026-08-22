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

	skills, err := filepath.Glob(filepath.Join("..", "skills", "*", "SKILL.md"))
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

// toolsByName parses the server's own tool definitions, so the guard
// tracks the real surface rather than a hand-maintained list that could
// drift exactly as the skills did.
func toolsByName(t *testing.T) map[string]map[string]bool {
	t.Helper()
	src, err := os.ReadFile("mcpserver.go")
	if err != nil {
		t.Fatalf("read mcpserver.go: %v", err)
	}
	out := map[string]map[string]bool{}
	nameRe := regexp.MustCompile(`Name:\s*"([a-z_]+)"`)
	propRe := regexp.MustCompile(`"([a-zA-Z_]+)":\s*map\[string\]any\{"type"`)

	locs := nameRe.FindAllStringSubmatchIndex(string(src), -1)
	for i, loc := range locs {
		name := string(src[loc[2]:loc[3]])
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := string(src[loc[1]:end])
		params := map[string]bool{}
		for _, p := range propRe.FindAllStringSubmatch(block, -1) {
			params[p[1]] = true
		}
		out[name] = params
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
