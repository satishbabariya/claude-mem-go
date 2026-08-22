package postgres

import (
	"context"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

func backdatePromptCreatedAtEpoch(t *testing.T, s *Store, id int64, epoch int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE user_prompts SET created_at_epoch = $1 WHERE id = $2`, epoch, id); err != nil {
		t.Fatalf("backdate prompt %d: %v", id, err)
	}
}

// TestPostgresPromptsInsertSearchAndSessionOrder is the Postgres half of
// the SQLite prompt tests, run against the shared container with
// uniqueProject so it never sees another run's rows.
func TestPostgresPromptsInsertSearchAndSessionOrder(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	project := uniqueProject(t)
	sessA, sessB := project+"-sess-a", project+"-sess-b"

	id1, err := st.InsertPrompt(ctx, sessA, project, "why does the prune command delete nothing")
	if err != nil {
		t.Fatalf("InsertPrompt 1: %v", err)
	}
	id2, err := st.InsertPrompt(ctx, sessA, project, "add a retry to the postgres ping")
	if err != nil {
		t.Fatalf("InsertPrompt 2: %v", err)
	}
	if _, err := st.InsertPrompt(ctx, sessB, project, "prune is still broken on my machine"); err != nil {
		t.Fatalf("InsertPrompt (other session): %v", err)
	}

	a, err := st.PromptsBySession(ctx, project, sessA, 10)
	if err != nil {
		t.Fatalf("PromptsBySession: %v", err)
	}
	if len(a) != 2 || a[0].PromptNumber != 1 || a[1].PromptNumber != 2 || a[0].ID != id1 || a[1].ID != id2 {
		t.Fatalf("PromptsBySession = %+v, want prompt numbers 1,2 oldest first", a)
	}
	b, err := st.PromptsBySession(ctx, project, sessB, 10)
	if err != nil || len(b) != 1 || b[0].PromptNumber != 1 {
		t.Fatalf("PromptsBySession(sessB) = %+v, %v; want one prompt numbered 1", b, err)
	}
	if a[0].CreatedAtEpoch == 0 {
		t.Fatalf("CreatedAtEpoch not populated: %+v", a[0])
	}

	hits, err := st.SearchPrompts(ctx, project, "prune", 10, 0)
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
	// Boolean operators mean the same thing as on SQLite (websearchQuery).
	or, err := st.SearchPrompts(ctx, project, "retry OR machine", 10, 0)
	if err != nil {
		t.Fatalf("SearchPrompts(OR): %v", err)
	}
	if len(or) != 2 {
		t.Fatalf("SearchPrompts(retry OR machine) returned %d rows, want 2", len(or))
	}

	all, err := st.SearchPrompts(ctx, project, "", 10, 0)
	if err != nil {
		t.Fatalf("SearchPrompts(enumerate): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("enumeration returned %d rows, want 3", len(all))
	}
	page, err := st.SearchPrompts(ctx, project, "", 1, 2)
	if err != nil || len(page) != 1 {
		t.Fatalf("limit=1 offset=2 = %+v, %v; want exactly one row", page, err)
	}
	if scoped, _ := st.PromptsBySession(ctx, uniqueProject(t), sessA, 10); len(scoped) != 0 {
		t.Fatalf("PromptsBySession scoped to the wrong project leaked %d rows", len(scoped))
	}
}

func TestPostgresPruneRemovesOldPromptsInScope(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	project, other := uniqueProject(t), uniqueProject(t)
	sess := project + "-sess"

	old, err := st.InsertPrompt(ctx, sess, project, "ancient question")
	if err != nil {
		t.Fatalf("InsertPrompt: %v", err)
	}
	backdatePromptCreatedAtEpoch(t, st, old, 1000)
	recent, err := st.InsertPrompt(ctx, sess, project, "recent question")
	if err != nil {
		t.Fatalf("InsertPrompt: %v", err)
	}
	backdatePromptCreatedAtEpoch(t, st, recent, 5000)
	otherOld, err := st.InsertPrompt(ctx, other+"-sess", other, "ancient elsewhere")
	if err != nil {
		t.Fatalf("InsertPrompt: %v", err)
	}
	backdatePromptCreatedAtEpoch(t, st, otherOld, 1000)

	if _, err := st.Prune(ctx, project, 2000, true); err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	if rows, _ := st.PromptsBySession(ctx, project, sess, 10); len(rows) != 2 {
		t.Fatalf("dry run deleted prompts: %d left, want 2", len(rows))
	}
	if _, err := st.Prune(ctx, project, 2000, false); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	left, err := st.PromptsBySession(ctx, project, sess, 10)
	if err != nil {
		t.Fatalf("PromptsBySession: %v", err)
	}
	if len(left) != 1 || left[0].ID != recent {
		t.Fatalf("after prune, session prompts = %+v, want only the recent one", left)
	}
	if untouched, _ := st.SearchPrompts(ctx, other, "", 10, 0); len(untouched) != 1 {
		t.Fatalf("prune scoped to %s touched project %s's prompts", project, other)
	}
	if _, err := st.InsertPrompt(ctx, sess, project, "after the prune"); err != nil {
		t.Fatalf("InsertPrompt after prune: %v", err)
	}
	after, _ := st.PromptsBySession(ctx, project, sess, 10)
	if len(after) != 2 || after[1].PromptNumber != 3 {
		t.Fatalf("after prune, session prompts = %+v, want #2 then #3 (max+1 numbering)", after)
	}
}

func TestPostgresExportImportPromptRoundTripIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	project := uniqueProject(t)
	sess := project + "-sess"
	for _, text := range []string{"one", "two"} {
		if _, err := st.InsertPrompt(ctx, sess, project, text); err != nil {
			t.Fatalf("InsertPrompt: %v", err)
		}
	}
	var mine []memory.PromptRow
	afterID := int64(0)
	for {
		page, err := st.ExportPrompts(ctx, afterID, 500)
		if err != nil {
			t.Fatalf("ExportPrompts: %v", err)
		}
		for _, r := range page {
			if r.Project == project {
				mine = append(mine, r)
			}
		}
		if len(page) < 500 {
			break
		}
		afterID = page[len(page)-1].ID
	}
	if len(mine) != 2 || mine[0].Kind != memory.PromptRowKind || mine[0].PromptNumber != 1 || mine[1].PromptNumber != 2 {
		t.Fatalf("ExportPrompts for %s = %+v, want two tagged rows oldest first", project, mine)
	}
	// Re-importing into the same store is a no-op (same session_id +
	// prompt_number); importing under a fresh session id is a real insert.
	for _, r := range mine {
		inserted, err := st.ImportPrompt(ctx, r)
		if err != nil {
			t.Fatalf("ImportPrompt (duplicate): %v", err)
		}
		if inserted {
			t.Fatalf("ImportPrompt re-inserted an existing prompt: %+v", r)
		}
	}
	restored := project + "-restored"
	for _, r := range mine {
		r.SessionID = restored
		inserted, err := st.ImportPrompt(ctx, r)
		if err != nil {
			t.Fatalf("ImportPrompt (fresh session): %v", err)
		}
		if !inserted {
			t.Fatalf("ImportPrompt under a new session id was not inserted: %+v", r)
		}
	}
	got, err := st.PromptsBySession(ctx, project, restored, 10)
	if err != nil {
		t.Fatalf("PromptsBySession: %v", err)
	}
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" || got[0].CreatedAtEpoch != mine[0].CreatedAtEpoch {
		t.Fatalf("restored prompts = %+v, want the two originals with their timestamps", got)
	}

	s, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Prompts < 4 {
		t.Fatalf("Stats.Prompts = %d, want at least the 4 this test wrote", s.Prompts)
	}
}
