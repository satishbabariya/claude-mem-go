package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func openPromptTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "prompts.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func setPromptCreatedAtEpoch(t *testing.T, s *Store, id int64, epoch int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE user_prompts SET created_at_epoch = ? WHERE id = ?`, epoch, id); err != nil {
		t.Fatalf("backdate prompt %d: %v", id, err)
	}
}

func TestInsertPromptNumbersWithinASessionAndSearchesByKeyword(t *testing.T) {
	st := openPromptTestStore(t)
	ctx := context.Background()

	id1, err := st.InsertPrompt(ctx, "sess-a", "proj", "why does the prune command delete nothing")
	if err != nil {
		t.Fatalf("InsertPrompt 1: %v", err)
	}
	id2, err := st.InsertPrompt(ctx, "sess-a", "proj", "add a retry to the postgres ping")
	if err != nil {
		t.Fatalf("InsertPrompt 2: %v", err)
	}
	idB, err := st.InsertPrompt(ctx, "sess-b", "proj", "prune is still broken on my machine")
	if err != nil {
		t.Fatalf("InsertPrompt (other session): %v", err)
	}
	if id1 == 0 || id2 <= id1 || idB <= id2 {
		t.Fatalf("ids not monotonic: %d %d %d", id1, id2, idB)
	}

	// prompt_number is per session, 1-based.
	a, err := st.PromptsBySession(ctx, "proj", "sess-a", 10)
	if err != nil {
		t.Fatalf("PromptsBySession: %v", err)
	}
	if len(a) != 2 || a[0].PromptNumber != 1 || a[1].PromptNumber != 2 || a[0].ID != id1 || a[1].ID != id2 {
		t.Fatalf("PromptsBySession(sess-a) = %+v, want prompt numbers 1,2 oldest first", a)
	}
	b, err := st.PromptsBySession(ctx, "proj", "sess-b", 10)
	if err != nil {
		t.Fatalf("PromptsBySession(sess-b): %v", err)
	}
	if len(b) != 1 || b[0].PromptNumber != 1 {
		t.Fatalf("PromptsBySession(sess-b) = %+v, want one prompt numbered 1 (numbering is per session)", b)
	}
	if a[0].CreatedAtEpoch == 0 || a[0].Text == "" || a[0].Project != "proj" || a[0].SessionID != "sess-a" {
		t.Fatalf("prompt row incompletely populated: %+v", a[0])
	}

	// Keyword search ranks by FTS and honours a hyphenated/punctuated term
	// the same way Search does (sanitizeFTSQuery).
	hits, err := st.SearchPrompts(ctx, "proj", "prune", 10, 0)
	if err != nil {
		t.Fatalf("SearchPrompts: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("SearchPrompts(prune) returned %d rows, want 2: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.ID == id2 {
			t.Fatalf("SearchPrompts(prune) returned the unrelated prompt %d", id2)
		}
	}
	none, err := st.SearchPrompts(ctx, "proj", "claude-mem", 10, 0)
	if err != nil {
		t.Fatalf("SearchPrompts with a hyphenated query must not error: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("SearchPrompts(claude-mem) = %+v, want none", none)
	}
}

func TestSearchPromptsEmptyQueryEnumeratesNewestFirstWithOffsetAndProjectScope(t *testing.T) {
	st := openPromptTestStore(t)
	ctx := context.Background()
	var ids []int64
	for _, text := range []string{"first prompt", "second prompt", "third prompt"} {
		id, err := st.InsertPrompt(ctx, "sess-a", "proj", text)
		if err != nil {
			t.Fatalf("InsertPrompt: %v", err)
		}
		ids = append(ids, id)
	}
	// Distinct timestamps so "newest first" is unambiguous.
	for i, id := range ids {
		setPromptCreatedAtEpoch(t, st, id, int64(1000*(i+1)))
	}
	if _, err := st.InsertPrompt(ctx, "sess-x", "other-proj", "a prompt from elsewhere"); err != nil {
		t.Fatalf("InsertPrompt (other project): %v", err)
	}

	all, err := st.SearchPrompts(ctx, "proj", "", 10, 0)
	if err != nil {
		t.Fatalf("SearchPrompts(enumerate): %v", err)
	}
	if len(all) != 3 || all[0].ID != ids[2] || all[2].ID != ids[0] {
		t.Fatalf("enumeration = %+v, want the three proj prompts newest first", all)
	}
	page, err := st.SearchPrompts(ctx, "proj", "", 1, 1)
	if err != nil {
		t.Fatalf("SearchPrompts(offset): %v", err)
	}
	if len(page) != 1 || page[0].ID != ids[1] {
		t.Fatalf("limit=1 offset=1 = %+v, want the middle prompt", page)
	}
	every, err := st.SearchPrompts(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("SearchPrompts(all projects): %v", err)
	}
	if len(every) != 4 {
		t.Fatalf("project \"\" enumeration returned %d rows, want 4 (every project)", len(every))
	}
	scoped, err := st.PromptsBySession(ctx, "other-proj", "sess-a", 10)
	if err != nil {
		t.Fatalf("PromptsBySession(wrong project): %v", err)
	}
	if len(scoped) != 0 {
		t.Fatalf("PromptsBySession scoped to the wrong project leaked %d rows", len(scoped))
	}
}

// TestPruneRemovesOldPromptsAndTheirFTSRows is the prompt-side version of
// TestPruneCleansUpFTSIndexToo: Prune's returned count is observations
// only, but prompts older than the cutoff must go too, and must stop
// matching afterwards (the user_prompts_ad trigger).
func TestPruneRemovesOldPromptsAndTheirFTSRows(t *testing.T) {
	st := openPromptTestStore(t)
	ctx := context.Background()
	old, err := st.InsertPrompt(ctx, "sess-a", "proj", "ancient xyzzy-plumbus question")
	if err != nil {
		t.Fatalf("InsertPrompt: %v", err)
	}
	setPromptCreatedAtEpoch(t, st, old, 1000)
	recent, err := st.InsertPrompt(ctx, "sess-a", "proj", "recent xyzzy-plumbus question")
	if err != nil {
		t.Fatalf("InsertPrompt: %v", err)
	}
	setPromptCreatedAtEpoch(t, st, recent, 5000)
	otherOld, err := st.InsertPrompt(ctx, "sess-z", "other", "ancient xyzzy-plumbus elsewhere")
	if err != nil {
		t.Fatalf("InsertPrompt: %v", err)
	}
	setPromptCreatedAtEpoch(t, st, otherOld, 1000)

	if _, err := st.Prune(ctx, "proj", 2000, true); err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	if rows, _ := st.SearchPrompts(ctx, "", "", 10, 0); len(rows) != 3 {
		t.Fatalf("dry run deleted prompts: %d left, want 3", len(rows))
	}
	if _, err := st.Prune(ctx, "proj", 2000, false); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	hits, err := st.SearchPrompts(ctx, "", "xyzzy-plumbus", 10, 0)
	if err != nil {
		t.Fatalf("SearchPrompts after prune: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("after a prune scoped to proj, search found %d prompts, want 2 (the recent one and the other project's)", len(hits))
	}
	for _, h := range hits {
		if h.ID == old {
			t.Fatalf("pruned prompt %d still matches — the FTS shadow row was not removed", old)
		}
	}

	// A later prompt in the pruned session must not collide with the
	// survivor's prompt_number (max+1, not count+1).
	if _, err := st.InsertPrompt(ctx, "sess-a", "proj", "after the prune"); err != nil {
		t.Fatalf("InsertPrompt after prune: %v", err)
	}
	after, err := st.PromptsBySession(ctx, "proj", "sess-a", 10)
	if err != nil {
		t.Fatalf("PromptsBySession: %v", err)
	}
	if len(after) != 2 || after[1].PromptNumber != 3 {
		t.Fatalf("after prune, session prompts = %+v, want the survivor (#2) then #3", after)
	}
}

func TestExportPromptsImportPromptRoundTripsIdempotently(t *testing.T) {
	src := openPromptTestStore(t)
	ctx := context.Background()
	for _, text := range []string{"one", "two"} {
		if _, err := src.InsertPrompt(ctx, "sess-a", "proj", text); err != nil {
			t.Fatalf("InsertPrompt: %v", err)
		}
	}
	rows, err := src.ExportPrompts(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ExportPrompts: %v", err)
	}
	if len(rows) != 2 || rows[0].Kind != memory.PromptRowKind || rows[0].PromptNumber != 1 || rows[1].PromptNumber != 2 {
		t.Fatalf("ExportPrompts = %+v, want two tagged rows oldest first", rows)
	}
	if _, err := time.Parse(time.RFC3339, rows[0].CreatedAt); err != nil {
		t.Fatalf("exported CreatedAt %q is not RFC3339: %v", rows[0].CreatedAt, err)
	}
	page, err := src.ExportPrompts(ctx, rows[0].ID, 100)
	if err != nil || len(page) != 1 || page[0].ID != rows[1].ID {
		t.Fatalf("ExportPrompts(afterID) = %+v, %v; want only the second row", page, err)
	}

	dst := openPromptTestStore(t)
	for _, r := range rows {
		inserted, err := dst.ImportPrompt(ctx, r)
		if err != nil {
			t.Fatalf("ImportPrompt: %v", err)
		}
		if !inserted {
			t.Fatalf("first ImportPrompt of %+v reported not inserted", r)
		}
	}
	for _, r := range rows {
		inserted, err := dst.ImportPrompt(ctx, r)
		if err != nil {
			t.Fatalf("second ImportPrompt: %v", err)
		}
		if inserted {
			t.Fatalf("second ImportPrompt of %+v was not a no-op", r)
		}
	}
	got, err := dst.PromptsBySession(ctx, "proj", "sess-a", 10)
	if err != nil {
		t.Fatalf("PromptsBySession: %v", err)
	}
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" || got[0].CreatedAtEpoch != rows[0].CreatedAtEpoch {
		t.Fatalf("imported prompts = %+v, want the two originals with their timestamps", got)
	}
	back, err := dst.ExportPrompts(ctx, 0, 100)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if back[0].CreatedAt != rows[0].CreatedAt {
		t.Fatalf("CreatedAt did not round-trip byte for byte: %q vs %q", back[0].CreatedAt, rows[0].CreatedAt)
	}
	if _, err := dst.ImportPrompt(ctx, memory.PromptRow{SessionID: "s", Project: "p", PromptNumber: 1, CreatedAt: "not a date"}); err == nil {
		t.Fatal("ImportPrompt accepted a malformed CreatedAt")
	}
	if _, err := dst.ImportPrompt(ctx, memory.PromptRow{SessionID: "s", Project: "p", PromptNumber: 0, CreatedAt: rows[0].CreatedAt}); err == nil {
		t.Fatal("ImportPrompt accepted prompt_number 0")
	}
}

func TestStatsCountsPrompts(t *testing.T) {
	st := openPromptTestStore(t)
	ctx := context.Background()
	s, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Prompts != 0 {
		t.Fatalf("Prompts on a fresh store = %d, want 0", s.Prompts)
	}
	if _, err := st.InsertPrompt(ctx, "sess-a", "proj", "hello"); err != nil {
		t.Fatalf("InsertPrompt: %v", err)
	}
	s, err = st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Prompts != 1 {
		t.Fatalf("Prompts = %d, want 1", s.Prompts)
	}
}
