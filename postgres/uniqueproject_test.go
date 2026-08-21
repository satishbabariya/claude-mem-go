package postgres

import "testing"

// TestUniqueProjectNeverCollides guards a real CI flake, not a
// hypothetical one. uniqueProject used to be name+UnixNano, and a
// full-suite run failed with two supposedly-distinct projects carrying
// the identical name — all three of that test's rows landed in one
// project and a single-result assertion saw two. Measured here
// afterwards: adjacent UnixNano reads are identical 92% of the time on
// this machine, and adjacent uniqueProject calls collided 74% of the
// time. It only passed in practice because isolated `-run` invocations
// spread the calls out; a loaded full-suite run does not.
//
// 20,000 iterations rather than a handful: at a 74% per-pair collision
// rate the old implementation fails this within the first few calls, so
// the count is not what makes it sensitive — it is here so the guard
// stays meaningful on a machine with a finer clock, where the old bug
// would be rare rather than absent.
func TestUniqueProjectNeverCollides(t *testing.T) {
	seen := make(map[string]bool, 20000)
	for i := 0; i < 20000; i++ {
		p := uniqueProject(t)
		if seen[p] {
			t.Fatalf("uniqueProject collided after %d calls: %q", i, p)
		}
		seen[p] = true
	}
}
