package memory

import "regexp"

// credentialsPattern matches a URL's userinfo when it carries a password —
// scheme://user:password@ — and captures the user so it can be preserved.
// A plain SQLite file path never contains "://" at all, so it can never
// match; a well-formed Postgres DSN with no password (postgres://user@host)
// has no ":" before the "@" either, so it doesn't match — correctly left
// unchanged in both cases, since there's nothing to redact.
var credentialsPattern = regexp.MustCompile(`://([^:/@\s]+):[^@/\s]*@`)

// RedactDSN returns dsn with any embedded password masked, for safe use in
// a log line or printed error. Deliberately regex-based on the raw string
// rather than net/url.Parse-then-reserialize: a malformed DSN (one that
// fails to parse — a real case, not hypothetical: an unreachable/garbled
// -db value a user might actually pass) must not fall back to returning
// the UNREDACTED original just because parsing failed elsewhere in the
// string. This was caught by hand, not assumed: an early net/url-based
// version of this function returned dsn verbatim — password included —
// whenever url.Parse errored, which a deliberately malformed host
// ("postgres://user:pass@[invalid host/db") triggered immediately. The
// regex only needs the scheme://user:password@ prefix to be well-formed,
// independent of whatever comes after "@", so it redacts correctly even
// when the rest of the DSN is broken.
//
// Every place a -db flag's value might reach a log file or stdout must go
// through this first: a real Postgres connection string carries a
// plaintext password, and every log site that printed *dbPath (or an
// error wrapping it — see postgres.Open) directly before this existed was
// a genuine credential leak, not a hypothetical one, for a backend this
// project calls "production-grade."
func RedactDSN(dsn string) string {
	return credentialsPattern.ReplaceAllString(dsn, "://$1:REDACTED@")
}
