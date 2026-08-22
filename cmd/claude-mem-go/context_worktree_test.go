package main

import (
	"context"
	"testing"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// fakeBackend records which projects were queried and serves canned rows.
type fakeBackend struct {
	memory.Backend
	rows    map[string][]memory.SearchResult
	queried []string
}

func (f *fakeBackend) RecentByProject(ctx context.Context, project string, limit int) ([]memory.SearchResult, error) {
	f.queried = append(f.queried, project)
	rs := f.rows[project]
	if len(rs) > limit {
		rs = rs[:limit]
	}
	return rs, nil
}

// TestRecentAcrossProjectsMergesAWorktreeWithItsParent covers the read
// half of worktree support. Writes go to the worktree's composite name,
// so without this a session in a worktree sees none of what the
// repository already knows — which is most of what there is to know.
func TestRecentAcrossProjectsMergesAWorktreeWithItsParent(t *testing.T) {
	f := &fakeBackend{rows: map[string][]memory.SearchResult{
		"mainrepo": {
			{ID: 1, Project: "mainrepo", CreatedAtEpoch: 100, Observation: memory.Observation{Title: "parent: auth uses JWT"}},
			{ID: 2, Project: "mainrepo", CreatedAtEpoch: 300, Observation: memory.Observation{Title: "parent: newer note"}},
		},
		"mainrepo/wt-feature": {
			{ID: 3, Project: "mainrepo/wt-feature", CreatedAtEpoch: 200, Observation: memory.Observation{Title: "worktree: started the feature"}},
		},
	}}

	got, err := recentAcrossProjects(context.Background(), f, []string{"mainrepo", "mainrepo/wt-feature"}, 10)
	if err != nil {
		t.Fatalf("recentAcrossProjects: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want all 3 across both projects: %+v", len(got), got)
	}
	// Merged strictly newest-first, regardless of which project each came
	// from — a worktree must not be forced to give up slots to its parent
	// or vice versa.
	if got[0].ID != 2 || got[1].ID != 3 || got[2].ID != 1 {
		t.Fatalf("merge order = %d,%d,%d, want 2,3,1 (newest first across both)", got[0].ID, got[1].ID, got[2].ID)
	}
}

// TestRecentAcrossProjectsRespectsTheLimitAfterMerging guards the obvious
// bug: fetching `limit` from each project and returning all of them would
// silently double the injected context for a worktree session.
func TestRecentAcrossProjectsRespectsTheLimitAfterMerging(t *testing.T) {
	rows := func(p string, base int64) []memory.SearchResult {
		var out []memory.SearchResult
		for i := 0; i < 5; i++ {
			out = append(out, memory.SearchResult{ID: base + int64(i), Project: p, CreatedAtEpoch: base + int64(i)})
		}
		return out
	}
	f := &fakeBackend{rows: map[string][]memory.SearchResult{
		"mainrepo":            rows("mainrepo", 100),
		"mainrepo/wt-feature": rows("mainrepo/wt-feature", 200),
	}}

	got, err := recentAcrossProjects(context.Background(), f, []string{"mainrepo", "mainrepo/wt-feature"}, 5)
	if err != nil {
		t.Fatalf("recentAcrossProjects: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d rows, want exactly the limit of 5 — merging must trim, not sum", len(got))
	}
}

// TestRecentAcrossProjectsSingleProjectIsUnchanged pins that the ordinary
// (non-worktree) case still makes exactly one query and does not pay for
// a merge path it never needs.
func TestRecentAcrossProjectsSingleProjectIsUnchanged(t *testing.T) {
	f := &fakeBackend{rows: map[string][]memory.SearchResult{
		"myrepo": {{ID: 1, Project: "myrepo", CreatedAtEpoch: 1}},
	}}
	got, err := recentAcrossProjects(context.Background(), f, []string{"myrepo"}, 10)
	if err != nil {
		t.Fatalf("recentAcrossProjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if len(f.queried) != 1 || f.queried[0] != "myrepo" {
		t.Fatalf("queried %v, want exactly one query for myrepo", f.queried)
	}
}
