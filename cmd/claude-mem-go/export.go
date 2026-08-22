package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
)

// exportPageSize bounds how many rows ExportAll fetches per page — keeps
// memory bounded for a very large store regardless of how big -db turns
// out to be, at the cost of a few more round trips for a small one.
const exportPageSize = 500

// cmdExport writes every observation as JSON Lines (one per line) — the
// store's only backup/migration story. There was no way to get data out
// of this store at all before this: no backup, and no way to move data
// between the SQLite and Postgres backends (the same export file imports
// cleanly into either).
func cmdExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := cli.DBFlag(fs)
	out := fs.String("out", "", "output file (JSON Lines, one observation per line); defaults to stdout")
	fs.Parse(args)

	ctx, cancel := cliContext()
	defer cancel()
	st, err := backend.Open(ctx, *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED to create %s: %v\n", *out, err)
			return 1
		}
		defer f.Close()
		w = f
	}

	enc := json.NewEncoder(w)
	afterID := int64(0)
	total := 0
	for {
		rows, err := st.ExportAll(ctx, afterID, exportPageSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED export: %v\n", err)
			return 1
		}
		for _, r := range rows {
			if err := enc.Encode(r); err != nil {
				fmt.Fprintf(os.Stderr, "FAILED encoding row %d: %v\n", r.ID, err)
				return 1
			}
			total++
		}
		if len(rows) < exportPageSize {
			break
		}
		afterID = rows[len(rows)-1].ID
	}

	// Prompt rows follow the observation rows, each tagged "kind":"prompt"
	// — observation rows carry no kind at all, so a file written before
	// prompts existed still imports unchanged, and a file written now
	// imports into an older binary as observations-only rather than
	// failing outright. Without this, a backup or a SQLite<->Postgres
	// migration would silently drop every stored prompt.
	prompts := 0
	afterID = 0
	for {
		rows, err := st.ExportPrompts(ctx, afterID, exportPageSize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAILED export prompts: %v\n", err)
			return 1
		}
		for _, r := range rows {
			r.Kind = memory.PromptRowKind
			if err := enc.Encode(r); err != nil {
				fmt.Fprintf(os.Stderr, "FAILED encoding prompt row %d: %v\n", r.ID, err)
				return 1
			}
			prompts++
		}
		if len(rows) < exportPageSize {
			break
		}
		afterID = rows[len(rows)-1].ID
	}

	if *out != "" {
		fmt.Fprintf(os.Stderr, "Exported %d observation(s) and %d prompt(s) to %s\n", total, prompts, *out)
	}
	return 0
}

// cmdImport reads a file written by `export` and re-inserts every row.
// Idempotent by construction — ImportRow preserves each row's original
// content_hash, so importing the same file twice (or restoring on top of
// data that's already there) skips rows already present instead of
// duplicating them.
func cmdImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dbPath := cli.DBFlag(fs)
	in := fs.String("in", "", "input file written by `export` (JSON Lines); required")
	fs.Parse(args)

	if *in == "" {
		fmt.Fprintln(os.Stderr, "usage: claude-mem-go import -in <file> [-db path]")
		return 2
	}

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open %s: %v\n", *in, err)
		return 1
	}
	defer f.Close()

	ctx, cancel := cliContext()
	defer cancel()
	st, err := backend.Open(ctx, *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED to open store: %v\n", err)
		return 1
	}
	defer st.Close()

	dec := json.NewDecoder(f)
	imported, skipped := 0, 0
	promptsImported, promptsSkipped := 0, 0
	for dec.More() {
		// Each line is decoded once into raw bytes and then into the type
		// its "kind" says it is: a row without one is an observation (the
		// format before prompts existed), "prompt" is a user prompt.
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			fmt.Fprintf(os.Stderr, "FAILED decoding row from %s: %v\n", *in, err)
			return 1
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			fmt.Fprintf(os.Stderr, "FAILED decoding row from %s: %v\n", *in, err)
			return 1
		}
		switch probe.Kind {
		case "":
			var row memory.ExportRow
			if err := json.Unmarshal(raw, &row); err != nil {
				fmt.Fprintf(os.Stderr, "FAILED decoding observation row from %s: %v\n", *in, err)
				return 1
			}
			res, err := st.ImportRow(ctx, row)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAILED importing row (content_hash=%s): %v\n", row.ContentHash, err)
				return 1
			}
			if res.Inserted {
				imported++
			} else {
				skipped++
			}
		case memory.PromptRowKind:
			var row memory.PromptRow
			if err := json.Unmarshal(raw, &row); err != nil {
				fmt.Fprintf(os.Stderr, "FAILED decoding prompt row from %s: %v\n", *in, err)
				return 1
			}
			inserted, err := st.ImportPrompt(ctx, row)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAILED importing prompt (session_id=%s, prompt_number=%d): %v\n", row.SessionID, row.PromptNumber, err)
				return 1
			}
			if inserted {
				promptsImported++
			} else {
				promptsSkipped++
			}
		default:
			fmt.Fprintf(os.Stderr, "FAILED importing row from %s: unknown kind %q\n", *in, probe.Kind)
			return 1
		}
	}

	fmt.Printf("Imported %d observation(s), skipped %d already present (matched by content_hash).\n", imported, skipped)
	if promptsImported > 0 || promptsSkipped > 0 {
		fmt.Printf("Imported %d prompt(s), skipped %d already present (matched by session_id + prompt_number).\n", promptsImported, promptsSkipped)
	}
	return 0
}
