package cli

import (
	"flag"

	"github.com/satishbabariya/claude-mem-go/internal/memory"
)

// DBFlag registers the -db flag every store-touching subcommand accepts.
// One definition, so the help text and the CLAUDE_MEM_DB default cannot
// drift between the sixteen subcommands that used to each declare it.
func DBFlag(fs *flag.FlagSet) *string {
	return fs.String("db", memory.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
}

// DefaultEmbedModel is the Ollama model every embedding path defaults to.
const DefaultEmbedModel = "nomic-embed-text"
