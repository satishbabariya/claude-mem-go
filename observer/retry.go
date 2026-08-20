// Retry policy for observer turns — motivated by a real failure, not a
// hypothetical: a live ingest run hit "API Error: The response stopped
// arriving" mid-stream, correctly classified as classify.Transient, and
// aborted the whole ingest because nothing retried it. This file adds that
// missing resilience.
package observer

import (
	"context"
	"time"

	"claude-mem-go/classify"
	"claude-mem-go/transcript"
)

// RetryPolicy controls ObserveResilient's backoff.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration // doubles after each retry
}

// DefaultRetryPolicy: 3 attempts, 500ms/1s backoff between them.
var DefaultRetryPolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: 500 * time.Millisecond}

// isRetryable reports whether a classify.Kind is worth retrying at all.
// Only transient/rate_limit conditions are — auth_invalid, unrecoverable,
// setup_required, and quota_exhausted will fail again identically on retry,
// so retrying them just burns attempts and money for no chance of success.
func isRetryable(k classify.Kind) bool {
	return k == classify.Transient || k == classify.RateLimit
}

// ObserveOneShot spawns a fresh session, observes exactly one tool call, and
// closes the session. Used for retries: after a transient failure (e.g. a
// broken stream mid-response), the underlying subprocess's state is
// suspect, so a retry gets a clean session rather than assuming the old one
// is still usable.
func ObserveOneShot(ctx context.Context, model string, tc transcript.ToolCall) (Turn, error) {
	o, err := New(ctx, model)
	if err != nil {
		return Turn{}, err
	}
	defer o.Close()
	return o.Observe(tc)
}

// ObserveResilient tries tc on the given persistent session first — the
// normal path, which preserves multi-turn context. Only if that fails with
// a retryable error does it fall back to fresh one-shot sessions (losing
// that turn's shared context, not the whole conversation) up to
// policy.MaxAttempts total attempts. A non-retryable error returns
// immediately without consuming further attempts.
func ObserveResilient(ctx context.Context, o Handle, model string, tc transcript.ToolCall, policy RetryPolicy) (Turn, error) {
	turn, err := o.Observe(tc)
	if err == nil {
		return turn, nil
	}
	ce, ok := err.(*classify.Error)
	if !ok || !isRetryable(ce.Kind) {
		return Turn{}, err
	}

	lastErr := err
	delay := policy.BaseDelay
	for attempt := 2; attempt <= policy.MaxAttempts; attempt++ {
		time.Sleep(delay)
		delay *= 2

		turn, err := ObserveOneShot(ctx, model, tc)
		if err == nil {
			return turn, nil
		}
		lastErr = err
		ce, ok := err.(*classify.Error)
		if !ok || !isRetryable(ce.Kind) {
			return Turn{}, err
		}
	}
	return Turn{}, lastErr
}
