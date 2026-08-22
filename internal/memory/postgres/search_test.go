package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// TestPostgresSearchCoversFactsAndConcepts is the regression test for a
// real, measured cross-backend divergence: this backend's search_vector
// used to cover only title/subtitle/narrative, while the SQLite backend's
// FTS5 table has always covered facts and concepts too — so the same
// observation was keyword-searchable through one memory.Backend
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
	if _, err := st.Insert(context.Background(), "s1", project, "Bash", memory.ContentHash("s1", "Bash", "facts", project),
		memory.Observation{Type: "discovery", Title: "unremarkable heading",
			Facts: []string{"the zorblatt subsystem was replaced"}}, 0); err != nil {
		t.Fatalf("Insert facts row: %v", err)
	}
	if _, err := st.Insert(context.Background(), "s1", project, "Bash", memory.ContentHash("s1", "Bash", "concepts", project),
		memory.Observation{Type: "discovery", Title: "another plain heading",
			Concepts: []string{"quibblesnort architecture"}}, 0); err != nil {
		t.Fatalf("Insert concepts row: %v", err)
	}

	factHits, err := st.Search(context.Background(), project, "zorblatt", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search for a facts-only term: %v", err)
	}
	if len(factHits) != 1 {
		t.Fatalf("Search(\"zorblatt\") returned %d rows, want 1 — the term exists only in facts, which search_vector must cover", len(factHits))
	}

	conceptHits, err := st.Search(context.Background(), project, "quibblesnort", "", 10, 0, 0, 0, "")
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

	if _, err := st.Insert(context.Background(), "s1", project, "Bash", memory.ContentHash("s1", "Bash", "tagged", project),
		memory.Observation{Type: "discovery", Title: "unrelated heading",
			Concepts: []string{"frobnicator"}}, 0); err != nil {
		t.Fatalf("Insert tag row: %v", err)
	}
	titled, err := st.Insert(context.Background(), "s1", project, "Bash", memory.ContentHash("s1", "Bash", "titled", project),
		memory.Observation{Type: "discovery", Title: "frobnicator rewritten from scratch"}, 0)
	if err != nil {
		t.Fatalf("Insert title row: %v", err)
	}

	hits, err := st.Search(context.Background(), project, "frobnicator", "", 10, 0, 0, 0, "")
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

// TestWebsearchQueryTranslation covers websearchQuery's pure logic — the
// operator rules it must mirror from sanitizeFTSQuery. No container
// needed; TestPostgresSearchBooleanOperatorParity below proves the
// resulting SQL actually behaves.
func TestWebsearchQueryTranslation(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain terms are quoted", "alpha beta", `"alpha" "beta"`},
		{"uppercase OR passes through", "alpha OR beta", `"alpha" OR "beta"`},
		{"uppercase AND passes through", "alpha AND beta", `"alpha" AND "beta"`},
		{"uppercase NOT becomes dash negation", "alpha NOT beta", `"alpha" -"beta"`},
		// FTS5 only honors these in uppercase, so lowercase must stay a
		// literal word — websearch_to_tsquery would otherwise treat it as
		// an operator, diverging in the opposite direction.
		{"lowercase or is a literal word", "cats or dogs", `"cats" "or" "dogs"`},
		{"lowercase not is a literal word", "alpha not beta", `"alpha" "not" "beta"`},
		{"trailing bare NOT is dropped", "alpha NOT", `"alpha"`},
		{"embedded quotes stripped", `say "hi"`, `"say" "hi"`},
		{"hyphenated term stays intact", "claude-mem", `"claude-mem"`},
		{"empty query", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := websearchQuery(c.in); got != c.want {
				t.Fatalf("websearchQuery(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestPostgresSearchBooleanOperatorParity is the regression test for a
// real, measured cross-backend divergence. This backend used
// plainto_tsquery, which ANDs every token and treats OR/NOT as ordinary
// words. Against these exact three rows, before the fix:
//
//	"alpha OR beta"  sqlite=[only-alpha only-beta both]  postgres=[both]
//	"alpha NOT beta" sqlite=[only-alpha]                 postgres=[both]
//
// OR silently lost two of three results, and NOT returned exactly the
// row the user asked to exclude while dropping the one they wanted.
func TestPostgresSearchBooleanOperatorParity(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)

	seed := func(key, title string) {
		t.Helper()
		if _, err := st.Insert(context.Background(), "s1", project, "Bash", memory.ContentHash("s1", "Bash", key, project),
			memory.Observation{Type: "discovery", Title: title}, 0); err != nil {
			t.Fatalf("Insert %s: %v", key, err)
		}
	}
	seed("a", "onlyalpha mentions alpha")
	seed("b", "onlybeta mentions beta")
	seed("c", "bothrow mentions alpha and beta")

	titles := func(rs []memory.SearchResult) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			for _, k := range []string{"onlyalpha", "onlybeta", "bothrow"} {
				if strings.Contains(r.Observation.Title, k) {
					m[k] = true
				}
			}
		}
		return m
	}

	t.Run("OR returns the union", func(t *testing.T) {
		got, err := st.Search(context.Background(), project, "alpha OR beta", "", 10, 0, 0, 0, "")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		m := titles(got)
		if len(got) != 3 || !m["onlyalpha"] || !m["onlybeta"] || !m["bothrow"] {
			t.Fatalf("Search(\"alpha OR beta\") = %d rows %v, want all three — OR must union, not AND", len(got), m)
		}
	})

	t.Run("NOT excludes rather than including", func(t *testing.T) {
		got, err := st.Search(context.Background(), project, "alpha NOT beta", "", 10, 0, 0, 0, "")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		m := titles(got)
		if len(got) != 1 || !m["onlyalpha"] {
			t.Fatalf("Search(\"alpha NOT beta\") = %d rows %v, want exactly onlyalpha — returning bothrow means NOT included the very row it was asked to exclude", len(got), m)
		}
	})

	t.Run("adjacent terms still AND", func(t *testing.T) {
		got, err := st.Search(context.Background(), project, "alpha beta", "", 10, 0, 0, 0, "")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if m := titles(got); len(got) != 1 || !m["bothrow"] {
			t.Fatalf("Search(\"alpha beta\") = %d rows %v, want only bothrow", len(got), m)
		}
	})

	// The case that motivated sanitizeFTSQuery on the other backend — a
	// bare hyphen — must keep working through the new quoted path.
	t.Run("hyphenated query still matches", func(t *testing.T) {
		seed("d", "the claude-mem project")
		got, err := st.Search(context.Background(), project, "claude-mem", "", 10, 0, 0, 0, "")
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("Search(\"claude-mem\") = %d rows, want 1 — the hyphen case must not regress", len(got))
		}
	})
}
