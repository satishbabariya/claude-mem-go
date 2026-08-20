package store

import "testing"

func TestSanitizeFTSQuery(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"single word", "hello", `"hello"`},
		{"hyphenated word", "claude-mem", `"claude-mem"`},
		{"boolean OR preserved", "monetization OR sqlite", `"monetization" OR "sqlite"`},
		{"boolean AND preserved", "config AND parser", `"config" AND "parser"`},
		{"boolean NOT preserved", "search NOT deprecated", `"search" NOT "deprecated"`},
		{"embedded quote escaped", `say "hi"`, `"say" """hi"""`},
		{"empty query", "", `""`},
		{"colon", "key:value", `"key:value"`},
		{"parens", "(grouped)", `"(grouped)"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeFTSQuery(c.in); got != c.want {
				t.Fatalf("sanitizeFTSQuery(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSearchHandlesHyphenatedQueries is the regression test for the actual
// bug found: searching for "claude-mem" — arguably the single most likely
// real query against this project — failed FTS5's query parser outright
// ("no such column: mem") before sanitizeFTSQuery existed.
func TestSearchHandlesHyphenatedQueries(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	o := Observation{Type: "discovery", Title: "claude-mem installation found"}
	if _, err := st.Insert("s1", "proj", "Bash", ContentHash("s1", "Bash", "a", "b"), o, 0); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	results, err := st.Search("", "claude-mem", 10)
	if err != nil {
		t.Fatalf("Search(\"claude-mem\") returned an error instead of results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(\"claude-mem\") returned %d results, want 1", len(results))
	}
}
