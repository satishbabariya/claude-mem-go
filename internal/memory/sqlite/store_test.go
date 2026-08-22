package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func TestParseXML(t *testing.T) {
	t.Run("plain XML", func(t *testing.T) {
		raw := `<observation>
  <type>discovery</type>
  <title>Found the config file</title>
  <subtitle>Located in the repo root</subtitle>
  <facts>
    <fact>File is config.yaml</fact>
    <fact>Contains 12 keys</fact>
  </facts>
  <narrative>The config file was located.</narrative>
  <concepts>
    <concept>configuration</concept>
  </concepts>
  <files_read>
    <file>config.yaml</file>
  </files_read>
</observation>`
		o, err := memory.ParseXML(raw)
		if err != nil {
			t.Fatalf("ParseXML: %v", err)
		}
		if o.Type != "discovery" || o.Title != "Found the config file" {
			t.Fatalf("got %+v", o)
		}
		if !reflect.DeepEqual(o.Facts, []string{"File is config.yaml", "Contains 12 keys"}) {
			t.Fatalf("Facts = %v", o.Facts)
		}
		if !reflect.DeepEqual(o.Concepts, []string{"configuration"}) {
			t.Fatalf("Concepts = %v", o.Concepts)
		}
		if !reflect.DeepEqual(o.FilesRead, []string{"config.yaml"}) {
			t.Fatalf("FilesRead = %v", o.FilesRead)
		}
		if len(o.FilesModified) != 0 {
			t.Fatalf("FilesModified = %v, want empty (tag omitted)", o.FilesModified)
		}
	})

	t.Run("wrapped in a markdown code fence, as models actually do", func(t *testing.T) {
		raw := "```xml\n<observation><type>change</type><title>x</title></observation>\n```"
		o, err := memory.ParseXML(raw)
		if err != nil {
			t.Fatalf("ParseXML: %v", err)
		}
		if o.Type != "change" || o.Title != "x" {
			t.Fatalf("got %+v", o)
		}
	})

	t.Run("placeholder list items are dropped, not returned literally", func(t *testing.T) {
		raw := `<observation><type>discovery</type><title>x</title>
<facts><fact>...</fact><fact>real fact</fact></facts></observation>`
		o, err := memory.ParseXML(raw)
		if err != nil {
			t.Fatalf("ParseXML: %v", err)
		}
		if !reflect.DeepEqual(o.Facts, []string{"real fact"}) {
			t.Fatalf("Facts = %v, want the placeholder \"...\" excluded", o.Facts)
		}
	})

	t.Run("no observation block is an error, not a zero value", func(t *testing.T) {
		if _, err := memory.ParseXML("I couldn't complete this request."); err == nil {
			t.Fatal("ParseXML on non-XML text: want an error, got nil")
		}
	})
}

func TestVectorEncodeDecodeRoundTrip(t *testing.T) {
	v := []float32{0.1, -2.5, 3.14159, 0, -0.0001, 1e10}
	blob := encodeVector(v)
	got, err := decodeVector(blob, len(v))
	if err != nil {
		t.Fatalf("decodeVector: %v", err)
	}
	if !reflect.DeepEqual(got, v) {
		t.Fatalf("round trip = %v, want %v", got, v)
	}
}

func TestDecodeVectorRejectsMismatchedLength(t *testing.T) {
	blob := encodeVector([]float32{1, 2, 3})
	if _, err := decodeVector(blob, 4); err == nil {
		t.Fatal("decodeVector with wrong dims: want an error, got nil")
	}
}

func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name    string
		a, b    []float32
		want    float64
		epsilon float64
	}{
		{"identical vectors", []float32{1, 0, 0}, []float32{1, 0, 0}, 1, 1e-9},
		{"orthogonal vectors", []float32{1, 0}, []float32{0, 1}, 0, 1e-9},
		{"opposite vectors", []float32{1, 0}, []float32{-1, 0}, -1, 1e-9},
		{"mismatched lengths", []float32{1, 2}, []float32{1, 2, 3}, -1, 0},
		{"a zero vector", []float32{0, 0}, []float32{1, 1}, -1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cosineSimilarity(c.a, c.b)
			if diff := got - c.want; diff > c.epsilon || diff < -c.epsilon {
				t.Fatalf("cosineSimilarity(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestInsertAndSearchRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	o := memory.Observation{
		Type:      "discovery",
		Title:     "Found a race condition in the pool",
		Subtitle:  "Acquire/Release under concurrent load",
		Facts:     []string{"Reproduced with 12 goroutines", "Fixed with a buffered channel"},
		Narrative: "A concurrency bug was found and fixed.",
	}
	res, err := st.Insert(context.Background(), "session-1", "my-project", "Bash", memory.ContentHash("session-1", "Bash", "ls -la", "file1 file2"), o, 0.01)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !res.Inserted {
		t.Fatal("first Insert of a new content_hash reported Inserted=false")
	}
	id := res.ID

	count, err := st.CountByProject(context.Background(), "my-project")
	if err != nil {
		t.Fatalf("CountByProject: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByProject = %d, want 1", count)
	}

	results, err := st.Search(context.Background(), "", "race condition", "", 10, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search returned %d results, want 1", len(results))
	}
	if results[0].ID != id {
		t.Fatalf("Search result ID = %d, want %d", results[0].ID, id)
	}
	if !reflect.DeepEqual(results[0].Observation.Facts, o.Facts) {
		t.Fatalf("round-tripped Facts = %v, want %v", results[0].Observation.Facts, o.Facts)
	}

	if noResults, err := st.Search(context.Background(), "", "completely unrelated query xyzzy", "", 10, 0, 0, 0, ""); err != nil {
		t.Fatalf("Search: %v", err)
	} else if len(noResults) != 0 {
		t.Fatalf("Search for an unrelated query returned %d results, want 0", len(noResults))
	}
}

func TestSaveEmbeddingAndSemanticSearchRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	res1, err := st.Insert(context.Background(), "s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "cmd-a", "out-a"), memory.Observation{Type: "discovery", Title: "about cats"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	res2, err := st.Insert(context.Background(), "s1", "proj", "Bash", memory.ContentHash("s1", "Bash", "cmd-b", "out-b"), memory.Observation{Type: "discovery", Title: "about dogs"}, 0)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	id1, id2 := res1.ID, res2.ID

	// Fake embeddings: id1 close to the query vector, id2 orthogonal to it.
	if err := st.SaveEmbedding(context.Background(), id1, []float32{1, 0, 0}); err != nil {
		t.Fatalf("SaveEmbedding id1: %v", err)
	}
	if err := st.SaveEmbedding(context.Background(), id2, []float32{0, 1, 0}); err != nil {
		t.Fatalf("SaveEmbedding id2: %v", err)
	}

	results, err := st.SemanticSearch(context.Background(), "", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("SemanticSearch returned %d results, want 2", len(results))
	}
	if results[0].ID != id1 {
		t.Fatalf("best match = observation %d, want %d (the vector aligned with the query)", results[0].ID, id1)
	}
	if results[0].Score <= results[1].Score {
		t.Fatalf("best match score %v should be strictly greater than second %v", results[0].Score, results[1].Score)
	}
}
