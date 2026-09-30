package worker

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/satishbabariya/claude-mem-go/internal/sockettest"
)

func TestIsRunningFalseWhenNothingListens(t *testing.T) {
	path := filepath.Join(sockettest.Dir(t), "nonexistent.sock")
	if IsRunning(path) {
		t.Fatal("IsRunning on a socket nobody is listening on: want false, got true")
	}
}

func TestIsRunningTrueWhenListening(t *testing.T) {
	path := filepath.Join(sockettest.Dir(t), "test.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	if !IsRunning(path) {
		t.Fatal("IsRunning on a live listener: want true, got false")
	}
}

func TestAcquireSpawnLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spawn.lock")

	if !AcquireSpawnLock(path) {
		t.Fatal("first AcquireSpawnLock on a fresh path: want true")
	}
	if AcquireSpawnLock(path) {
		t.Fatal("second AcquireSpawnLock while the first still holds a fresh lock: want false (contention)")
	}
	ReleaseSpawnLock(path)
	if !AcquireSpawnLock(path) {
		t.Fatal("AcquireSpawnLock after Release: want true (lock was actually released)")
	}
}

func TestAcquireSpawnLockBreaksStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spawn.lock")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d", os.Getpid()+999999)), 0o600); err != nil {
		t.Fatalf("writing fake stale lock: %v", err)
	}
	// Back-date the lock file well past spawnLockStaleAfter so it reads as abandoned.
	old := time.Now().Add(-2 * spawnLockStaleAfter)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("os.Chtimes: %v", err)
	}

	if !AcquireSpawnLock(path) {
		t.Fatal("AcquireSpawnLock on a stale lock: want true (breaks and re-acquires)")
	}
}

func TestReleaseSpawnLockIsOwnerChecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spawn.lock")
	otherPID := os.Getpid() + 999999
	if err := os.WriteFile(path, []byte(strconv.Itoa(otherPID)), 0o600); err != nil {
		t.Fatalf("writing fake foreign lock: %v", err)
	}

	ReleaseSpawnLock(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatal("ReleaseSpawnLock deleted a lock file it does not own — a launcher must never delete a competitor's live lock")
	}
}

func TestValidateSocketPathRejectsPathsTheKernelCannotBind(t *testing.T) {
	if err := ValidateSocketPath("/tmp/ok.sock"); err != nil {
		t.Fatalf("short path rejected: %v", err)
	}
	long := "/tmp/" + strings.Repeat("d", 200) + "/w.sock"
	err := ValidateSocketPath(long)
	if err == nil {
		t.Fatal("a path over sun_path's limit was accepted; bind(2) would fail with EINVAL")
	}
	if !strings.Contains(err.Error(), "-socket") {
		t.Fatalf("error does not tell the operator how to fix it: %v", err)
	}
	if err := ValidateSocketPath(""); err == nil {
		t.Fatal("empty path accepted")
	}
}
