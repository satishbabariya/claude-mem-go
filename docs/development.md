# Development

For people changing claude-mem-go rather than running it.

## Contents

- [Layout](#layout)
- [Building](#building)
- [Testing](#testing)
- [Schema migrations](#schema-migrations)
- [Releases](#releases)
- [CI](#ci)

## Layout

Module path `github.com/satishbabariya/claude-mem-go`, Go 1.25. The package
map is in the [README](../README.md#architecture). Conventions worth knowing:

- `cmd/claude-mem-go` is one thin file per subcommand with dispatch in
  `main.go`; tests there pin `hooks/hooks.json` against that dispatch switch,
  the usage message against it, hook budgets against manifest timeouts, every
  `CLAUDE_MEM_*` variable against the README, and the skills against
  `doctor`'s critical findings and the MCP tool table.
- `memory.Backend` is the storage contract. Both backends must agree; several
  findings in [findings.md](findings.md) are cross-backend divergences, and
  new behaviour should get a mirrored test on each side.
- Every `Backend` method takes a `ctx`. Hooks use the budgets in `ctx.go`;
  operator commands use `cliContext`, which is unbounded but cancels on
  Ctrl-C.
- `mcpserver` cannot import `package main`, so the two hook formatters it
  mirrors (`session_start_context`, `observation_context`) are duplicated
  byte for byte and tested for parity.
- Library code reads environment variables through named constants (e.g.
  `memory.DBPathEnvVar`, `embed.BaseURLEnvVar`) so the env-docs test can find
  them.

## Building

```sh
go build -o claude-mem-go ./cmd/claude-mem-go
./claude-mem-go version        # commit, build time, go version; "-dirty" for an uncommitted tree
```

The version comes from `runtime/debug.ReadBuildInfo()`'s VCS stamping — no
ldflags, no version file. `doctor`'s header and the MCP `initialize` response
print the same string, and the worker stats file records it so `start` can
detect a daemon running an older build. `CGO_ENABLED=0` works everywhere:
`modernc.org/sqlite` and `pgx/v5` are pure Go.

## Testing

```sh
go test ./...
go test ./... -race
gofmt -l .                      # must print nothing
go vet ./...
```

Unit tests cover the pure-logic packages (`classify`, `pool`, `privacy`,
`excludeproject`, `transcript`, `migrate`, `observer`'s retry policy,
`worker`'s spawn lock, cache, and socket protocol, `mcpserver` against a temp
SQLite file). Nothing in the suite needs the `claude` CLI or Ollama: tests
that would (embedding retries, `add_observation` embedding) skip when Ollama
is unreachable, and the observer/hook flow is verified by hand against real
Claude Code sessions, because mocking the subprocess protocol would test the
mock.

### Postgres

The Postgres tests are integration tests against a live container and refuse
to guess at a database:

```sh
docker compose up -d
docker compose exec postgres createdb -U claudemem claudemem_test
CLAUDE_MEM_GO_TEST_POSTGRES_DSN=postgres://claudemem:claudemem@localhost:55432/claudemem_test?sslmode=disable \
  go test ./internal/memory/postgres/... -v
```

Without the variable they skip (52 tests). Point it at a throwaway database:
the tests write and delete real rows. The variable is mandatory because an
earlier fallback to the README's own DSN filled the development store with
7,217 test rows (99.2% of 7,275) across 3,968 `test-*` projects, and a
`backend` dispatch test left rows under `backend-dispatch-test`; that test now
uses a closed port. Near-miss names (`CLAUDE_MEM_TEST_POSTGRES_DSN`,
`CLAUDE_MEM_POSTGRES_DSN`, and three others) are detected and reported so a
typo does not silently skip the suite.

`recall_fixture_test.go` guards the ANN properties with 300 committed real
embeddings (`testdata/recall_fixture.f32.gz`): `SemanticSearch` at the default
`ef_search` returns a full top-10 at >= 90% of exact, and a store opened with
`ef_search = 3` returns at most 3 rows on at least one query, proving the
per-call `SET LOCAL` reaches pgvector. It forces the index through a
connection GUC because a fresh database is small enough for the planner to
take an exact sequential scan.

### Linux via Docker

The project is developed on macOS. To run the suite on Linux:

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.25 go test ./... -race
```

(`go1.25.14 linux/arm64` is the last recorded run, all packages under
`-race`.) The systemd unit in `deploy/systemd/` can be checked the same way
with `systemd-analyze verify` in a systemd container.

### End-to-end expectations

When a change touches a hook or the daemon, run it for real: `claude
--plugin-dir "$PWD"` in a throwaway project with an explicit `-db` or
`CLAUDE_MEM_DB` pointing at a scratch store, never the shared one. The
expected outcome of a session with a few tool calls is an observation per
tool call in `stats`, a `summary` row after the session ends, injected
context at the next `SessionStart`, and clean `~/.claude-mem-go/*.log` files.
Three concurrent sessions against one daemon should capture every event
(9/9 in the recorded soak). Concurrency fixes in this project were each
confirmed by break/restore — reverting the fix and watching the new test
fail — and that standard applies to new regression tests.

## Schema migrations

`internal/migrate` is shared by both backends: a `schema_migrations` table
records which numbered migrations have run; `migrate.Run` sorts migrations by
version, rejects duplicate version numbers, applies the unapplied ones in
order, and retries the whole check-and-apply sequence on failure. The retry is
what makes concurrent first-open safe — `start` (via the daemon) and
`context` both open a brand-new SQLite file at a project's first
`SessionStart`, which surfaced `database is locked`, `UNIQUE constraint
failed: schema_migrations.version`, and `duplicate column name` depending on
timing. It works because every migration must be idempotent: a retry sees
whatever the other process committed and skips it.

To add one:

- **SQLite** — append to the list in `internal/memory/sqlite/migrations.go`
  with the next `Version` and a `Name`. Use `IF NOT EXISTS`, and for column
  additions check `PRAGMA table_info` first (see the `content_hash` and
  `next_steps` migrations), since SQLite has no `ADD COLUMN IF NOT EXISTS`.
  Current head: version 8 (`user_prompts`).
- **Postgres** — append to the list in `internal/memory/postgres/postgres.go`
  and, if the change affects fresh databases, mirror it in `schemaSQL`. Use
  `IF NOT EXISTS`; for constraints, check `pg_constraint` inside a `DO` block
  (migration 2); a generated column cannot be altered, so drop and re-add it
  (migration 3). Current head: version 6 (`user_prompts`).
- Test the upgrade path, not just a fresh database: both backends have tests
  that build a store at an older version and reopen it. Keep the two backends'
  behaviour identical and add a mirrored test on each side.

## Releases

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`, which runs
goreleaser (`.goreleaser.yaml`): `claude-mem-go` cross-compiled for
linux/darwin × amd64/arm64 with `CGO_ENABLED=0` and `-s -w`, tar.gz archives
containing README, CHANGELOG, and LICENSE, a `checksums.txt`, and a GitHub
Release whose notes are the hand-written `CHANGELOG.md` (goreleaser's own
changelog is disabled).

The stamp must be clean. Go's VCS stamp reports `-dirty` for any modified or
untracked file in the tree at build time, and the v0.4.0 assets shipped with
`<commit>-dirty` because goreleaser's `dist/` output counted. `dist/` is now
gitignored and the `before` hook runs `go mod tidy -diff` — verify, never
modify — so the release checkout stays clean. The workflow checks out with
`fetch-depth: 0` so `vcs.revision`/`vcs.time` resolve. To cut a release:

```sh
# update CHANGELOG.md and .claude-plugin/plugin.json version, commit, then:
git tag vX.Y.Z && git push origin vX.Y.Z
```

Dry run locally with `goreleaser release --snapshot --clean --skip=publish`;
the darwin/arm64 archive's binary has been extracted and run (`version`,
`doctor`) to confirm the cross-compiled output works.

## CI

`.github/workflows/ci.yml` runs on every push and pull request, on Go 1.25
(the floor the plugin promises) and `stable` (what a fresh Go install has) —
two versions because a `go build -o X` onto an existing file only fails on
1.26+. Steps: build, `go vet`, staticcheck, govulncheck, `gofmt -l`, `go test
./... -race -v` against a `pgvector/pgvector:pg16` service container with
`CLAUDE_MEM_GO_TEST_POSTGRES_DSN` pointed at `claudemem_ci_test` (named so it
is obviously disposable), and `claude plugin validate .` with no skip
fallback. Workflow YAML is checked with `actionlint` before committing.
