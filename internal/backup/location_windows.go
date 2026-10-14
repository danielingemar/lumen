//go:build windows

package backup

func deviceOf(string) (uint64, bool) { return 0, false }
