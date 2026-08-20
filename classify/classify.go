// Package classify sorts a claude-agent-sdk-go Result (or a spawn failure)
// into the same error taxonomy claude-mem's real
// src/services/worker/provider-errors.ts and ClaudeProvider.ts's
// classifyClaudeError use. This is deliberately NOT part of
// claude-agent-sdk-go itself — see that package's doc comment: what counts
// as retryable vs. fatal is this application's policy, not a protocol fact,
// so it lives here, one layer up.
//
// The real classifyClaudeError reads a thrown JS exception's .status/.name/
// .error.type against the Anthropic SDK's own error shapes. This port reads
// the CLI's own api_error_status/subtype instead, because the CLI already
// did that classification server-side and reports it structurally — see
// go-observer-spike's Phase 2, which validated this against a genuine
// bad-model-name error (api_error_status 404 -> Unrecoverable, matching what
// classifyClaudeError assigns a bad-model 400/404).
package classify

import (
	"fmt"
	"strings"

	claudeagent "claude-agent-sdk-go"
)

// Kind mirrors ProviderErrorClass in provider-errors.ts.
type Kind string

const (
	Transient      Kind = "transient"
	Unrecoverable  Kind = "unrecoverable"
	RateLimit      Kind = "rate_limit"
	QuotaExhausted Kind = "quota_exhausted"
	AuthInvalid    Kind = "auth_invalid"
	SetupRequired  Kind = "setup_required"
)

// Error is this package's ClassifiedProviderError equivalent.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("[%s] %s", e.Kind, e.Message) }

// Result classifies a Result that reported IsError. Returns nil if the
// result was not an error — callers should check r.IsError themselves if
// they want to distinguish "not an error" from "classified", but Result nil
// on a non-error input is the convenient common case.
func Result(r claudeagent.Result) *Error {
	if !r.IsError {
		return nil
	}
	status := 0
	if r.APIErrorStatus != nil {
		status = *r.APIErrorStatus
	}
	switch {
	case status == 401 || status == 403:
		return &Error{AuthInvalid, r.Text}
	case status == 429:
		return &Error{RateLimit, r.Text}
	case status == 529:
		return &Error{Transient, r.Text}
	case status >= 500 && status < 600:
		return &Error{Transient, r.Text}
	case status == 400 || status == 404:
		// 404 here means "model not found" — classifyClaudeError treats both
		// as unrecoverable configuration errors, not something to retry.
		return &Error{Unrecoverable, r.Text}
	case strings.Contains(strings.ToLower(r.Text), "quota exceeded"):
		return &Error{QuotaExhausted, r.Text}
	default:
		// Default: unknown errors are transient — classifyClaudeError's
		// stated policy is "preserve old behavior of retrying everything not
		// explicitly marked unrecoverable."
		return &Error{Transient, r.Text}
	}
}

// Spawn classifies a failure to even start or run the subprocess (claude
// not found, pipe broken, context canceled) — mirrors classifyClaudeError's
// "Executable / spawn issues" branch, which is always SetupRequired: there
// is no point retrying a missing binary.
func Spawn(err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{SetupRequired, err.Error()}
}
