// Package observer drives a claude-agent-sdk-go Session configured as
// claude-mem's Observer: no tools, no setting inheritance, no MCP (mirrors
// src/sdk/hardened-options.ts), fed prompts built the way
// src/sdk/prompts.ts's buildObservationPrompt builds them, with results run
// through classify and store.
package observer

import (
	"context"
	"fmt"
	"strings"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"claude-mem-go/classify"
	"claude-mem-go/store"
	"claude-mem-go/transcript"
)

// DisallowedTools mirrors OBSERVER_DISALLOWED_TOOLS in hardened-options.ts —
// the "suspenders" layer, redundant by design with Tools: []string{} (the
// "belt" — see HardenedOptions).
var DisallowedTools = []string{
	"Bash", "Read", "Write", "Edit", "Grep", "Glob",
	"WebFetch", "WebSearch", "Task", "NotebookEdit", "AskUserQuestion", "TodoWrite",
}

const systemPrompt = `You are an Observer. You do not have access to tools. ` +
	`You read a single tool-use record from a coding session and produce a compressed ` +
	`observation of it in the exact XML shape requested. Output nothing but that XML.`

// HardenedOptions builds the claudeagent.Options an Observer session always
// uses: Tools: []string{} is the true restrictive allowlist (disables every
// built-in), DisallowedTools is the redundant explicit deny-list,
// SettingSources: []SettingSource{} means no user/project/local settings —
// no inherited hooks/skills/plugins — and StrictMCPConfig with no MCP
// config passed means zero MCP servers. Removing any one layer must not
// reopen the gap — see hardened-options.ts's threat-model comment, which
// this is a direct port of.
func HardenedOptions(model string) claudeagent.Options {
	return claudeagent.Options{
		Model:           model,
		SystemPrompt:    systemPrompt,
		PermissionMode:  claudeagent.PermissionModeDontAsk,
		Tools:           []string{},
		DisallowedTools: DisallowedTools,
		SettingSources:  []claudeagent.SettingSource{},
		StrictMCPConfig: true,
	}
}

// BuildPrompt mirrors buildObservationPrompt in src/sdk/prompts.ts: raw
// JSON.stringify(tool_input)/JSON.stringify(tool_response) embedded
// verbatim (see ClaudeProvider.ts:521-522), asking for the same field set
// store.Observation persists.
func BuildPrompt(tc transcript.ToolCall) string {
	var b strings.Builder
	b.WriteString("Compress this tool-use record into an observation.\n\n")
	fmt.Fprintf(&b, "<tool_name>%s</tool_name>\n", tc.ToolName)
	fmt.Fprintf(&b, "<tool_input>%s</tool_input>\n", tc.ToolInput)
	fmt.Fprintf(&b, "<tool_output>%s</tool_output>\n\n", tc.ToolOutput)
	b.WriteString("Respond with exactly this shape (omit a list's items if there are none):\n\n")
	b.WriteString("<observation>\n")
	b.WriteString("  <type>[ discovery | change | decision ]</type>\n")
	b.WriteString("  <title>short title</title>\n")
	b.WriteString("  <subtitle>one-line detail</subtitle>\n")
	b.WriteString("  <facts>\n    <fact>...</fact>\n    <fact>...</fact>\n  </facts>\n")
	b.WriteString("  <narrative>one paragraph</narrative>\n")
	b.WriteString("  <concepts>\n    <concept>...</concept>\n  </concepts>\n")
	b.WriteString("  <files_read>\n    <file>...</file>\n  </files_read>\n")
	b.WriteString("  <files_modified>\n    <file>...</file>\n  </files_modified>\n")
	b.WriteString("</observation>\n")
	return b.String()
}

