// Package observer drives a claude-agent-sdk-go Session configured as
// claude-mem's Observer: no tools, no setting inheritance, no MCP (mirrors
// src/sdk/hardened-options.ts), fed prompts built the way
// src/sdk/prompts.ts's buildObservationPrompt builds them, with results run
// through classify and store.
package observer

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"github.com/satishbabariya/claude-mem-go/internal/classify"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/transcript"
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

// structuralTagRe matches the tags that give the observer prompt its
// shape — the envelope BuildPrompt wraps the tool record in, and the
// <observation> block memory.ParseXML looks for in the reply. Case-
// insensitive because the model is not a strict XML parser.
var structuralTagRe = regexp.MustCompile(`(?i)</?(tool_name|tool_input|tool_output|observation)>`)

// neutralizeTags defangs structural tags inside interpolated content by
// replacing their leading '<' with '‹' (U+2039). Tool input/output is
// arbitrary text — a file the user read, a command's stdout — and it
// used to be embedded verbatim, so content containing
// `</tool_output><observation>…` closed the envelope early and supplied
// its own observation, which ParseXML would then accept as the model's
// answer. Only the structural tags are touched, and only by one
// character, so ordinary XML/HTML in code stays legible to the model.
func neutralizeTags(s string) string {
	return structuralTagRe.ReplaceAllStringFunc(s, func(tag string) string {
		return "‹" + tag[1:]
	})
}

