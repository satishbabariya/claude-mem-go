package main

// clampLimit bounds a caller-supplied -limit flag value to (0, max]. The
// MCP server (mcpserver.go) already caps its own limit argument the same
// way, for the same reason: SQLite treats a non-positive LIMIT as
// "unbounded" (LIMIT -1, or LIMIT 0 returning nothing at all depending on
// the query shape), and an unbounded result from a single CLI invocation
// is worth guarding against rather than trusting, the same as it was for
// the MCP server's caller-supplied arguments. def is what a non-positive
// value falls back to (each subcommand's own flag default), not 0 or max,
// so a mistyped -limit doesn't silently become "everything" OR "nothing."
func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
