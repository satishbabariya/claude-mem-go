package observer

import (
	"strings"
	"testing"

	"claude-mem-go/store"
)

func TestBuildSummaryPromptIncludesEveryObservationInOrder(t *testing.T) {
	observations := []store.SearchResult{
		{Observation: store.Observation{Type: "discovery", Title: "Found the bug", Subtitle: "in the parser"}},
		{Observation: store.Observation{Type: "change", Title: "Fixed the parser"}},
	}
	prompt := BuildSummaryPrompt(observations)

	if !strings.Contains(prompt, "Found the bug") || !strings.Contains(prompt, "in the parser") {
		t.Error("prompt does not mention the first observation's title/subtitle")
	}
	if !strings.Contains(prompt, "Fixed the parser") {
		t.Error("prompt does not mention the second observation's title")
	}
	if strings.Index(prompt, "Found the bug") > strings.Index(prompt, "Fixed the parser") {
		t.Error("observations appear out of order in the prompt")
	}
	if !strings.Contains(prompt, "<type>summary</type>") {
		t.Error("prompt does not ask for type=summary — would produce an observation indistinguishable from a per-tool-call one")
	}
}

func TestBuildSummaryPromptHandlesNoSubtitle(t *testing.T) {
	observations := []store.SearchResult{
		{Observation: store.Observation{Type: "discovery", Title: "Just a title"}},
	}
	prompt := BuildSummaryPrompt(observations)
	if strings.Contains(prompt, "Just a title — ") {
		t.Error("prompt should not print a dangling separator for an empty subtitle")
	}
}
