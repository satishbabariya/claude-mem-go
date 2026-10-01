// Command build-mcpb packs a staging directory into a deterministic
// .mcpb bundle: a zip file with manifest.json at its root (the MCPB
// format; see modelcontextprotocol/mcpb's MANIFEST.md).
//
// Deliberately not the upstream `mcpb pack` CLI (@anthropic-ai/mcpb):
// its zipSync call stamps every entry with time.Now() (see its
// src/cli/pack.ts, `mtime: new Date()`), so two independent builds of
// the exact same input directory produce different bytes purely because
// they ran a few seconds apart — the same reproducibility bug
// .goreleaser.yaml already works around for the release archives via
// mod_timestamp/builds_info.mtime (see that file's own comments). Fixed
// the same way here: every zip entry gets one caller-supplied timestamp
// instead of wall-clock "now", so release.yml's build and verify jobs
// (which run on separate runners, each invoking this tool once) produce
// byte-identical .mcpb files for the same commit.
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func main() {
	root := flag.String("root", "", "staging directory to pack (must contain manifest.json at its root)")
	out := flag.String("out", "", "output .mcpb path")
	mtimeFlag := flag.String("mtime", "", "RFC3339 timestamp stamped on every zip entry (required, for byte-reproducibility)")
	flag.Parse()

	if *root == "" || *out == "" || *mtimeFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: build-mcpb -root <staging-dir> -out <file.mcpb> -mtime <RFC3339>")
		os.Exit(2)
	}
	mtime, err := time.Parse(time.RFC3339, *mtimeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build-mcpb: invalid -mtime: %v\n", err)
		os.Exit(1)
	}
	if err := build(*root, *out, mtime); err != nil {
		fmt.Fprintf(os.Stderr, "build-mcpb: %v\n", err)
		os.Exit(1)
	}
}

func build(root, out string, mtime time.Time) error {
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); err != nil {
		return fmt.Errorf("no manifest.json at the root of %s: %w", root, err)
	}

	paths, err := collectFiles(root)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for _, rel := range paths {
		if err := addFile(zw, root, rel, mtime); err != nil {
			return err
		}
	}
	return zw.Close()
}

// collectFiles walks root and returns every regular file's path relative
// to root, sorted. Sorted explicitly rather than relying on
// filepath.WalkDir's lexical visit order as an implementation detail: a
// byte-reproducible zip needs a guaranteed entry order, not an
// incidental one that a future Go release or refactor could change.
func collectFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// addFile writes one zip entry. The file's mode (specifically, the
// executable bit on mcpb/launch.sh and the bundled platform binaries) is
// preserved via FileHeader.SetMode, the same Unix-permissions-in-the-
// upper-16-bits-of-ExternalAttrs convention @anthropic-ai/mcpb's own
// pack.ts uses and that unzip/MCPB-consuming hosts already expect —
// without it, launch.sh would land in the bundle without +x and fail to
// exec. The CI step that lays out the staging directory is responsible
// for chmod'ing launch.sh and the binaries before this runs; this
// function only preserves whatever mode is already on disk, deliberately
// not hardcoding one, so a staging-directory bug (missing chmod) shows up
// as a real permission failure when the bundle is launched rather than
// being silently papered over here.
func addFile(zw *zip.Writer, root, rel string, mtime time.Time) error {
	full := filepath.Join(root, rel)
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}

	fh := &zip.FileHeader{
		Name:     filepath.ToSlash(rel),
		Method:   zip.Deflate,
		Modified: mtime,
	}
	fh.SetMode(info.Mode().Perm())

	w, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
