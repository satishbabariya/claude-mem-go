# Development

Notes for people changing claude-mem-go rather than running it: the schema
migration framework both backends share, how the binary reports its own
version, what the test suite needs and why the Postgres tests refuse to guess
at a database, and how a release is cut.

## Schema migrations

- **Schema migrations** (`migrate/`) — a real, versioned schema-migration
  framework shared by both backends: a `schema_migrations` table records
  which numbered, idempotent migrations have run, so the next schema change
  has somewhere to go instead of either re-editing a `CREATE TABLE` that
  production databases already ran once, or repeating the bespoke
  "check `PRAGMA table_info`, `ALTER` if missing" pattern SQLite's
  `content_hash` column used before this existed. Both backends' existing
  schema steps (SQLite: the initial table, `content_hash`, the FTS5 index,
  the vector table; Postgres: the initial table + indexes) are now
  registered migrations rather than unconditional statements re-run on
  every `Open`. Verified against a real upgrade path, not just unit tests:
  a simulated pre-migration-framework SQLite database (missing both the
  `content_hash` column and the `schema_migrations` table itself) correctly
  migrates forward on reopen, and a live Postgres container correctly
  records its migration once and doesn't re-apply it on a second `Open`.
  `migrate.Run` itself had two real bugs, found by hand rather than
  assumed correct just because every migration so far happened to avoid
  triggering them: it documented applying migrations in "ascending
  Version order" but never actually sorted them — a migrations slice
  listing version 2 before version 1 applied version 2 *first*, silently
  violating the one guarantee this whole package exists to provide. Worse,
  two migrations accidentally sharing a Version number didn't error at
  all: once the first one got recorded as applied, the "already applied?"
  check silently skipped the second one forever, indistinguishable from
  having run correctly. Fixed by sorting explicitly before applying and
  rejecting duplicate version numbers outright, verified against both real
  backends (including the live Postgres container, in a throwaway schema
  cleaned up afterward, confirming migrations still apply in the correct
  order there too).

  A third, more serious bug in the same package was found by an actual
  full-stack integration smoke test — a real project wired with hooks
  pointing at explicit, isolated `-db`/`-socket` paths, run through a
  genuine `claude` session — rather than any of the targeted unit/e2e
  tests above: `SessionStart`'s `start` (which spawns the worker daemon,
  which itself calls `Open`) and `context` (which also calls `Open`
  directly) can both race to migrate the SAME brand-new SQLite file on a
  project's very first session. Reproduced deterministically: several
  independent connections to the same fresh file, migrated concurrently,
  surfaced three *different* real errors depending on timing —
  `"database is locked"` (SQLite's `busy_timeout` doesn't cover a losing
  DDL statement the way it covers a losing row lock), `"UNIQUE constraint
  failed: schema_migrations.version"` (two connections both saw a
  migration as unapplied and both tried to record it), and `"duplicate
  column name"` (a migration's own idempotency check — see
  `ensureContentHashColumn` — racing against an identical concurrent
  check, a classic check-then-act TOCTOU window). Fixed not by trying to
  prevent the race at the SQL level, but by retrying `Run`'s entire
  check-and-apply sequence on any failure: every `Migration.Apply` is
  already required to be idempotent, and the "already applied?" read
  happens fresh on each attempt, so a retry after a race-induced failure
  simply sees whatever the other process already committed and skips it
  — correct regardless of which specific error a given race happened to
  surface as. Verified by deliberately widening the exact TOCTOU window
  in a dedicated concurrency test (proven to reliably fail 3/3 times
  without the fix and pass 8/8 with it, including under `-race`), and by
  re-running the original full integration scenario that found this five
  more times with zero failures.

## Version stamping

- **version** — prints the exact commit and build time via Go's own
  `runtime/debug.ReadBuildInfo()` (VCS stamping is on by default since Go
  1.18 — no ldflags wiring, no version file to keep in sync, no CI change
  needed), including a `-dirty` marker if the tree had uncommitted changes
  at build time. There was no way to answer "what build is this" at all
  before this — no version flag, no way to correlate a bug report with an
  exact build. `doctor`'s header prints the same string. This is also
  `cmd/claude-mem-go`'s first test file — every other package already had
  coverage; this one didn't.

