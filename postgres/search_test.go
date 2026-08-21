package postgres

import (
	"testing"

	"claude-mem-go/store"
)

// TestPostgresSearchCoversFactsAndConcepts is the regression test for a
// real, measured cross-backend divergence: this backend's search_vector
// used to cover only title/subtitle/narrative, while the SQLite backend's
// FTS5 table has always covered facts and concepts too — so the same
// observation was keyword-searchable through one store.Backend
// implementation and completely invisible through the other. Measured
// against this project's own accumulated dev container before the fix:
// 536 of 567 fact strings and 200 of 289 concept tags could not be found
// by a search for their own text.
func TestPostgresSearchCoversFactsAndConcepts(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	// The distinctive terms live ONLY in facts/concepts — nothing in
	// title/subtitle/narrative mentions them, so a hit can only come from
	// the columns this fix added.
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "facts", project),
		store.Observation{Type: "discovery", Title: "unremarkable heading",
			Facts: []string{"the zorblatt subsystem was replaced"}}, 0); err != nil {
		t.Fatalf("Insert facts row: %v", err)
	}
	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "concepts", project),
		store.Observation{Type: "discovery", Title: "another plain heading",
			Concepts: []string{"quibblesnort architecture"}}, 0); err != nil {
		t.Fatalf("Insert concepts row: %v", err)
	}

	factHits, err := st.Search(project, "zorblatt", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search for a facts-only term: %v", err)
	}
	if len(factHits) != 1 {
		t.Fatalf("Search(\"zorblatt\") returned %d rows, want 1 — the term exists only in facts, which search_vector must cover", len(factHits))
	}

	conceptHits, err := st.Search(project, "quibblesnort", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search for a concepts-only term: %v", err)
	}
	if len(conceptHits) != 1 {
		t.Fatalf("Search(\"quibblesnort\") returned %d rows, want 1 — the term exists only in concepts, which search_vector must cover", len(conceptHits))
	}
}

// TestPostgresSearchRanksTitleAboveTags locks in the 'D' weight choice:
// facts/concepts are searchable, but a hit in an observation's own title
// must still outrank a hit in another's tags, or adding these columns
// would have quietly degraded result ordering.
func TestPostgresSearchRanksTitleAboveTags(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	if _, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "tagged", project),
		store.Observation{Type: "discovery", Title: "unrelated heading",
			Concepts: []string{"frobnicator"}}, 0); err != nil {
		t.Fatalf("Insert tag row: %v", err)
	}
	titled, err := st.Insert("s1", project, "Bash", store.ContentHash("s1", "Bash", "titled", project),
		store.Observation{Type: "discovery", Title: "frobnicator rewritten from scratch"}, 0)
	if err != nil {
		t.Fatalf("Insert title row: %v", err)
	}

	hits, err := st.Search(project, "frobnicator", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search returned %d rows, want both the title hit and the tag hit", len(hits))
	}
	if hits[0].ID != titled.ID {
		t.Fatalf("first result is id=%d, want the title match id=%d — weight 'D' should rank a tag hit below a title hit", hits[0].ID, titled.ID)
	}
}
