package main

import "testing"

func TestClampLimitPassesThroughAValidValue(t *testing.T) {
	if got := clampLimit(5, 10, 100); got != 5 {
		t.Errorf("clampLimit(5, 10, 100) = %d, want 5", got)
	}
}

func TestClampLimitFallsBackToDefaultOnZeroOrNegative(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		if got := clampLimit(n, 10, 100); got != 10 {
			t.Errorf("clampLimit(%d, 10, 100) = %d, want 10 (the default)", n, got)
		}
	}
}

func TestClampLimitCapsAboveMax(t *testing.T) {
	if got := clampLimit(99999, 10, 100); got != 100 {
		t.Errorf("clampLimit(99999, 10, 100) = %d, want 100 (capped)", got)
	}
}
