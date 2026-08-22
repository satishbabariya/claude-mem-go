package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	claudeagent "github.com/satishbabariya/claude-agent-sdk-go"

	"github.com/satishbabariya/claude-mem-go/internal/embed"
	"github.com/satishbabariya/claude-mem-go/internal/memory"
	"github.com/satishbabariya/claude-mem-go/internal/memory/backend"
	"github.com/satishbabariya/claude-mem-go/internal/plugincheck"
	"github.com/satishbabariya/claude-mem-go/internal/worker"
)

// cmdDoctor is an operational health check — the kind of thing a real
// deployment needs and a personal dev setup can get away without: is the
// model backend reachable, is the database reachable, is the worker up, is
// semantic search actually usable. Distinguishes critical failures (claude
// CLI missing, database unreachable — nothing works without these) from
// informational ones (worker not running — `start` launches it lazily;
// Ollama unreachable — keyword search still works, just not semantic).
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dbPath := fs.String("db", memory.DefaultDBPath(), "sqlite file path, or a postgres:// DSN for the Postgres+pgvector backend")
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker listens on")
	// Parallel to -socket, and for the same reason: a daemon can be run
	// on a non-default socket and stats path (the worker subcommand has
	// taken both since it existed), and doctor could point at the first
	// but not the second — so it silently read a DIFFERENT daemon's
	// stats file than the socket it was probing.
	statsPath := fs.String("stats", worker.DefaultStatsPath(), "worker stats file to read (must match the daemon on -socket)")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model semantic search would use")
	hnswEfSearch := fs.Int("hnsw-ef-search", 0, "Postgres backend only: the hnsw.ef_search override configured "+
		"elsewhere (mcp/semantic-search/prompt-context), so its HealthDetails reflects the same value — "+
		"valid range 1-1000 (default 0 leaves pgvector's own default of 40 in place)")
	fs.Parse(args)

	critical := true

	buildInfo, _ := debug.ReadBuildInfo()
	fmt.Println("doctor —", buildVersionString(buildInfo))
	fmt.Println()

	if path, err := claudeagent.FindClaudeExecutable(); err != nil {
		fmt.Printf("✘ claude CLI: %v\n", err)
		critical = false
	} else {
		fmt.Printf("✔ claude CLI found at %s\n", path)
	}

	// The single most consequential thing this check can tell an operator,
	// and the one doctor had no way to ask before: every automatic capture
	// path (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop)
	// runs ONLY because a real plugin installation wires hooks/hooks.json
	// in. Without it, everything below can pass — claude CLI present,
	// database reachable, Ollama serving — while nothing is ever captured
	// and the memory store stays permanently empty.
	//
	// Reported prominently but NOT as a critical failure, deliberately
	// diverging from real claude-mem's own doctor (which marks the
	// equivalent check required): there, an installed plugin is the only
	// way the product runs at all, whereas this port's CLI subcommands
	// (search/export/prune) and MCP server are genuinely first-class
	// without it, and `--plugin-dir` runs the hooks for real without ever
	// touching the installed-plugins manifest. Hard-failing would report a
	// broken install for setups that are working exactly as intended.
	// Hoisted out of the if-statement's scope: the store-contents check
	// further down needs to know whether the plugin is installed, because
	// "empty store" is only a failure when capture is actually configured.
	installed, installs := plugincheck.IsInstalled(plugincheck.DefaultManifestPath())
	if installed {
		fmt.Printf("✔ plugin %q installed", plugincheck.PluginName)
		for i, in := range installs {
			if i == 0 {
				fmt.Print(" (")
			} else {
				fmt.Print(", ")
			}
			fmt.Printf("scope=%s version=%s", in.Scope, in.Version)
			if i == len(installs)-1 {
				fmt.Print(")")
			}
		}
		fmt.Println()

		// "Installed" and "able to run" are different states, and only
		// the first was checked before this. Every hook resolves
		// "$CLAUDE_PLUGIN_ROOT/claude-mem-go", a gitignored binary built
		// separately and copied into the plugin cache in whatever state
		// the tree was in at install time — so an install can be present
		// and well-formed while every hook silently fails to execute.
		// Critical ONLY when the plugin is installed, exactly matching
		// real claude-mem's own "Marketplace runtime" check
		// (required: installed): that preserves the deliberate carve-out
		// above for --plugin-dir and CLI/MCP-only users, who never had a
		// plugin install for this to be true of in the first place, while
		// still hard-failing the case where someone HAS installed and it
		// genuinely cannot work.
		for _, in := range installs {
			binVersion, berr := plugincheck.BinaryStatus(in)
			if berr != nil {
				fmt.Printf("✘ plugin binary unusable (scope=%s): %v\n", in.Scope, berr)
				critical = false
				continue
			}
			fmt.Printf("  ↳ binary OK (scope=%s): %s\n", in.Scope, binVersion)
			// A rebuilt-but-not-reinstalled tree is a real and easy state
			// to end up in — the plugin cache holds a COPY, so `go build`
			// alone never updates it. Informational, not critical: a stale
			// binary still runs, it's just not the code the operator
			// thinks they're running.
			if running := buildVersionString(buildInfo); running != binVersion {
				fmt.Printf("  ↳ … note: the installed binary differs from this one (%s) — rebuild and reinstall the plugin to sync them\n", running)
			}
		}
	} else {
		fmt.Printf("… plugin %q is NOT installed — automatic capture is inactive: no hooks fire, so nothing is being recorded.\n", plugincheck.PluginName)
		fmt.Printf("  Install it to enable capture, or ignore this if you're using --plugin-dir or only the CLI/MCP surface.\n")
	}

	// Captured, because everything derived from the stats file below is a
	// claim about a LIVE daemon and is meaningless without one.
	workerRunning := worker.IsRunning(*socketPath)
	if workerRunning {
		fmt.Printf("✔ worker daemon reachable at %s\n", *socketPath)
	} else {
		fmt.Printf("… worker daemon not running at %s (not necessarily a problem — `start` launches it lazily from SessionStart)\n", *socketPath)
	}

	redactedDBPath := memory.RedactDSN(*dbPath)

	// Informational only, never critical: a missing stats file just means
	// the worker hasn't processed anything yet (or predates this feature),
	// not that anything is broken.
	//
	// A dead daemon leaves its stats file behind, so everything here is
	// history unless the daemon is actually running. Reading this output
	// as an operator would is what exposed that: doctor reported "worker
	// daemon not running" and then, from the same leftover file, its pool
	// saturation, its build version, and a CRITICAL store mismatch —
	// failing the whole run over a daemon that did not exist. The live
	// checks below are gated on workerRunning for that reason; the
	// activity line survives either way but says which it is.
	if stats, err := worker.ReadStatsFile(*statsPath); err == nil {
		if workerRunning {
			fmt.Print("… worker activity: ")
		} else {
			fmt.Print("… last worker activity before it stopped: ")
		}
		recallAll, recallEmptyAll := stats.RecallTotals()
		recallHit := recallAll - recallEmptyAll
		fmt.Printf("processed=%d duplicates=%d observer_errors=%d insert_errors=%d embed_errors=%d pool=%d/%d cached_sessions=%d recall=%d/%d",
			stats.Processed, stats.Duplicates, stats.ObserverErrors, stats.InsertErrors, stats.EmbedErrors,
			stats.PoolInFlight, stats.PoolCapacity, stats.CachedSessions,
			recallHit, recallAll)
		if stats.LastActivityAt != "" {
			fmt.Printf(" last_activity=%s", stats.LastActivityAt)
		}
		fmt.Println()
		// The read path's own failure signal. A recall that returns
		// nothing raises no error and looks exactly like "nothing was
		// relevant", so a persistently empty rate is the only way this
		// surfaces at all — and this project has shipped two bugs (a
		// cross-project leak, a project post-filter returning zero rows
		// against 60,000 observations) that would have shown up here and
		// nowhere else. Reported, never critical: on a young store,
		// empty recalls are simply correct.
		// Judged on the prompt and session paths only. The file-context
		// lookup legitimately finds nothing most of the time — most files
		// have never been touched before — so including it would push the
		// rate high on a perfectly healthy install and train the operator
		// to ignore this line.
		if alarmAll, alarmEmpty := stats.RecallAlarming(); alarmAll >= minRecallsToJudge && alarmEmpty*2 > alarmAll {
			fmt.Printf("  ↳ %d of %d prompt/session recalls returned NOTHING — expected on a nearly-empty store, but if it has content, memory is reaching none of it\n",
				alarmEmpty, alarmAll)
		}
		// Printed unconditionally when anything has been recalled at all:
		// the breakdown is what any actual diagnosis needs, and the file
		// path's own rate is meaningful to a reader even though it is not
		// alarming on its own.
		if recallAll > 0 {
			for _, nr := range stats.RecallAll() {
				if nr.Stat.Searches > 0 {
					fmt.Printf("  ↳ recall[%s]: %d of %d returned results\n", nr.Source, nr.Stat.Searches-nr.Stat.Empty, nr.Stat.Searches)
				}
			}
		}

		// The daemon opened its store once, at start, and nothing
		// re-reads $CLAUDE_MEM_DB afterwards — correctly, since a daemon
		// switching databases underneath in-flight work would be worse.
		// But that means a worker started before the variable changed
		// keeps writing to the OLD store while every hook, every CLI
		// command and this very check resolve the new one.
		//
		// Reproduced end to end: worker started on store A,
		// CLAUDE_MEM_DB then pointed at B, one PostToolUse event — the
		// observation landed in A, while doctor reported "worker daemon
		// reachable", "database reachable (B)" and "the store is empty —
		// nothing recorded yet". Every check green, memory in another
		// file. Critical, because every automatic capture path goes
		// through the daemon: whatever this command reads is not where
		// anything is being written.
		// A saturated pool is the observable symptom of a real capture
		// stall. A slot is held for a cached observer session's whole
		// lifetime, so once every slot is taken a NEW session's
		// observations wait — and before this was surfaced they waited
		// silently while every check here still reported green. Found by
		// running three real concurrent sessions against the default
		// capacity of 2: two were captured, the third produced nothing.
		if workerRunning && stats.PoolCapacity > 0 && stats.PoolInFlight >= stats.PoolCapacity {
			fmt.Printf("… all %d observer slot(s) are held by cached sessions — a NEW concurrent\n"+
				"  session's observations will wait, and eventually be dropped, until one goes idle.\n"+
				"  Raise -max-concurrent if you routinely run more than %d sessions at once.\n",
				stats.PoolCapacity, stats.PoolCapacity)
		}

		// A daemon running older code than this binary is the same class
		// of problem as the store mismatch below, and was found the same
		// way — by running the loop end to end. `start` now replaces such
		// a daemon automatically, so seeing this here means that did not
		// happen: the daemon predates the version field, or the
		// replacement failed. Informational rather than critical for
		// exactly that reason — a stale daemon still captures, it just
		// may apply older rules.
		if workerRunning && stats.Version != "" && stats.Version != buildVersionString(buildInfo) {
			fmt.Printf("… the worker daemon is running an older build than this binary:\n")
			fmt.Printf("    worker:    %s\n", stats.Version)
			fmt.Printf("    this cmd:  %s\n", buildVersionString(buildInfo))
			fmt.Printf("    It applies the rules it started with — including how project names are derived —\n")
			fmt.Printf("    so writes and reads can silently disagree. SessionStart replaces it automatically;\n")
			fmt.Printf("    if this persists, stop the daemon (pid %d) and let the next session respawn it.\n", stats.PID)
		}

		if workerRunning && stats.Store != "" && stats.Store != redactedDBPath {
			fmt.Printf("✘ the worker daemon is writing to a DIFFERENT store than this command reads:\n")
			fmt.Printf("    worker:    %s\n", stats.Store)
			fmt.Printf("    this cmd:  %s\n", redactedDBPath)
			fmt.Printf("    Every captured observation goes to the worker's store. Restart the daemon to pick up\n")
			fmt.Printf("    the current $%s (stop it and let SessionStart respawn it).\n", memory.DBPathEnvVar)
			critical = false
		}
	}

	// Say WHICH source chose this store, not just which store won. The
	// failure this exists for is a split brain that looks like data loss:
	// hooks and MCP write to one backend while the operator's `-db`
	// commands read another, and the symptom is "my memory is empty" with
	// nothing anywhere naming a second database. Printing the source turns
	// that into a one-line diagnosis. Only shown when the env var is
	// actually set, so the common case stays quiet.
	if envDB := strings.TrimSpace(os.Getenv(memory.DBPathEnvVar)); envDB != "" {
		switch {
		case *dbPath == envDB:
			fmt.Printf("  store selected by $%s (hooks and the MCP server use this too)\n", memory.DBPathEnvVar)
		default:
			// An explicit -db beat the env var. Worth saying out loud: it
			// means THIS command is not looking at the store the hooks are
			// writing to, which is exactly when someone concludes their
			// memory vanished.
			fmt.Printf("  note: -db overrides $%s (=%s), which is what hooks and the MCP server will still use\n",
				memory.DBPathEnvVar, memory.RedactDSN(envDB))
		}
	}

	var st memory.Backend
	ctx, cancel := cliContext()
	defer cancel()
	if opened, err := backend.Open(ctx, *dbPath, 0, *hnswEfSearch); err != nil {
		fmt.Printf("✘ database (%s): %v\n", redactedDBPath, err)
		critical = false
	} else {
		st = opened
		defer st.Close()
		if _, cerr := st.CountByProject(ctx, ""); cerr != nil {
			fmt.Printf("✘ database (%s) opened but a query failed: %v\n", redactedDBPath, cerr)
			critical = false
		} else {
			fmt.Printf("✔ database reachable (%s)\n", redactedDBPath)
		}
		// What the store CONTAINS, not just whether it answers. Every
		// other check here is a reachability check, so doctor could
		// report "All critical checks passed" while capture had been
		// silently dead for weeks — the most likely failure this design
		// has, since PostToolUse is fire-and-forget and a failing hook
		// writes to a log nobody reads.
		//
		// Reported factually rather than judged against an invented
		// staleness threshold: how long is "too long" between
		// observations depends entirely on how much the operator is
		// using Claude Code, and a wrong guess here would either cry wolf
		// or reassure falsely. The one unambiguous case — a store that
		// has never recorded anything while the plugin IS installed, so
		// capture is configured and demonstrably not working — is called
		// out as a real problem.
		var embeddedRows int
		if st != nil {
			if sst, serr := st.Stats(ctx); serr == nil {
				embeddedRows = sst.Embedded
				if sst.Observations == 0 {
					if installed {
						fmt.Println("✘ the store is EMPTY, but the plugin is installed — capture is configured and not working")
						critical = false
					} else {
						fmt.Println("… the store is empty — nothing recorded yet (expected if the plugin isn't installed)")
					}
				} else {
					fmt.Printf("… store: %d observations across %d project(s) and %d session(s); newest %s\n",
						sst.Observations, sst.Projects, sst.Sessions, formatAge(sst.NewestEpochMs))
					if missing := sst.Observations - sst.Embedded; missing > 0 {
						fmt.Printf("  ↳ %d not embedded — invisible to semantic search until `reembed` runs\n", missing)
					}
					if sst.ByType["summary"] == 0 && sst.Sessions > 1 {
						fmt.Printf("  ↳ no session summaries across %d sessions — the Stop hook may not be completing\n", sst.Sessions)
					}
				}
			}
		}

		// Informational only, never critical on its own — a detail like
		// hnsw_index_exists=false is a real problem worth surfacing, but
		// it's a degraded-performance signal, not "nothing works."
		if details, herr := st.HealthDetails(ctx); herr == nil {
			reportEfSearchRecall(details, embeddedRows)
			keys := make([]string, 0, len(details))
			for k := range details {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Print("  ")
			for i, k := range keys {
				if i > 0 {
					fmt.Print(" ")
				}
				fmt.Printf("%s=%s", k, details[k])
			}
			fmt.Println()
		}
	}

	client := embed.NewClient(*embedModel)
	if err := client.Ping(); err != nil {
		fmt.Printf("… semantic search unavailable: %v (keyword search still works)\n", err)
	} else {
		fmt.Printf("✔ Ollama reachable, model %q pulled — semantic search available\n", *embedModel)
		// embedding_dims_consistent (above) only catches internal
		// disagreement between stored embeddings — it says nothing about
		// whether what's stored actually matches the model that's live
		// RIGHT NOW. A store embedded entirely under a since-replaced
		// model would report "consistent" while every single embedding is
		// silently unsearchable under the current one. A real probe embed
		// (the only way to learn what this model's dimension actually is;
		// nothing here maintains a name-to-dimension lookup table) plus
		// the exact same query `reembed` uses (capped at 1 row — this is
		// a cheap presence check, not a full scan) catches that case too.
		if st != nil {
			if probe, perr := client.Embed("dimension probe"); perr == nil {
				if needing, nerr := st.ObservationsNeedingEmbedding(ctx, "", int64(len(probe)), 0, 1); nerr == nil && len(needing) > 0 {
					fmt.Printf("… some observations need (re-)embedding with the current model (%d dims) — run `reembed` for a full count and to fix it\n", len(probe))
				}
			}
		}
	}

	fmt.Println()
	if !critical {
		fmt.Println("Critical checks failed — claude-mem-go will not function until these are fixed.")
		return 1
	}
	fmt.Println("All critical checks passed.")
	return 0
}

