package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/cli"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
)

// cmdStats reports what the store actually CONTAINS.
//
// `doctor` answers "is the machinery working" — every check it runs is a
// reachability check. That left the more important question unanswerable:
// is anything actually being remembered? Those come apart in exactly this
// project's most likely failure mode, because PostToolUse is
// fire-and-forget, so a hook that fails writes to a log nobody reads.
// Every component stays reachable, `doctor` reports "All critical checks
// passed", and the store quietly stops growing.
//
// The local counterpart of real claude-mem's db_observation_count /
// db_session_count / db_project_count / days_since_last_obs figures,
// which it collects as outbound telemetry. Reported to the operator here
// instead of sent anywhere — the same stance this port takes on cloud
// sync.
func cmdStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dbPath := cli.DBFlag(fs)
	fs.Parse(args)

	ctx, cancel := cliContext()
	defer cancel()
	st, err := backend.Open(ctx, *dbPath, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED opening store at %s: %v\n", memory.RedactDSN(*dbPath), err)
		return 1
	}
	defer st.Close()

	s, err := st.Stats(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED reading stats: %v\n", err)
		return 1
	}

	fmt.Printf("store: %s\n\n", memory.RedactDSN(*dbPath))
	if s.Observations == 0 && s.Prompts == 0 {
		fmt.Println("  The store is EMPTY — nothing has ever been recorded.")
		fmt.Println("  If the plugin is installed, capture is not working; run `doctor`.")
		return 0
	}

	fmt.Printf("  observations   %d\n", s.Observations)
	fmt.Printf("  projects       %d\n", s.Projects)
	fmt.Printf("  sessions       %d\n", s.Sessions)

	// The gap here is exactly what semantic search cannot see, which is
	// invisible from any other output.
	fmt.Printf("  embedded       %d", s.Embedded)
	if missing := s.Observations - s.Embedded; missing > 0 {
		fmt.Printf("  (%d not embedded — invisible to semantic search; `reembed` fixes it)", missing)
	}
	fmt.Println()
	// Only when the opt-in -store-prompts feature has ever written
	// anything: a line reading "prompts 0" on every store that never
	// enabled it would suggest something is missing when nothing is.
	if s.Prompts > 0 {
		fmt.Printf("  prompts        %d  (verbatim user prompts; -store-prompts is on)\n", s.Prompts)
	}

	if len(s.ByType) > 0 {
		types := make([]string, 0, len(s.ByType))
		for t := range s.ByType {
			types = append(types, t)
		}
		sort.Strings(types)
		fmt.Print("  by type        ")
		for i, t := range types {
			if i > 0 {
				fmt.Print("  ")
			}
			fmt.Printf("%s=%d", t, s.ByType[t])
		}
		fmt.Println()
		// A store with sessions but no summaries means the Stop hook is
		// not completing — visible nowhere else, since a missing summary
		// looks exactly like a session that was never summarized.
		if s.ByType["summary"] == 0 && s.Sessions > 1 {
			fmt.Printf("  … no `summary` observations across %d sessions — the Stop hook may not be firing\n", s.Sessions)
		}
	}

	fmt.Println()
	fmt.Printf("  oldest         %s\n", formatAge(s.OldestEpochMs))
	fmt.Printf("  newest         %s\n", formatAge(s.NewestEpochMs))
	return 0
}

// formatAge renders an epoch-milliseconds timestamp with how long ago it
// was, because the absolute time alone does not answer the question the
// operator is actually asking.
func formatAge(epochMs int64) string {
	if epochMs == 0 {
		return "(none)"
	}
	t := time.UnixMilli(epochMs)
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%s (just now)", t.Format(time.RFC3339))
	case d < time.Hour:
		return fmt.Sprintf("%s (%d minutes ago)", t.Format(time.RFC3339), int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%s (%d hours ago)", t.Format(time.RFC3339), int(d.Hours()))
	default:
		return fmt.Sprintf("%s (%d days ago)", t.Format(time.RFC3339), int(d.Hours()/24))
	}
}
