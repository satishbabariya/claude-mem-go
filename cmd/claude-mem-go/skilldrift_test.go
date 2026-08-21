package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMemDoctorSkillExplainsEveryCriticalFinding is a drift guard, and it
// exists because the drift already happened.
//
// `doctor` gained five checks over this project's recent work — the
// plugin-install check, the plugin-binary check, an empty-store-but-
// installed failure, a worker/store mismatch, and a stale-daemon
// notice — and the skill that tells Claude how to INTERPRET doctor's
// output was not updated for any of them. Three had zero mentions. A
// skill that describes a tool as it used to behave is worse than one
// that says nothing, because Claude acts on it.
//
// Nothing checked the skill against the tool, so nothing noticed. This
// does. It is deliberately scoped to CRITICAL findings: those are the
// ones a user is most likely to hit and least able to interpret, and
// demanding prose for every informational line would make the skill a
// transcript of the source.
func TestMemDoctorSkillExplainsEveryCriticalFinding(t *testing.T) {
	src, err := os.ReadFile("doctor.go")
	if err != nil {
		t.Fatalf("read doctor.go: %v", err)
	}
	skillPath := filepath.Join("..", "..", "skills", "mem-doctor", "SKILL.md")
	skillBytes, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}
	skill := strings.ToLower(string(skillBytes))

	// Every critical finding doctor can print. The ✘ marker is how doctor
	// itself distinguishes them, so this stays in sync with the tool
	// rather than with a hand-maintained list that could drift too.
	crit := regexp.MustCompile(`"✘ ([^"%\\]{6,60})`).FindAllStringSubmatch(string(src), -1)
	if len(crit) < 4 {
		t.Fatalf("found only %d critical findings in doctor.go — the extraction is probably broken, "+
			"which would make this guard silently pass forever", len(crit))
	}

	word := regexp.MustCompile(`[a-z]{4,}`)
	for _, m := range crit {
		phrase := m[1]
		words := word.FindAllString(strings.ToLower(phrase), -1)
		if len(words) > 3 {
			words = words[:3]
		}
		var missing []string
		for _, w := range words {
			if !strings.Contains(skill, w) {
				missing = append(missing, w)
			}
		}
		if len(missing) > 0 {
			t.Errorf("doctor can print the critical finding %q, but skills/mem-doctor/SKILL.md never "+
				"mentions %v — Claude would see the failure and have no guidance for it", phrase, missing)
		}
	}
}