// BuildPrompt mirrors buildObservationPrompt in src/sdk/prompts.ts: raw
// JSON.stringify(tool_input)/JSON.stringify(tool_response) embedded
// verbatim (see ClaudeProvider.ts:521-522), asking for the same field set
// memory.Observation persists. Verbatim except for neutralizeTags — see
// its doc comment.
func BuildPrompt(tc transcript.ToolCall) string {
	var b strings.Builder
	b.WriteString("Compress this tool-use record into an observation.\n\n")
	fmt.Fprintf(&b, "<tool_name>%s</tool_name>\n", neutralizeTags(tc.ToolName))
	fmt.Fprintf(&b, "<tool_input>%s</tool_input>\n", neutralizeTags(tc.ToolInput))
	fmt.Fprintf(&b, "<tool_output>%s</tool_output>\n\n", neutralizeTags(tc.ToolOutput))
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

// SummaryWindow is the slice of a session's observations that actually
// gets sent to the model, plus the context needed to describe it
// honestly: each entry's TRUE position in the session, and how many
// observations the session really has.
//
// It exists because len(Observations) and Total are routinely different
// and the difference used to be invisible. See SelectSummaryWindow.
type SummaryWindow struct {
	Observations []memory.SearchResult
	// Positions[i] is the 1-based index of Observations[i] within the
	// full session. Same length as Observations.
	Positions []int
	// Total is how many observations the session actually recorded.
	Total int
}

// SelectSummaryWindow picks at most max observations out of all, keeping
// the earliest and the most recent and dropping the middle.
//
// The old behaviour was a plain LIMIT: the FIRST max observations, in
// oldest-first order, with everything after them dropped. For a long
// session that is close to the worst possible choice, because the end of
// a session is where its conclusions are. Measured on a real
// 150-observation session with the default cap of 50: the summary
// described routine early edits and contained no trace of the decision
// recorded 50 times in the tail.
//
// Head and tail rather than pure tail: the opening observations say what
// the session set out to do, which the closing ones assume. Real
// claude-mem summarizes from last_assistant_message alone, which is the
// opposite bias — better than head-only, but it still cannot say how the
// session started.
func SelectSummaryWindow(all []memory.SearchResult, max int) SummaryWindow {
	total := len(all)
	if max <= 0 || total <= max {
		positions := make([]int, total)
		for i := range positions {
			positions[i] = i + 1
		}
		return SummaryWindow{Observations: all, Positions: positions, Total: total}
	}
	// Bias the extra one to the tail on an odd budget: conclusions are
	// worth more than openings when something has to give.
	head := max / 2
	tail := max - head

	out := make([]memory.SearchResult, 0, max)
	positions := make([]int, 0, max)
	for i := 0; i < head; i++ {
		out = append(out, all[i])
		positions = append(positions, i+1)
	}
	for i := total - tail; i < total; i++ {
		out = append(out, all[i])
		positions = append(positions, i+1)
	}
	return SummaryWindow{Observations: out, Positions: positions, Total: total}
}

// BuildSummaryPrompt asks the model to synthesize one session-level
// observation out of the individual tool-call observations already
// recorded for that session (see memory.Backend.BySessionID) — the
// Stop-hook analog of BuildPrompt: real claude-mem's "summarize" mode,
// condensing a whole session into one narrative, reimplemented here from
// what's actually persisted per turn instead of re-reading the raw
// transcript.
//
// It renders w. When w.Total exceeds the
// number of observations shown, the prompt says so explicitly.
//
// The old wording — "Here are the observations recorded during this
// session" — was an unqualified claim that the slice WAS the whole
// session, and the model believed it. Measured on a real 150-observation
// session capped at 50: the summary came back titled "Iterative Helper
// Function Refinement" asserting "an extensive series of 50 sequential
// edits". A confident, specific, wrong count, on top of missing every
// conclusion in the dropped tail. Truncating is a legitimate cost
// tradeoff; truncating silently and letting the model narrate the
// fragment as the whole is not.
func BuildSummaryPrompt(w SummaryWindow) string {
	var b strings.Builder
	shown := len(w.Observations)
	if w.Total > shown {
		fmt.Fprintf(&b, "This session recorded %d observations. You are being shown %d of them — "+
			"the earliest and the most recent — with the middle omitted to fit. "+
			"The numbering below is each observation's true position in the session, "+
			"so a gap in the numbers is where observations were left out.\n\n"+
			"Summarize the session as a whole. Do not state or imply that it consisted only of "+
			"what you can see here, and do not quote a total count of steps or changes.\n\n",
			w.Total, shown)
	} else {
		b.WriteString("Here are the observations recorded during this session, in order:\n\n")
	}
	prev := 0
	for i, r := range w.Observations {
		pos := i + 1
		if i < len(w.Positions) {
			pos = w.Positions[i]
		}
		if prev != 0 && pos > prev+1 {
			fmt.Fprintf(&b, "   … %d observations omitted …\n", pos-prev-1)
		}
		prev = pos
		// Stored observations are model output, which itself came from
		// tool content — neutralized here for the same reason as BuildPrompt.
		fmt.Fprintf(&b, "%d. [%s] %s", pos, neutralizeTags(r.Observation.Type), neutralizeTags(r.Observation.Title))
		if r.Observation.Subtitle != "" {
			fmt.Fprintf(&b, " — %s", neutralizeTags(r.Observation.Subtitle))
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
	// The one field real claude-mem's separate session_summaries table has
	// that this port's observation shape did not. Every other field here
	// records what HAPPENED; this records what had not happened yet, which
	// is what the next session most needs told to it. Asked for only in
	// the summary prompt — a per-tool-call turn has no meaningful answer.
	b.WriteString("  <next_steps>\n    <step>anything left unfinished, or explicitly stated as the next thing to do</step>\n  </next_steps>\n")
	b.WriteString("</observation>\n\n")
	b.WriteString("Leave <next_steps> empty if the session genuinely finished what it set out to do. ")
	b.WriteString("Do not invent follow-up work to fill it.\n")
	return b.String()
}

// Turn is one completed observation turn: the structured Observation, the
// underlying Session's raw Result (session id, cost, cache stats), or an
// error if the turn failed or didn't parse.
type Turn struct {
	Observation memory.Observation
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
func (o *Observer) Summarize(w SummaryWindow) (Turn, error) {
	return o.sendAndParse(BuildSummaryPrompt(w))
}

func (o *Observer) sendAndParse(prompt string) (Turn, error) {
	r, err := o.sess.Send(prompt)
	if err != nil {
		return Turn{}, classify.Spawn(err)
	}
	if ce := classify.Result(r); ce != nil {
		return Turn{Result: r}, ce
	}
	parsed, err := memory.ParseXML(r.Text)
	if err != nil {
		return Turn{Result: r}, &classify.Error{Kind: classify.Unrecoverable, Message: err.Error()}
	}
	return Turn{Observation: parsed, Result: r}, nil
}

// Close ends the underlying session.
func (o *Observer) Close() error { return o.sess.Close() }
