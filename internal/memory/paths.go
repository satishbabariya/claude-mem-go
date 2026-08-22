// Package memory is claude-mem-go's storage-neutral domain: the Backend
// contract every caller programs against, the Observation/SearchResult/
// ExportRow value types both backends exchange, and the pure helpers
// (content hashing, path canonicalization, project derivation, file-
// context selection) that have nothing to do with any one database.
// The SQLite and Postgres implementations live in the sqlite and
// postgres subpackages; backend.Open picks one from a DSN.
package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultHome is ~/.claude-mem-go, created if missing.
func DefaultHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(home, ".claude-mem-go")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// DBPathEnvVar overrides where every command looks for the store, and is
// the only way an installed plugin can reach Postgres at all.
//
// This is not a convenience. All 15 subcommands declare
// `-db` with DefaultDBPath() as its default, but nothing that actually
// runs in a plugin install can pass that flag: hooks/hooks.json invokes
// the binary as `"$CLAUDE_PLUGIN_ROOT/claude-mem-go" start` (and context,
// prompt-context, file-context, hook, stop), and .mcp.json execs
// `... mcp` — no flags, and no place to add them without editing files
// that ship with the plugin and get overwritten on update.
//
// So an operator who follows the README, stands up Postgres, and installs
// the plugin gets a split brain, measured here: `doctor` with no flag
// reports `/Users/…/.claude-mem-go/observations.db`, while `doctor -db
// postgres://…` reports the Postgres store. Every hook and every MCP tool
// call took the first branch. The Postgres backend this project exists to
// provide was unreachable in the one configuration most users run.
//
// Named to match the CLAUDE_MEM_POSTGRES_* family already used for pool,
// timeout, and embedding-dimension settings.
const DBPathEnvVar = "CLAUDE_MEM_DB"

// DefaultDBPath is $CLAUDE_MEM_DB when set, else
// ~/.claude-mem-go/observations.db.
//
// Returning the env value from the *default* rather than checking it at
// each use site is deliberate: it gives the precedence operators expect —
// an explicit `-db` still wins, because flag parsing overwrites the
// default — and it fixed all 15 subcommands at once rather than relying
// on 15 separate call sites remembering to look.
//
// A whitespace-only value is treated as unset. That is the shape a
// misconfigured shell actually produces (`CLAUDE_MEM_DB=$UNSET_VAR`), and
// honoring it would send the store to a file literally named " ".
func DefaultDBPath() string {
	if v := strings.TrimSpace(os.Getenv(DBPathEnvVar)); v != "" {
		return v
	}
	return BuiltinDBPath()
}

// BuiltinDBPath is the store used when nothing configures one: the path
// DefaultDBPath falls back to with $CLAUDE_MEM_DB unset. Exposed so
// `start` can tell "the daemon predates the user's configuration" (it is
// on this path, the session is not) from "two sessions legitimately
// configured two stores" (both explicit), which must not restart it.
func BuiltinDBPath() string { return filepath.Join(DefaultHome(), "observations.db") }
