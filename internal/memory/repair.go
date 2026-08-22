package memory

import "path/filepath"

// SplitRelativePaths partitions paths into the absolute ones to keep and
// the relative ones RepairFilePaths drops. Both backends implement the
// repair as "select candidates, filter in Go, update", so the rule lives
// here once.
func SplitRelativePaths(paths []string) (keep, dropped []string) {
	for _, p := range paths {
		if filepath.IsAbs(p) {
			keep = append(keep, p)
		} else {
			dropped = append(dropped, p)
		}
	}
	if keep == nil {
		keep = []string{}
	}
	return keep, dropped
}
