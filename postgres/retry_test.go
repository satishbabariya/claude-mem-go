package postgres

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"testing"
	"time"
)

// unreachablePortDSN targets a real, permanently-closed local TCP port —
// confirmed by hand to fail with a fast "connection refused" (a few ms),
// not a slow OS-level timeout, so these tests are deterministic and quick
// without needing a real Docker container to test the retry LOOP's own
// logic (timing, cancellation) in isolation from an actual outage.
const unreachablePortDSN = "postgres://nouser:nopass@127.0.0.1:1/nodb?sslmode=disable"

func openUnreachableDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", unreachablePortDSN)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPingWithRetryStopsOnContextCancellation(t *testing.T) {
	db := openUnreachableDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := pingWithRetry(ctx, db, connectRetryBackoff)
	if err == nil {
		t.Fatal("pingWithRetry against an already-canceled context: want an error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("pingWithRetry took %s after an already-canceled context — want it to stop immediately instead of sleeping through connectRetryBackoff's real schedule", elapsed)
	}
}

// TestPingWithRetryActuallyRetries is the regression test for the bug this
// whole feature closes: before pingWithRetry existed, Open failed on the
// very first ping with no retry at all. A single-attempt schedule and a
// multi-attempt schedule against the identical unreachable target must
// take measurably different amounts of time, or the "retry" isn't
// actually retrying anything.
func TestPingWithRetryActuallyRetries(t *testing.T) {
	db := openUnreachableDB(t)

	oneAttempt := []time.Duration{0}
	start := time.Now()
	if err := pingWithRetry(context.Background(), db, oneAttempt); err == nil {
		t.Fatal("want an error against an unreachable port")
	}
	singleAttemptElapsed := time.Since(start)

	fiveAttempts := []time.Duration{0, 20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}
	start = time.Now()
	if err := pingWithRetry(context.Background(), db, fiveAttempts); err == nil {
		t.Fatal("want an error against an unreachable port")
	}
	fiveAttemptElapsed := time.Since(start)

	if fiveAttemptElapsed < singleAttemptElapsed+60*time.Millisecond {
		t.Errorf("5-attempt schedule took %s, only-slightly-more than the 1-attempt schedule's %s — want it to have actually slept through the backoff intervals between retries", fiveAttemptElapsed, singleAttemptElapsed)
	}
}

// --- Real Docker container test below ---
//
// Manipulates the shared docker-compose postgres container's running
// state, unlike every other test in this package — run standalone
// (`go test ./postgres/... -run TestPostgresOpenRecoversFromContainerRestart -v`),
// not as part of the normal suite, since stopping the container partway
// through `go test ./...` could make an unrelated test in this same
// package flake on a container that's mid-restart. Always leaves the
// container running afterward, even on failure.

func composeCmd(t *testing.T, args ...string) error {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = ".." // docker-compose.yml lives at the repo root, not postgres/
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("docker compose %v: %v\n%s", args, err, out)
	}
	return err
}

func waitUntilReachable(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, err := Open(ctx, testDSN(), DefaultEmbedDims)
		cancel()
		if err == nil {
			st.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("postgres did not become reachable again within %s of cleanup", timeout)
}

// TestPostgresOpenRecoversFromContainerRestart is the real end-to-end
// version of the two tests above: stops the actual docker-compose postgres
// container, calls Open, and confirms it recovers once the container comes
// back up mid-retry — instead of failing permanently on the very first
// ping the moment the container merely isn't ready yet. This is the exact
// production scenario connectRetryBackoff exists for: the worker daemon
// (or any hook) starting a beat before Postgres's own container finishes
// its healthcheck (docker-compose.yml allows up to 40s for that).
func TestPostgresOpenRecoversFromContainerRestart(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if os.Getenv("CLAUDE_MEM_GO_TEST_POSTGRES_DSN") != "" {
		t.Skip("CLAUDE_MEM_GO_TEST_POSTGRES_DSN overridden — this test only knows how to stop/start THIS project's own docker-compose container")
	}
	// CI's postgres service is a GitHub Actions `services:` container, not
	// one started via `docker compose up` against this repo's
	// docker-compose.yml — `compose ps -q` for a service compose never
	// started returns nothing, which is exactly the signal to skip rather
	// than risk stopping/restarting a container this test doesn't actually
	// own the lifecycle of.
	psCmd := exec.Command("docker", "compose", "ps", "-q", "postgres")
	psCmd.Dir = ".."
	out, err := psCmd.Output()
	if err != nil || len(out) == 0 {
		t.Skip("postgres isn't running under this repo's docker-compose.yml (e.g. CI's services: container) — nothing this test can safely stop/restart")
	}

	if err := composeCmd(t, "stop", "postgres"); err != nil {
		t.Skipf("could not stop the postgres container (docker compose not usable here?): %v", err)
	}
	t.Cleanup(func() {
		_ = composeCmd(t, "start", "postgres")
		waitUntilReachable(t, 30*time.Second)
	})

	go func() {
		time.Sleep(1500 * time.Millisecond)
		_ = composeCmd(t, "start", "postgres")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	st, err := Open(ctx, testDSN(), DefaultEmbedDims)
	if err != nil {
		t.Fatalf("Open did not recover once the container came back (waited %s): %v", time.Since(start), err)
	}
	defer st.Close()

	if elapsed := time.Since(start); elapsed < 1*time.Second {
		t.Errorf("Open returned successfully after only %s — expected it to actually retry past the container being down for ~1.5s, not get lucky on the very first attempt", elapsed)
	}
}
