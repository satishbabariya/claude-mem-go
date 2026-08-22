package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"unicode/utf8"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/contextfmt"
	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/excludeproject"
	"github.com/satishbabariya/claude-mem-go/internal/hook"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
	"github.com/satishbabariya/claude-mem-go/internal/privacy"
	"github.com/satishbabariya/claude-mem-go/internal/worker"
)

// cmdPromptContext is the UserPromptSubmit hook — real claude-mem's own
// session-init handler covers the identical real gap: SessionStart's
// context injection (cmdContext) is a static "most recent observations"
// dump, decided before Claude has any idea what the user is about to ask.
// This hook fires on the actual submitted prompt text, embeds it, and
// injects the observations semantically closest to THIS question — a
// sharper form of recall SessionStart can't provide on its own, not just a
// duplicate of it. See real claude-mem's src/cli/handlers/session-init.ts,
// which does the same "embed the prompt, semantic-search, inject" shape
// against its own server-backed store.
//
// -embed-model is this hook's on/off switch, the same convention cmdMCP and
// cmdStop already use: empty disables it (a plain "{}", no injection,
// no error) rather than a separate settings flag real claude-mem needs
// because its embedding call goes through a hosted API with its own cost
// implications; this project's embeddings are a local, free Ollama call,
// so there's no separate reason to default it off once a model IS
// configured.
//
// minPromptLen mirrors real claude-mem's own `prompt.length >= 20` guard:
// short prompts ("yes", "continue", "ok") embed to noise, not a
// meaningful semantic anchor, and would waste an Ollama round-trip on
// every single message for no benefit.
func cmdPromptContext(args []string) int {
	fs := flag.NewFlagSet("prompt-context", flag.ExitOnError)
	dbPath := cli.DBFlag(fs)
	embedModel := fs.String("embed-model", cli.DefaultEmbedModel, "Ollama model for embedding the prompt (empty disables this hook)")
	limit := fs.Int("limit", 5, "how many semantically relevant observations to inject")
	minPromptLen := fs.Int("min-prompt-len", 20, "prompts shorter than this are skipped, not embedded")
	hnswEfSearch := fs.Int("hnsw-ef-search", 0, "Postgres backend only: override pgvector's hnsw.ef_search "+
		"query-time recall/speed tradeoff, valid range 1-1000 (default 200 — measured 94% recall@10; pgvector's own 40 measured 71-80%)")
	excludedProjects := fs.String("excluded-projects", "", "comma-separated glob patterns (supports *, **, ?, and a leading ~) — "+
		"a matching project gets no automatic prompt-context injection, the real claude-mem CLAUDE_MEM_EXCLUDED_PROJECTS feature; "+
		"empty (the default) excludes nothing")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "worker daemon's unix socket, notified of this turn's privacy state "+
		"(best-effort — a hook payload failure here is logged, never surfaced as a Claude Code-visible hook failure)")
	storePromptsFlag := fs.Bool("store-prompts", false, "persist each submitted prompt's text (after <private> stripping) to the store, "+
		"searchable via the search_prompts/session_prompts MCP tools; off by default because it stores the user's verbatim words. "+
		"Also enabled by "+storePromptsEnvVar+"=1")
	fs.Parse(args)
	*limit = clampLimit(*limit, 5, 100)

	l := openLog("prompt-context.log")

	in, err := claudeagent.ParseHookInput(os.Stdin)
	if err != nil {
		l.Errorf("FAILED parsing hook payload: %v", err)
		fmt.Println("{}")
		return 0
	}
	if excludeproject.IsExcluded(in.Cwd, *excludedProjects) {
		l.Printf("skip: project excluded (cwd=%s)", in.Cwd)
		fmt.Println("{}")
		return 0
	}
	if in.Prompt != "" && privacy.IsInternalProtocolPayload(in.Prompt) {
		// Real claude-mem's own session-init.ts checks this immediately
		// after its project-exclusion check too, before any privacy
		// stripping or embedding — a synthetic Claude Code
		// <task-notification>...</task-notification> payload auto-submitted
		// as a "prompt" is not real user text, so it must never be treated
		// as one: no privacy-state report to the worker, no embedding call,
		// no "memory relevant to what you just asked" injected in response
		// to internal plumbing.
		l.Printf("skip: prompt is an internal protocol payload, not real user text")
		fmt.Println("{}")
		return 0
	}

	prompt := privacy.StripMemoryTags(in.Prompt)
	private := prompt == "" && in.Prompt != ""
	// Sent for EVERY prompt, private or not — not just when private — so
	// the worker daemon's flag for this session never goes stale once a
	// later, non-private prompt supersedes an earlier private one. This
	// is the only place that knows a prompt's privacy state at all, so it
	// must report it regardless of whether -embed-model leaves the rest
	// of this hook's own injection feature disabled below.
	if err := hook.SetSessionPrivate(*socketPath, in.SessionID, private); err != nil {
		l.Warnf("failed to report privacy state to worker (best-effort, not fatal): %v", err)
	}

	if private {
		// Real claude-mem's own session-init route (SessionRoutes.ts) skips
		// entirely — no embedding call, no injection — when a prompt is
		// wholly wrapped in a privacy tag, its own documented convention
		// (real claude-mem's UserPromptSubmit banner tells users exactly
		// this: wrap a message in <private>...</private> to keep it out of
		// memory). Checked before EITHER feature below: a wholly-private
		// prompt is neither embedded nor, when -store-prompts is on,
		// persisted — the privacy flag has already been reported above,
		// and that is the only trace of it this hook ever leaves.
		l.Printf("skip: prompt entirely private after tag-stripping")
		fmt.Println("{}")
		return 0
	}
	storePrompts := *storePromptsFlag || os.Getenv(storePromptsEnvVar) == "1"
	embedEnabled := *embedModel != ""
	if !storePrompts && !embedEnabled {
		l.Printf("skip: semantic prompt injection disabled (-embed-model empty) and prompt storage not enabled")
		fmt.Println("{}")
		return 0
	}
	if dup, err := hook.CheckDuplicatePrompt(*socketPath, in.SessionID, promptHash(prompt)); err != nil {
		l.Warnf("failed to check duplicate-prompt state with worker (best-effort, not fatal): %v", err)
	} else if dup {
		// Real claude-mem's own fix for issue #2515: Claude Code can fire
		// UserPromptSubmit more than once for the same prompt within a
		// short window (USER_PROMPT_DEDUPE_WINDOW_MS, 10s) — without this
		// check, each firing would pay its own Ollama embedding call and
		// inject its own duplicate "memory relevant to what you just
		// asked" block in the same turn — and, with -store-prompts on,
		// store the same prompt twice under two prompt_numbers.
		l.Printf("skip: duplicate prompt for session %s within the dedupe window", in.SessionID)
		fmt.Println("{}")
		return 0
	}

	project := memory.ProjectFor(in.Cwd)
	if project == "" || project == "." {
		l.Printf("no usable project from cwd=%q, skipping", in.Cwd)
		fmt.Println("{}")
		return 0
	}

	ctx, cancel := hookContext(promptContextBudget)
	defer cancel()
	st, err := backend.Open(ctx, *dbPath, 0, *hnswEfSearch)
	if err != nil {
		l.Errorf("FAILED opening store at %s: %v", memory.RedactDSN(*dbPath), err)
		fmt.Println("{}")
		return 0
	}
	defer st.Close()

	if storePrompts {
		// The write is here and nowhere earlier, deliberately: every
		// privacy gate above (project exclusion, internal-protocol
		// payloads, <private> stripping, the wholly-private check) has
		// already run, so what reaches the store is exactly what a
		// partially-private prompt has left after stripping — never the
		// raw text. Stored regardless of the min-prompt-len/embedding
		// outcome below: "yes" or "continue" is still part of what the
		// user said, even if it's too short to be worth embedding. Never
		// fails the hook — a storage error is a Warn, and the injection
		// half carries on.
		if id, err := st.InsertPrompt(ctx, in.SessionID, project, prompt); err != nil {
			l.Warnf("failed to store prompt for session %s (best-effort, not fatal): %v", in.SessionID, err)
		} else {
			l.Debugf("stored prompt id=%d for session %s project=%s (%d chars)", id, in.SessionID, project, len(prompt))
		}
	}

	if !embedEnabled {
		l.Debugf("skip: semantic prompt injection disabled (-embed-model empty)")
		fmt.Println("{}")
		return 0
	}
	if len(prompt) < *minPromptLen {
		l.Printf("skip: prompt too short to embed meaningfully (%d chars, want >= %d)", len(prompt), *minPromptLen)
		fmt.Println("{}")
		return 0
	}

	vec, err := embed.NewClient(*embedModel).Embed(ctx, prompt)
	if err != nil {
		l.Errorf("FAILED embedding prompt (%d chars): %v", len(prompt), err)
		fmt.Println("{}")
		return 0
	}

	matches, err := st.SemanticSearch(ctx, project, vec, *limit)
	if err != nil {
		l.Errorf("FAILED SemanticSearch for project=%s: %v", project, err)
		fmt.Println("{}")
		return 0
	}
	// Report the outcome to the daemon so the read path is observable at
	// all. Errors are logged at debug and never affect this hook: the
	// daemon may legitimately not be running, and a telemetry write must
	// not delay or fail the injection the user is waiting on.
	if err := hook.ReportRecall(*socketPath, hook.RecallPrompt, len(matches)); err != nil {
		l.Debugf("skip: could not report recall outcome to the worker: %v", err)
	}
	if len(matches) == 0 {
		l.Printf("no embedded observations for project=%s, nothing to inject", project)
		fmt.Println("{}")
		return 0
	}

	injected := formatPromptContext(matches)
	out := hookOutput{HookSpecificOutput: &hookSpecificOutput{
		HookEventName:     "UserPromptSubmit",
		AdditionalContext: injected,
	}}
	enc, err := json.Marshal(out)
	if err != nil {
		l.Errorf("FAILED marshaling output: %v", err)
		fmt.Println("{}")
		return 0
	}
	l.Printf("injected %d semantically relevant observations for project=%s prompt=%q", len(matches), project, truncateForLog(prompt))
	fmt.Println(string(enc))
	return 0
}

// storePromptsEnvVar is the environment-variable form of -store-prompts,
// so the opt-in can be made once in a shell profile rather than edited
// into the plugin's hooks.json. "1" is the only value that enables it.
const storePromptsEnvVar = "CLAUDE_MEM_STORE_PROMPTS"

// truncateForLog keeps a real user prompt (which may be long, or contain
// sensitive-looking text) out of the log file beyond a short preview —
// this hook's log line exists to debug "why didn't it inject," not to
// duplicate the transcript. Cuts on a rune boundary, the same way
// transcript's truncate does: a byte slice through the middle of a
// multi-byte character (non-English prompts, emoji) leaves invalid UTF-8
// in the log, and %q then renders it as escaped garbage.
func truncateForLog(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// promptHash hashes the (already tag-stripped) prompt text for the
// worker's dedupe check — sent instead of the raw prompt itself, since
// this project's plain-text socket protocol has no length-prefixing or
// escaping for arbitrary prompt content, and the worker only ever needs
// to compare two prompts for equality, never to read the text back.
func promptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// formatPromptContext delegates to contextfmt so this hook and the
// observation_context MCP tool share one implementation.
func formatPromptContext(matches []memory.VectorMatch) string {
	return contextfmt.PromptContext(matches)
}
