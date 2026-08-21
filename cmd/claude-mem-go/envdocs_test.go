package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryEnvVarIsDocumented guards a gap that is invisible by
// construction: an environment variable the code reads but no document
// mentions is unusable, because nobody can discover it.
//
// It caught a real one. `CLAUDE_MEM_LOG_LEVEL` was added, wired through
// every hook and the daemon, and recorded only in the CHANGELOG — so the
// README, the document an operator actually reads, never mentioned the
// one variable they need when diagnosing why an observation went missing.
//
// Deliberately one-directional: code → README. The reverse is a
// legitimate pattern here, since the README discusses real claude-mem's
// own variables (`CLAUDE_MEM_EXCLUDED_PROJECTS`) when explaining what
// this port does differently, and flagging those would be noise. That
// asymmetry was confirmed by checking the reverse direction by hand
// first, which produced exactly that false positive.
func TestEveryEnvVarIsDocumented(t *testing.T) {
	root := filepath.Join("..", "..")
	readmeBytes, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(readmeBytes)

	envRe := regexp.MustCompile(`"(CLAUDE_MEM[A-Z_]*)"`)
	found := map[string]string{}

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && (info.Name() == ".git" || info.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		// Test files declare throwaway variables (the Postgres test DSN)
		// that no operator needs to know about.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range envRe.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(found) < 5 {
		t.Fatalf("discovered only %d environment variables in the source — the extraction is "+
			"probably broken, which would leave this guard passing forever", len(found))
	}

	for name, where := range found {
		if !strings.Contains(readme, name) {
			t.Errorf("%s is read by %s but never appears in README.md — an environment variable "+
				"nobody can discover is one nobody can use", name, where)
		}
	}
}
