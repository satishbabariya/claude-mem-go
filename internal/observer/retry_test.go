package observer

import (
	"context"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/classify"
	"github.com/satishbabariya/claude-mem-go/internal/transcript"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		kind classify.Kind
		want bool
	}{
		{classify.Transient, true},
		{classify.RateLimit, true},
		{classify.AuthInvalid, false},
		{classify.Unrecoverable, false},
		{classify.SetupRequired, false},
		{classify.QuotaExhausted, false},
	}
	for _, c := range cases {
		if got := isRetryable(c.kind); got != c.want {
			t.Errorf("isRetryable(%q) = %v, want %v", c.kind, got, c.want)
		}
	}
}

// fakeHandle is a Handle whose Observe always fails with err — the
// persistent-session half of ObserveResilient, without a subprocess.
type fakeHandle struct{ err error }

func (f fakeHandle) Observe(transcript.ToolCall) (Turn, error) { return Turn{}, f.err }
func (f fakeHandle) Close() error                              { return nil }

func TestObserveResilientRetryLoop(t *testing.T) {
	transient := &classify.Error{Kind: classify.Transient, Message: "stream stopped"}
	fatal := &classify.Error{Kind: classify.AuthInvalid, Message: "bad key"}
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}

	cases := []struct {
		name      string
		first     error   // what the persistent session returns
		oneShots  []error // what each successive one-shot returns
		wantErr   error   // nil means success expected
		wantCalls int     // one-shot calls expected
	}{
		{"retryable then succeeds", transient, []error{nil}, nil, 1},
		{"retryable exhausts attempts", transient, []error{transient, transient}, transient, 2},
		{"non-retryable returns immediately", fatal, nil, fatal, 0},
		{"retryable then non-retryable stops", transient, []error{fatal}, fatal, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			observeOneShot = func(context.Context, string, transcript.ToolCall) (Turn, error) {
				calls++
				if calls > len(c.oneShots) {
					t.Fatalf("one-shot called %d times, only %d planned", calls, len(c.oneShots))
				}
				return Turn{}, c.oneShots[calls-1]
			}
			defer func() { observeOneShot = ObserveOneShot }()

			_, err := ObserveResilient(context.Background(), fakeHandle{c.first}, "m", transcript.ToolCall{}, policy)
			if err != c.wantErr {
				t.Errorf("err = %v, want %v", err, c.wantErr)
			}
			if calls != c.wantCalls {
				t.Errorf("one-shot calls = %d, want %d", calls, c.wantCalls)
			}
		})
	}
}

// TestObserveResilientCancelDuringBackoff: the old time.Sleep ignored ctx,
// so a cancelled hook budget still waited out the full backoff.
func TestObserveResilientCancelDuringBackoff(t *testing.T) {
	observeOneShot = func(context.Context, string, transcript.ToolCall) (Turn, error) {
		t.Fatal("one-shot must not run once ctx is cancelled")
		return Turn{}, nil
	}
	defer func() { observeOneShot = ObserveOneShot }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Second}
	start := time.Now()
	_, err := ObserveResilient(ctx, fakeHandle{&classify.Error{Kind: classify.Transient}}, "m", transcript.ToolCall{}, policy)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("ObserveResilient slept through the backoff instead of returning on cancel")
	}
}
