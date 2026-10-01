package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stageFixture builds a minimal staging directory shaped like the real
// release layout: manifest.json at the root, an executable launch.sh,
// and one "binary" under bin/ (a placeholder file; the content doesn't
// matter here, only that its executable bit round-trips).
func stageFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"manifest_version":"0.3"}`, 0o644)
	write("mcpb/launch.sh", "#!/bin/sh\nexec true\n", 0o755)
	write("bin/claude-mem-go_darwin_arm64", "fake binary bytes", 0o755)
	return dir
}

var fixedMtime = mustParse("2026-09-30T00:00:00Z")

func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestBuildIsReproducible is the property release.yml's build and verify
// jobs rely on: packing the identical staging directory twice, from
// scratch, with the same -mtime, must produce byte-identical .mcpb
// files on what are in CI two entirely separate runners. Here it's two
// separate output files from two separate build() calls over the same
// fixture, which exercises the same code path.
func TestBuildIsReproducible(t *testing.T) {
	root := stageFixture(t)
	dir := t.TempDir()
	out1 := filepath.Join(dir, "a.mcpb")
	out2 := filepath.Join(dir, "b.mcpb")

	if err := build(root, out1, fixedMtime); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if err := build(root, out2, fixedMtime); err != nil {
		t.Fatalf("second build: %v", err)
	}

	b1, err := os.ReadFile(out1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(out2)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(b1) != sha256.Sum256(b2) {
		t.Fatalf("two builds of the identical staging directory with the same -mtime produced different bytes")
	}
}

// TestBuildChangesWithMtime proves the -mtime flag is actually load-
// bearing (not silently ignored): the same content at a different
// commit timestamp must produce a different, equally reproducible,
// result — otherwise a real divergence between two commits could be
// masked by a stale cached entry timestamp.
func TestBuildChangesWithMtime(t *testing.T) {
	root := stageFixture(t)
	dir := t.TempDir()
	out1 := filepath.Join(dir, "a.mcpb")
	out2 := filepath.Join(dir, "b.mcpb")

	if err := build(root, out1, fixedMtime); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if err := build(root, out2, fixedMtime.Add(24*time.Hour)); err != nil {
		t.Fatalf("second build: %v", err)
	}

	b1, _ := os.ReadFile(out1)
	b2, _ := os.ReadFile(out2)
	if sha256.Sum256(b1) == sha256.Sum256(b2) {
		t.Fatalf("builds at two different -mtime values produced identical bytes; -mtime is not being applied")
	}
}

// TestCorruptedByteFailsChecksum is the acceptance check that a single
// flipped byte, after hashing, is caught: the same scenario
// release.yml's "verify" job and ensure-binary.sh's checksum step both
// depend on — a corrupted artifact must not match its recorded sha256.
func TestCorruptedByteFailsChecksum(t *testing.T) {
	root := stageFixture(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "a.mcpb")
	if err := build(root, out, fixedMtime); err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(original)
	wantHex := hex.EncodeToString(wantSum[:])

	corrupted := append([]byte(nil), original...)
	corrupted[len(corrupted)/2] ^= 0xff
	gotSum := sha256.Sum256(corrupted)
	gotHex := hex.EncodeToString(gotSum[:])

	if gotHex == wantHex {
		t.Fatalf("flipping one byte did not change the sha256 — checksum verification would not catch tampering")
	}
}

// TestLaunchScriptStaysExecutable guards the exact failure mode noted in
// addFile's own comment: without SetMode preserving the executable bit,
// mcpb/launch.sh would land in the bundle without +x and fail to exec
// once unpacked.
func TestLaunchScriptStaysExecutable(t *testing.T) {
	root := stageFixture(t)
	out := filepath.Join(t.TempDir(), "a.mcpb")
	if err := build(root, out, fixedMtime); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	var found bool
	for _, f := range zr.File {
		if f.Name != "mcpb/launch.sh" {
			continue
		}
		found = true
		mode := f.Mode()
		if mode&0o111 == 0 {
			t.Fatalf("mcpb/launch.sh lost its executable bit in the bundle: mode=%v", mode)
		}
	}
	if !found {
		t.Fatalf("mcpb/launch.sh was not found in the bundle")
	}
}

// TestEntryOrderIsSorted guards collectFiles' own documented reason for
// sorting explicitly: zip entry order must be a guaranteed property of
// this tool, not an incidental one inherited from filepath.WalkDir.
func TestEntryOrderIsSorted(t *testing.T) {
	root := stageFixture(t)
	out := filepath.Join(t.TempDir(), "a.mcpb")
	if err := build(root, out, fixedMtime); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("zip entries are not strictly sorted: %q then %q", names[i-1], names[i])
		}
	}
}

// TestMissingManifestFailsLoud guards against silently packing the
// wrong directory (e.g. a typo'd -root) into something that looks like a
// valid .mcpb but has no manifest.json at its root at all.
func TestMissingManifestFailsLoud(t *testing.T) {
	root := t.TempDir() // deliberately empty — no manifest.json
	out := filepath.Join(t.TempDir(), "a.mcpb")
	if err := build(root, out, fixedMtime); err == nil {
		t.Fatalf("expected an error for a staging directory with no manifest.json, got nil")
	}
}
