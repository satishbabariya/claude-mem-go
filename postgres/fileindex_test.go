package postgres

import (
	"fmt"
	"strings"
	"testing"

	"claude-mem-go/store"
)

// TestObservationsForFileUsesTheGinIndexes asserts the PLAN, not just the
// presence of an index. An index that exists but is never chosen is worth
// nothing, and that is a real risk here rather than a hypothetical one:
// the query uses the jsonb key-exists operator, which the jsonb_path_ops
// opclass does not support, so indexing with the wrong opclass would
// build cleanly and then be silently ignored.
//
// This is the read behind the PreToolUse file-context hook, so it runs
// before every Read tool call — the highest-frequency query this backend
// serves. Un-indexed its cost scaled with rows-per-project rather than
// with matches: measured on a 250,000-row corpus with realistic file
// arrays, 47.7ms without these indexes versus 12.9ms with, and a plan
// showing "Rows Removed by Filter: 4687".
func TestObservationsForFileUsesTheGinIndexes(t *testing.T) {
	st := openTestStore(t)
	project := uniqueProject(t)
	target := "/src/indexed-target.go"

	// Enough rows in the project that a post-filter plan would be
	// distinguishable from an index scan, and enough non-matching rows
	// that the planner has a reason to prefer the index.
	for i := 0; i < 300; i++ {
		files := []string{fmt.Sprintf("/src/other%d.go", i)}
		if i%50 == 0 {
			files = append(files, target)
		}
		title := fmt.Sprintf("row %d", i)
		if _, err := st.Insert("s-idx", project, "Read",
			store.ContentHash("s-idx", "Read", title, project),
			store.Observation{Type: "change", Title: title, FilesRead: files}, 0); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	if _, err := st.db.Exec("ANALYZE observations"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	// The indexes must exist at all — migration 4, or schemaSQL on a
	// fresh store.
	for _, idx := range []string{"idx_observations_files_read", "idx_observations_files_modified"} {
		var n int
		if err := st.db.QueryRow(
			"SELECT count(*) FROM pg_indexes WHERE tablename='observations' AND indexname=$1", idx).Scan(&n); err != nil {
			t.Fatalf("check %s: %v", idx, err)
		}
		if n != 1 {
			t.Fatalf("%s is missing — migration 4 did not apply", idx)
		}
	}

	// And the correct opclass, which is what makes them usable at all.
	var def string
	if err := st.db.QueryRow(
		"SELECT indexdef FROM pg_indexes WHERE indexname='idx_observations_files_read'").Scan(&def); err != nil {
		t.Fatalf("indexdef: %v", err)
	}
	if strings.Contains(def, "jsonb_path_ops") {
		t.Fatalf("index uses jsonb_path_ops: %s\n"+
			"That opclass does not support the key-exists operator this query uses, so the "+
			"index would build cleanly and then never be chosen.", def)
	}

	// Finally: does the planner actually pick it?
	rows, err := st.db.Query(`
		EXPLAIN (ANALYZE, COSTS OFF)
		SELECT id FROM observations
		WHERE project = $1 AND (files_read ? $2 OR files_modified ? $2)
		ORDER BY created_at_epoch DESC, id DESC LIMIT 50`, project, target)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}

	got := plan.String()
	if !strings.Contains(got, "idx_observations_files_read") && !strings.Contains(got, "idx_observations_files_modified") {
		t.Fatalf("the planner did not use either GIN index — the query is still a post-filter "+
			"over every row in the project, which is what these indexes exist to stop.\nPlan:\n%s", got)
	}

	// And the results must still be correct, not merely fast.
	results, err := st.ObservationsForFile(project, target, 50)
	if err != nil {
		t.Fatalf("ObservationsForFile: %v", err)
	}
	if len(results) != 6 {
		t.Fatalf("ObservationsForFile returned %d rows, want 6 (every 50th of 300)", len(results))
	}
}
