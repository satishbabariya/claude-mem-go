package main

// hookOutput is the JSON shape Claude Code expects back on stdout from a
// hook that injects context — confirmed against real sessions, not guessed
// from docs: a SessionStart hook injecting a skill this exact way was
// observed directly, and the same shape with hookEventName="PreToolUse"
// was independently verified for the file-context hook (a real, distinctive
// marker round-tripped through an actual Read tool call).
type hookOutput struct {
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}
