//go:build !windows

package backup

import "syscall"

// deviceOf says which file system a path is on.
func deviceOf(path string) (uint64, bool) {
	var s syscall.Stat_t
	if syscall.Stat(path, &s) != nil {
		return 0, false
	}
	return uint64(s.Dev), true
}
