module github.com/satishbabariya/claude-mem-go

go 1.25.0

// Pinned, not left floating: release.yml's build and verify jobs must
// resolve the exact same Go toolchain (via go-version-file: go.mod in
// both), or a patch release landing between the two runs would silently
// change every release binary's bytes and make verify's independent
// rebuild diverge from build's output instead of matching it. See
// scripts/checksums/README.md for the reproducibility chain this
// protects.
toolchain go1.25.14

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/pgvector/pgvector-go v0.4.1
	github.com/satishbabariya/claude-agent-sdk-go v0.1.3
	modernc.org/sqlite v1.57.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
