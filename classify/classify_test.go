package classify

import (
	"errors"
	"testing"

	claudeagent "claude-agent-sdk-go"
)

func intp(i int) *int { return &i }

func TestResult(t *testing.T) {
	cases := []struct {
		name string
		r    claudeagent.Result
		want Kind // zero value ("") means Result() must return nil
	}{
		{"not an error passes through nil", claudeagent.Result{IsError: false}, ""},
		{"401 is auth invalid", claudeagent.Result{IsError: true, APIErrorStatus: intp(401)}, AuthInvalid},
		{"403 is auth invalid", claudeagent.Result{IsError: true, APIErrorStatus: intp(403)}, AuthInvalid},
		{"429 is rate limit", claudeagent.Result{IsError: true, APIErrorStatus: intp(429)}, RateLimit},
		{"529 is transient", claudeagent.Result{IsError: true, APIErrorStatus: intp(529)}, Transient},
		{"500 is transient", claudeagent.Result{IsError: true, APIErrorStatus: intp(500)}, Transient},
		{"503 is transient", claudeagent.Result{IsError: true, APIErrorStatus: intp(503)}, Transient},
		{"400 is unrecoverable", claudeagent.Result{IsError: true, APIErrorStatus: intp(400)}, Unrecoverable},
		{"404 (bad model) is unrecoverable", claudeagent.Result{IsError: true, APIErrorStatus: intp(404)}, Unrecoverable},
		{
			"quota exceeded text with no status is quota_exhausted",
			claudeagent.Result{IsError: true, Text: "Quota Exceeded for this billing period"},
			QuotaExhausted,
		},
		{
			"unknown error with no status defaults to transient",
			claudeagent.Result{IsError: true, Text: "something unexpected"},
			Transient,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Result(c.r)
			if c.want == "" {
				if got != nil {
					t.Fatalf("Result() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Result() = nil, want Kind %q", c.want)
			}
			if got.Kind != c.want {
				t.Fatalf("Result().Kind = %q, want %q", got.Kind, c.want)
			}
		})
	}
}

func TestSpawn(t *testing.T) {
	if got := Spawn(nil); got != nil {
		t.Fatalf("Spawn(nil) = %v, want nil", got)
	}

	err := errors.New("claude executable not found")
	got := Spawn(err)
	if got == nil {
		t.Fatal("Spawn(err) = nil, want a SetupRequired *Error")
	}
	if got.Kind != SetupRequired {
		t.Fatalf("Spawn(err).Kind = %q, want %q", got.Kind, SetupRequired)
	}
	if got.Message != err.Error() {
		t.Fatalf("Spawn(err).Message = %q, want %q", got.Message, err.Error())
	}
}

func TestErrorImplementsErrorInterface(t *testing.T) {
	var err error = &Error{Kind: Transient, Message: "boom"}
	if err.Error() != "[transient] boom" {
		t.Fatalf("Error() = %q, want %q", err.Error(), "[transient] boom")
	}
}
