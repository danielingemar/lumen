//go:build windows

package agent

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	k32             = syscall.NewLazyDLL("kernel32.dll")
	pGetSystemTimes = k32.NewProc("GetSystemTimes")
	pMemStatus      = k32.NewProc("GlobalMemoryStatusEx")
	pDiskFree       = k32.NewProc("GetDiskFreeSpaceExW")
)

// Host collects basic Windows host metrics through kernel32 (no external dependencies).
// CPU usage is a delta between two calls, so the first call omits it.
type Host struct {
	Service        string
	prevIdle, prev uint64
}

type memoryStatusEx struct {
	Length                                      uint32
	MemoryLoad                                  uint32
	TotalPhys, AvailPhys                        uint64
	TotalPageFile, AvailPageFile                uint64
	TotalVirtual, AvailVirtual, AvailExtVirtual uint64
}

func ft(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

func (h *Host) Collect(now int64) []Point {
	pt := func(name string, v float64, a map[string]string) Point {
		return Point{Service: h.Service, Name: name, Type: "gauge", Value: v, Attrs: a, TimeNs: now}
	}
	var out []Point
	var idle, kernel, user syscall.Filetime
	if r, _, _ := pGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r != 0 {
		i, total := ft(idle), ft(kernel)+ft(user) // kernel time includes idle time
		if h.prev != 0 && total > h.prev {
			out = append(out, pt("system.cpu.utilization", 1-float64(i-h.prevIdle)/float64(total-h.prev), nil))
		}
		h.prev, h.prevIdle = total, i
	}
	m := memoryStatusEx{}
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := pMemStatus.Call(uintptr(unsafe.Pointer(&m))); r != 0 {
		out = append(out, pt("system.memory.total", float64(m.TotalPhys), nil),
			pt("system.memory.used", float64(m.TotalPhys-m.AvailPhys), nil))
	}
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	if p, err := syscall.UTF16PtrFromString(drive + `\`); err == nil {
		var avail, total, free uint64
		if r, _, _ := pDiskFree.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free))); r != 0 {
			a := map[string]string{"mountpoint": drive}
			out = append(out, pt("system.filesystem.total", float64(total), a), pt("system.filesystem.used", float64(total-avail), a))
		}
	}
	return out
}
