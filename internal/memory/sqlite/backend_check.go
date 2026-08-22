package sqlite

import "github.com/satishbabariya/claude-mem-go/internal/memory"

// Compile-time check that *Store satisfies memory.Backend — a signature
// drift fails the build here instead of at the first call site.
var _ memory.Backend = (*Store)(nil)