## Testing

The commands are in the [README](../README.md#testing); this is the history
behind the mandatory `CLAUDE_MEM_GO_TEST_POSTGRES_DSN` variable and what CI runs.

The damage was measured, not hypothesized. In the development store here:
**7,275 observations total, of which 7,217 (99.2%) were test debris**,
spread across 3,968 synthetic `test-*` projects. Only 58 rows were real,
and most of those were e2e artifacts too. A `internal/memory/backend/backend_test.go`
dispatch test had the identical defect on a smaller scale and left 2 rows
under project `backend-dispatch-test`; it now runs against a closed port
instead, since its assertion never needed a live database in the first
place.

Both were confirmed by break/restore: reinstating the old fallback and
running with no variable set silently wrote 118 rows into a database that
appeared nowhere on the command line. With the fallback removed, the same
run skips 52 tests, fails none, and leaves the store at exactly the row
count it started with.

CI sets the variable at `.github/workflows/ci.yml`, so coverage there is
unchanged — pointed at a database named `claudemem_ci_test`, deliberately
not `claudemem`, so the name alone says it is disposable.

`.github/workflows/ci.yml` runs the same commands (plus a `pgvector/pgvector:pg16`
service container) on every push, and genuinely passes there — checked via
`gh run list`, not assumed: every run since "Add a real versioned
schema-migration framework for both backends" has completed successfully
on GitHub's own hosted runners. That wasn't always true earlier in this
project's history: every run before that failed at `startup_failure`
before ever reaching a single step, caused by an account-level GitHub
billing lock unrelated to this repo or its workflow file — that's what
`act` (a real Docker-backed local runner) and `actionlint` were verifying
against at the time, since the hosted runners weren't reachable at all.
The billing lock has since been resolved; hosted-runner CI is the current
source of truth again.

`internal/memory/postgres/`'s tests skip cleanly (not fail) when nothing is listening at
`localhost:55432` — start `docker compose up -d` first if you want them to
actually run. They're real integration tests against a live container, not
mocks: dedup, hyphenated-query full-text search, and vector-similarity
ranking (with a control vector orthogonal to the query, confirming rank
order rather than just "no error") all execute real SQL.

Packages with pure logic (`classify`, `pool`, `store`, `transcript`,
`observer`'s retry policy, `worker`'s spawn-lock) have real unit tests.
`claude-agent-sdk-go`'s `Session`/`Query` and this project's end-to-end
worker/hook flow are validated by hand against the real `claude` CLI and
Claude Code hooks (see the project history) rather than mocked — mocking
the subprocess protocol would test the mock, not the thing that actually
breaks.

## Releases

Installing used to mean `go build` locally, every time, with no other
option. `.goreleaser.yaml` + `.github/workflows/release.yml` close that gap:
pushing a `vX.Y.Z` tag cross-compiles `claude-mem-go` for
linux/darwin × amd64/arm64 (`CGO_ENABLED=0` — a real constraint, not a
default left in place: both `modernc.org/sqlite` and `pgx/v5` are pure Go,
this project's whole reason for choosing them, so nothing here needs a C
toolchain per target) and attaches the archives plus a `checksums.txt` to a
real GitHub Release. Release notes are `CHANGELOG.md` itself, not an
auto-generated commit dump — this project already maintains one by hand
for exactly this reason.

Verified locally with a real snapshot build (`goreleaser release
--snapshot --clean --skip=publish`, no tag or publish needed) — all four
targets actually compiled, and the darwin/arm64 archive's binary was
extracted and run for real: `version` printed a correct commit/build-time
string (Go's own VCS stamping — see `version.go` — needs no ldflags
wiring, cross-compiled or not), and `doctor` ran its full real checklist
against a fresh temp database. The workflow YAML passed `actionlint`
before being committed, the same discipline `ci.yml` was checked with.
