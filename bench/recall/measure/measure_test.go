package main

import (
	"strings"
	"testing"
)

// These tests exist because this harness published a wrong number.
//
// The benchmark itself cannot run in CI — it needs a local Ollama and a
// ~20,000-row Postgres corpus that takes eleven minutes to seed. But the
// logic that made the published figure wrong is pure, and pure logic can
// be tested anywhere. What follows guards the two defences added after
// the retraction, so a future edit cannot quietly remove them.

// TestRecallAtIsTieTolerant is the exact defect that produced the
// retracted 95%: recall was counted as ID OVERLAP, and on a corpus with
// duplicate vectors the exact top-k is several copies of one vector at an
// identical distance. The sequential scan and the HNSW walk break that
// tie differently, so id overlap measured tie-break agreement rather than
// retrieval quality.
//
// Counting by distance makes the two indistinguishable, which is correct:
// returning a DIFFERENT row at the SAME distance is a perfect result, not
// a miss.
func TestRecallAtIsTieTolerant(t *testing.T) {
	// Ten equidistant rows; ANN returns ten different ids at that same
	// distance. Id overlap would score 0/10. The truth is 10/10.
	var exact, ann []hit
	for i := 0; i < 10; i++ {
		exact = append(exact, hit{id: int64(i), dist: 0.25})
		ann = append(ann, hit{id: int64(100 + i), dist: 0.25})
	}
	hits, total := recallAt(exact, ann)
	if hits != 10 || total != 10 {
		t.Fatalf("recallAt on an all-ties corpus = %d/%d, want 10/10 — "+
			"a different row at the same distance is a hit, not a miss", hits, total)
	}
}

func TestRecallAtCountsGenuineMisses(t *testing.T) {
	exact := []hit{{1, 0.1}, {2, 0.2}, {3, 0.3}}
	// Two inside the cutoff, one genuinely worse than the exact k-th.
	ann := []hit{{1, 0.1}, {2, 0.2}, {9, 0.9}}
	hits, total := recallAt(exact, ann)
	if hits != 2 || total != 3 {
		t.Fatalf("recallAt = %d/%d, want 2/3 — a result beyond the exact k-th distance is a real miss", hits, total)
	}
}

// TestRecallAtAcceptsFloatNoiseAtTheBoundary covers why distEpsilon
// exists: both plans compute the distance with the same operator, but
// they accumulate it in different scan orders, and float addition is not
// associative. Without a tolerance, a row that IS the exact k-th would
// score as a miss on the last bit.
func TestRecallAtAcceptsFloatNoiseAtTheBoundary(t *testing.T) {
	exact := []hit{{1, 0.1}, {2, 0.30000000000000004}}
	ann := []hit{{1, 0.1}, {2, 0.3000000000000001}}
	if hits, _ := recallAt(exact, ann); hits != 2 {
		t.Fatalf("recallAt = %d/2, want 2 — a last-bit float difference is not a miss", hits)
	}
	// The tolerance must stay tiny: it absorbs rounding, never a real gap.
	exact = []hit{{1, 0.1}}
	ann = []hit{{1, 0.1001}}
	if hits, _ := recallAt(exact, ann); hits != 0 {
		t.Fatal("recallAt treated a 1e-4 distance gap as a hit — distEpsilon is far too loose to distinguish rounding from a real miss")
	}
}

func TestRecallAtOnEmptyExactReportsNothing(t *testing.T) {
	hits, total := recallAt(nil, []hit{{1, 0.1}})
	if hits != 0 || total != 0 {
		t.Fatalf("recallAt with no ground truth = %d/%d, want 0/0 — "+
			"a query with no exact results must not contribute to the ratio", hits, total)
	}
}

// TestCheckCorpusRefusesDegenerateCorpora guards the second defence: the
// harness refuses to report at all on a corpus that cannot support a
// recall figure. The real case was 1,440 distinct vectors across 20,000
// rows — 7.2% — which produced a confident, plausible, meaningless
// number.
func TestCheckCorpusRefusesDegenerateCorpora(t *testing.T) {
	err := checkCorpus(20000, 1440)
	if err == nil {
		t.Fatal("checkCorpus accepted 1,440 distinct vectors across 20,000 rows — the exact corpus that produced the retracted figure")
	}
	// The message has to say what to DO; "degenerate" alone sends the
	// reader looking for a bug in the index rather than the corpus.
	if !strings.Contains(err.Error(), "Re-seed") {
		t.Errorf("degeneracy error does not say how to fix it: %v", err)
	}
}

func TestCheckCorpusAcceptsAUniqueCorpus(t *testing.T) {
	if err := checkCorpus(20000, 20000); err != nil {
		t.Fatalf("checkCorpus rejected a fully distinct corpus: %v", err)
	}
	// A handful of collisions is tolerable; the threshold is about a
	// generator that saturated its space, not about perfection.
	if err := checkCorpus(20000, 19900); err != nil {
		t.Fatalf("checkCorpus rejected 99.5%% distinct: %v", err)
	}
}

// TestCheckCorpusRejectsJustBelowTheThreshold pins the boundary, so
// loosening it is a deliberate edit rather than an accident.
func TestCheckCorpusRejectsJustBelowTheThreshold(t *testing.T) {
	if err := checkCorpus(10000, 9899); err == nil {
		t.Fatalf("checkCorpus accepted 98.99%% distinct, below the %.0f%% threshold", minDistinctFraction*100)
	}
}

func TestCheckCorpusRejectsAnEmptyCorpus(t *testing.T) {
	if err := checkCorpus(0, 0); err == nil {
		t.Fatal("checkCorpus accepted an empty corpus — recall over nothing is not 100%, it is undefined")
	}
}
