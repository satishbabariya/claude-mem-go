package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// Observation is what an observer session's XML reply decodes into — the
// same field set buildObservationPrompt (observer package) asks for, and
// the same one this package's schema persists.
type Observation struct {
	Type          string
	Title         string
	Subtitle      string
	Facts         []string
	Narrative     string
	Concepts      []string
	FilesRead     []string
	FilesModified []string
	// NextSteps is what was left unfinished — populated for session
	// summaries and empty for ordinary per-tool-call observations.
	//
	// Real claude-mem keeps a whole separate session_summaries table with
	// structured request/investigated/learned/completed/next_steps
	// columns; this port folds the session summary into the same
	// observations table, which loses nothing EXCEPT this field, because
	// title/subtitle/narrative/facts already carry the rest. Next steps
	// are different in kind: every other field records what happened,
	// while this one records what had not happened yet, which is exactly
	// what the NEXT session needs told to it first.
	NextSteps []string
}

// ValidObservationTypes is the actual, small, fixed vocabulary this
// project's own code ever produces for Observation.Type: three from the
// real observer prompt (observer.go's buildObservationPrompt — discovery/
// change/decision), one from the Stop hook's session-summary prompt
// (summary), and one hardcoded Go literal from add_observation (manual).
// Nothing in either backend's schema or Go code validated this before —
// a real schema-completeness gap, found by hand, not a demonstrated live
// bug: every real row in this project's own long-lived shared Postgres
// dev container (3000+ accumulated across this session's testing) was
// checked by hand and already falls within this vocabulary with zero
// drift, confirming this closes a real gap without breaking anything
// already persisted.
var ValidObservationTypes = map[string]bool{
	"discovery": true,
	"change":    true,
	"decision":  true,
	"summary":   true,
	"manual":    true,
}

// ValidateObservationType returns an error if t isn't one of
// ValidObservationTypes. Called at every real ingestion boundary in both
// backends (Insert and ImportRow, via their shared insertRow) rather
// than trusting an LLM's own <type> tag or an imported file's claim
// unconditionally — the one field every -type/type filter
// (search_observations, the CLI search subcommand) and this schema
// itself needs a small, known vocabulary for; nothing else about an
// Observation (title, narrative, facts, ...) is free-form-LLM-prose and
// deliberately left unconstrained.
func ValidateObservationType(t string) error {
	if !ValidObservationTypes[t] {
		return fmt.Errorf("invalid observation type %q (want one of discovery, change, decision, summary, manual)", t)
	}
	return nil
}

var xmlFenceRe = regexp.MustCompile("(?s)```(?:xml)?\\s*(.*?)\\s*```")

func tagRe(tag string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`)
}

func extractTag(s, tag string) string {
	m := tagRe(tag).FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// extractItems reads a wrapper block (e.g. <facts>...</facts>) and returns
// every <item>...</item> found inside it, in order.
func extractItems(s, block, item string) []string {
	blockMatch := tagRe(block).FindStringSubmatch(s)
	if blockMatch == nil {
		return nil
	}
	matches := tagRe(item).FindAllStringSubmatch(blockMatch[1], -1)
	var out []string
	for _, m := range matches {
		v := strings.TrimSpace(m[1])
		if v != "" && v != "..." {
			out = append(out, v)
		}
	}
	return out
}

// ParseXML pulls an Observation out of an observer session's raw text
// reply. Models routinely wrap XML in a ```xml code fence despite being
// told not to — strip that first, tolerantly: a shape we don't expect is
// worked around, not fatal.
func ParseXML(raw string) (Observation, error) {
	body := raw
	if m := xmlFenceRe.FindStringSubmatch(raw); m != nil {
		body = m[1]
	}
	if !strings.Contains(body, "<observation>") {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return Observation{}, fmt.Errorf("no <observation> block in model output: %q", snippet)
	}

	return Observation{
		Type:          extractTag(body, "type"),
		Title:         extractTag(body, "title"),
		Subtitle:      extractTag(body, "subtitle"),
		Facts:         extractItems(body, "facts", "fact"),
		Narrative:     extractTag(body, "narrative"),
		Concepts:      extractItems(body, "concepts", "concept"),
		FilesRead:     extractItems(body, "files_read", "file"),
		FilesModified: extractItems(body, "files_modified", "file"),
		// Parsed for every observation, though only the session-summary
		// prompt asks for it — an ordinary per-tool-call turn simply has
		// no <next_steps> block and yields nil, which is correct rather
		// than special-cased.
		NextSteps: extractItems(body, "next_steps", "step"),
	}, nil
}
