# Security

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting for this repository
(the "Report a vulnerability" button under the Security tab at
https://github.com/satishbabariya/claude-mem-go/security) rather than a
public issue. If that isn't available, open a regular issue without
exploit details and ask for a private channel to share them.

## Trust model

`claude-mem-go` has no network-facing service by default, and the pieces
that do talk to something else are scoped narrowly:

- **The MCP server** (`mcp` subcommand) speaks stdio JSON-RPC only — no
  socket, no port. It's spawned by Claude Code as a child process, and
  the trust boundary is the local OS user, the same as every other MCP
  server. There is no authentication layer of its own, because there is
  no network surface to authenticate against.
- **The worker daemon** listens on a Unix domain socket
  (`~/.claude-mem-go/worker.sock` by default). Access control is
  whatever the OS filesystem permissions on that socket path already
  provide — no separate protocol-level auth.
- **The optional Prometheus endpoint** (`-metrics-addr`, off by default)
  is the one thing in this project that binds an actual TCP port, and it
  is unauthenticated. If you enable it, bind it to `127.0.0.1` or a
  private interface — never expose it on a public or shared network
  without your own reverse proxy / auth in front of it.
- **The Postgres backend** connects with whatever DSN you provide;
  TLS/`sslmode` is entirely your own configuration choice via that DSN.
  `docker-compose.yml`'s bundled container uses `sslmode=disable`
  because it's a local dev/test fixture, not a deployment template — a
  production Postgres instance should use a DSN with TLS enabled if it's
  reachable over anything but localhost.

## What's actually hardened, and why

A recurring theme in this project's own history (see `CHANGELOG.md`) has
been finding real, previously-latent bugs by testing against real
dependencies rather than assuming correctness. The security-relevant
ones:

- **DSN credentials are never logged in the clear.** `store.RedactDSN`
  masks a Postgres DSN's password before it reaches any log line, error
  message, or `doctor` output — including on connection failure, which
  is exactly when a raw DSN is most likely to get printed somewhere. An
  early version of this redaction had its own bug (a malformed DSN made
  it fall back to the *unredacted* original), caught by hand before
  shipping.
- **Every caller-supplied size/count is bounded.** Hook payloads
  (`hook.MaxPayloadBytes`, 8MB), MCP tool argument counts
  (`store.MaxIDsPerLookup`, `store.MaxTimelineDepth`, both 100), and
  every `limit` argument across both storage backends reject or clamp
  values that would otherwise let one caller force an unbounded
  scan/transfer, or — in a couple of real cases found this way — crash
  the process outright (a negative `limit` slicing out of bounds in
  `SemanticSearch`; a negative `-max-concurrent` panicking Go's own
  `make(chan)`).
- **The worker daemon and MCP server both recover from panics.** Neither
  had any panic recovery at all until a real, reproducible panic was
  found in `SemanticSearch`'s own limit handling — since both are
  long-lived processes (the worker shared across every project on the
  machine; the MCP server for the whole `claude` session), an
  unrecovered panic in either would have taken the whole process down,
  not just failed one call. This is a backstop against the *next* bug of
  this shape too, not just the one that was found.

## What's explicitly out of scope

- **No encryption at rest.** The SQLite file and the Postgres database
  hold observations in plain form; protecting them is a filesystem
  permissions / database access-control question, the same as any other
  local application data.
- **No rate limiting.** The bounded-input protections above stop a
  single call from being unboundedly expensive, but nothing here limits
  *how often* a client can call. For a stdio MCP server spawned
  per-session and a Unix-socket daemon on a single machine, this hasn't
  been a real-world problem; it would be a reasonable addition if this
  project's trust model ever changed (e.g. a network-facing deployment).
- **No cross-project access control beyond scoping.** Every project's
  observations live in one shared database (SQLite file or Postgres
  instance); isolation between projects is enforced by this codebase's
  own query scoping (see `CHANGELOG.md`'s cross-project-leak fixes), not
  by separate credentials or database-level permissions per project.