// efSearchRecallFloor is the corpus size at which pgvector's default
// hnsw.ef_search stops being good enough, and it is a measured number
// rather than a guessed one.
//
// bench/recall measures recall@10 against a forced exact scan on real
// nomic-embed-text embeddings. On 20,000 distinct vectors the default
// ef_search of 40 returns 80% — one relevant memory in five simply
// missing, with no error and no way for the operator to notice, which is
// the same silent-degradation shape as the project post-filter bug
// SemanticSearch already documents. Raising the knob fixes it cheaply:
// 200 gives 94% and 400 gives 98%, and the p50 query cost across that
// whole range stayed under 4ms on the same corpus.
//
// The threshold is set at 10,000 because that is also, measured with
// EXPLAIN on the same data, roughly where the planner starts choosing the
// HNSW index over a sequential scan at all. Below it the scan is exact
// and ef_search is irrelevant; above it the approximation is live and
// unmeasured by anything the operator can see.
//
// Deliberately a WARNING and not a changed default. The measurement
// covers one embedding model on one corpus, and silently altering search
// behaviour for every existing store on that evidence would be a bigger
// claim than the evidence supports. Telling the operator the number, and
// the flag that fixes it, is the honest version.
const efSearchRecallFloor = 10000

// minRecallsToJudge is how many recalls must have happened before an
// empty RATE means anything. Two empties out of two is a brand-new
// install, not a broken one, and a check that fires there would train the
// operator to ignore it.
const minRecallsToJudge = 10

// reportEfSearchRecall warns when a store is large enough for the ANN
// approximation to matter while still running pgvector's default.
func reportEfSearchRecall(details map[string]string, embedded int) {
	if details["hnsw_ef_search"] != "default (40)" {
		return // an override is configured; the operator has already chosen
	}
	if embedded < efSearchRecallFloor {
		return
	}
	fmt.Printf("… %d embedded rows with hnsw.ef_search at pgvector's default (40) — measured at ~80%% recall@10\n", embedded)
	fmt.Println("  ↳ project-scoped search — what every hook uses — measured worse still, ~71%")
	fmt.Println("  ↳ -hnsw-ef-search 200 measured ~94% unscoped and exact-and-complete scoped, under 4ms p50 (see bench/recall)")
}