// BuildSummaryPrompt asks the model to synthesize one session-level
// observation out of the individual tool-call observations already
// recorded for that session (see store.Backend.BySessionID) — the Stop-hook
// analog of BuildPrompt: real claude-mem's "summarize" mode, condensing a
// whole session's user_prompt/last_assistant_message into one narrative,
// reimplemented here from what's actually persisted per turn instead of
// re-reading the raw transcript.
func BuildSummaryPrompt(observations []store.SearchResult) string {
	var b strings.Builder
	b.WriteString("Here are the observations recorded during this session, in order:\n\n")
	for i, r := range observations {
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, r.Observation.Type, r.Observation.Title)
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, " — %s", r.Observation.Subtitle)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nSynthesize these into a single session-level summary. ")
	b.WriteString("Respond with exactly this shape (omit a list's items if there are none):\n\n")
	b.WriteString("<observation>\n")
	b.WriteString("  <type>summary</type>\n")
	b.WriteString("  <title>short title for the whole session</title>\n")
	b.WriteString("  <subtitle>one-line detail</subtitle>\n")
	b.WriteString("  <facts>\n    <fact>...</fact>\n    <fact>...</fact>\n  </facts>\n")
	b.WriteString("  <narrative>one paragraph describing what was accomplished this session</narrative>\n")
	b.WriteString("  <concepts>\n    <concept>...</concept>\n  </concepts>\n")
	b.WriteString("</observation>\n")
	return b.String()
}

// Turn is one completed observation turn: the structured Observation, the
// underlying Session's raw Result (session id, cost, cache stats), or an
// error if the turn failed or didn't parse.
type Turn struct {
	Observation store.Observation
	Result      claudeagent.Result
}

// Handle is the minimal interface a caller needs to drive and dispose of an
// observer session: *Observer satisfies it. Exists so ObserveResilient (and
// anything caching/reusing sessions, like claude-mem-go's worker) can
// depend on this instead of the concrete type — useful for substituting a
// fake in tests without spawning a real claude subprocess.
type Handle interface {
	Observe(tc transcript.ToolCall) (Turn, error)
	Close() error
}

// Observer wraps one persistent claudeagent.Session in the Observer role.
// Like the Session it wraps, multiple Observe calls share context/session
// id — this is what lets claude-mem's real per-user-session conversation
// (init prompt, then one turn per tool call, then a summary) work as one
// ongoing exchange instead of independent one-shot calls.
type Observer struct {
	sess *claudeagent.Session
}

// New spawns a hardened observer session for the given model.
func New(ctx context.Context, model string) (*Observer, error) {
	sess, err := claudeagent.NewSession(ctx, HardenedOptions(model))
	if err != nil {
		return nil, classify.Spawn(err)
	}
	return &Observer{sess: sess}, nil
}

// Observe sends one tool call as a turn and returns its parsed observation.
// The returned error, when non-nil, is always a *classify.Error — from a
// spawn/transport failure, a classified provider error, or (wrapped
// plainly) a parse failure when the model didn't reply in the requested
// shape.
func (o *Observer) Observe(tc transcript.ToolCall) (Turn, error) {
	return o.sendAndParse(BuildPrompt(tc))
}

// Summarize sends a session's already-recorded observations as one turn and
// returns the synthesized session-level observation — the Stop-hook path
// (see BuildSummaryPrompt).
func (o *Observer) Summarize(observations []store.SearchResult) (Turn, error) {
	return o.sendAndParse(BuildSummaryPrompt(observations))
}

func (o *Observer) sendAndParse(prompt string) (Turn, error) {
	r, err := o.sess.Send(prompt)
	if err != nil {
		return Turn{}, classify.Spawn(err)
	}
	if ce := classify.Result(r); ce != nil {
		return Turn{Result: r}, ce
	}
	parsed, err := store.ParseXML(r.Text)
	if err != nil {
		return Turn{Result: r}, &classify.Error{Kind: classify.Unrecoverable, Message: err.Error()}
	}
	return Turn{Observation: parsed, Result: r}, nil
}

// Close ends the underlying session.
func (o *Observer) Close() error { return o.sess.Close() }
