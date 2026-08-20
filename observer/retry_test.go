package observer

import (
	"testing"

	"claude-mem-go/classify"
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
