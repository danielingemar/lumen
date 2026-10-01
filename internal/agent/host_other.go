//go:build !linux && !windows

package agent

// Host metrics are implemented for Linux and Windows; other platforms report nothing.
type Host struct{ Service string }

func (h *Host) Collect(int64) []Point { return nil }
