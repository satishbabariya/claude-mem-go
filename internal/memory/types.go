package memory

// InsertResult reports whether Insert actually created a new row or found
// one already there with the same ContentHash — the caller (worker/ingest)
// needs to know which happened: a duplicate is not an error, but it also
// shouldn't be double-counted or re-embedded.
type InsertResult struct {
	ID       int64
	Inserted bool // false: a row with this ContentHash already existed; ID is that row's.
}

// SearchResult is one FTS5 match, joined back to its full observation.
type SearchResult struct {
	ID          int64
	Observation Observation
	SessionID   string
	Project     string
	ToolName    string
	// CreatedAtEpoch is when the observation was recorded, in Unix epoch
	// milliseconds — the same column Search's date filters and every
	// ORDER BY here already use.
	//
	// A search result carried no time information at all before this,
	// which made a whole class of question unanswerable from a result
	// alone: "when did we learn this", and more sharply, "is what we
	// remember about this file still true?" The PreToolUse file-context
	// hook injects remembered facts about a file immediately before
	// Claude reads it, and without a timestamp it could not tell the file
	// had been rewritten since — see cmdFileContext, which now compares
	// this against the file's mtime. Real claude-mem's own file-context
	// handler makes exactly that comparison (buildFileContextTimeline:
	// "File modified since last observation, skipping context injection").
	//
	// Populated by every query that builds a SearchResult, deliberately:
	// a field only some code paths fill is worse than no field at all,
	// because a zero here reads as "1970", not as "unknown".
	CreatedAtEpoch int64
}

// MaxTimelineDepth bounds Timeline's depthBefore/depthAfter — the same
// "no legitimate caller needs more than a page" reasoning as
// MaxIDsPerLookup, and for a query built from a caller-controlled integer
// rather than a caller-controlled slice length, so there's no driver-level
// placeholder-count failure mode here to reproduce; this cap exists purely
// to keep one MCP call bounded, not to work around a driver limit.
const MaxTimelineDepth = 100

// MaxIDsPerLookup bounds a single ByIDs call, enforced by both backends —
// found the hard way, not anticipated: a real test against this project's
// own SQLite driver (modernc.org/sqlite) showed 100,000 IDs failing
// outright with "SQL logic error: too many SQL variables" once the
// hand-built IN (?,?,...) placeholder list crossed the driver's real
// limit, rather than degrading gracefully. Set well below where that
// limit actually starts (confirmed empirically between 10,000 and
// 100,000) and matching the "max 100" convention every other MCP tool's
// own limit argument already uses (see mcpserver.go), since no legitimate
// caller needs more than a page of IDs at once — this is a detail lookup
// for results a search already returned, not a bulk export (see export/
// import for that).
const MaxIDsPerLookup = 100

// VectorMatch is one semantic search result.
type VectorMatch struct {
	ID          int64
	Project     string
	ToolName    string
	Observation Observation
	Score       float64 // cosine similarity, [-1, 1], higher = closer
}

// ExportRow is one observation as read back by ExportAll — everything
// needed to reinsert it via ImportRow, preserving its original identity
// (ContentHash, the idempotency key), timing (CreatedAt/CreatedAtEpoch),
// and — a real gap in this feature's first version, found by the same
// "does every write path do what the others do" check that caught the
// add_observation/Stop embedding bugs — its embedding, if it had one.
// Without this, export+import (backup, or the SQLite<->Postgres migration
// path) silently dropped semantic searchability for every single
// observation: a "migrate to Postgres for real ANN search at scale" would
// have arrived with nothing left to search.
type ExportRow struct {
	ID             int64       `json:"id"`
	SessionID      string      `json:"session_id"`
	Project        string      `json:"project"`
	ToolName       string      `json:"tool_name"`
	ContentHash    string      `json:"content_hash"`
	Observation    Observation `json:"observation"`
	CostUSD        float64     `json:"cost_usd"`
	CreatedAt      string      `json:"created_at"`
	CreatedAtEpoch int64       `json:"created_at_epoch"`
	// Embedding is nil when the observation was never embedded (no embed
	// model configured at capture time, or the embedding call failed) —
	// that's a legitimate, common state, not an error.
	Embedding []float32 `json:"embedding,omitempty"`
}
