// Package sockettest gives tests a short, fixed-base directory to bind
// unix domain sockets in.
//
// t.TempDir() nests under $TMPDIR plus the package and test name, and on
// macOS/BSD that combination routinely exceeds sockaddr_un's ~104-byte
// sun_path limit — net.Listen("unix", ...) then fails with "bind: invalid
// argument" for a reason that has nothing to do with the code under
// test. $TMPDIR itself is often already most of that budget on macOS
// (a per-process path under /var/folders), so even trimming what comes
// after it is not enough; Dir ignores it entirely and asks the OS
// default (MkdirTemp("", ...) resolves to plain "/tmp" when TMPDIR is
// unset) directly instead.
package sockettest

import (
	"os"
	"testing"
)

// Dir returns a freshly created directory suitable for unix domain
// socket paths, short enough to stay under sun_path's limit regardless
// of how long the calling test's own name or $TMPDIR is. The directory
// is removed in test cleanup.
func Dir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cmg")
	if err != nil {
		t.Fatalf("sockettest.Dir: MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
