package store

import (
	"path/filepath"
	"testing"
)

func TestHealthDetailsReflectsRealPragmas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	details, err := st.HealthDetails()
	if err != nil {
		t.Fatalf("HealthDetails: %v", err)
	}
	if details["journal_mode"] != "wal" {
		t.Errorf("journal_mode = %q, want wal", details["journal_mode"])
	}
	if details["foreign_keys"] != "1" {
		t.Errorf("foreign_keys = %q, want 1", details["foreign_keys"])
	}
	if details["busy_timeout_ms"] != "5000" {
		t.Errorf("busy_timeout_ms = %q, want 5000", details["busy_timeout_ms"])
	}
}
