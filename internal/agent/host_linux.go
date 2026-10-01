//go:build linux

package agent

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Host collects basic host metrics from /proc. CPU usage is a delta between two calls,
// so the first call omits it.
type Host struct {
	Service        string
	prevIdle, prev uint64
}

func (h *Host) Collect(now int64) []Point {
	pt := func(name string, v float64, a map[string]string) Point {
		return Point{Service: h.Service, Name: name, Type: "gauge", Value: v, Attrs: a, TimeNs: now}
	}
	var out []Point
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		if f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0]); len(f) >= 8 && f[0] == "cpu" {
			var total, idle uint64
			for i, s := range f[1:9] {
				v, _ := strconv.ParseUint(s, 10, 64)
				total += v
				if i == 3 || i == 4 { // idle + iowait
					idle += v
				}
			}
			if h.prev != 0 && total > h.prev {
				out = append(out, pt("system.cpu.utilization", 1-float64(idle-h.prevIdle)/float64(total-h.prev), nil))
			}
			h.prev, h.prevIdle = total, idle
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		mem := map[string]float64{}
		for _, ln := range strings.Split(string(b), "\n") {
			if f := strings.Fields(ln); len(f) >= 2 {
				v, _ := strconv.ParseFloat(f[1], 64)
				mem[strings.TrimSuffix(f[0], ":")] = v * 1024
			}
		}
		if t := mem["MemTotal"]; t > 0 {
			out = append(out, pt("system.memory.total", t, nil), pt("system.memory.used", t-mem["MemAvailable"], nil))
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			for i, n := range []string{"system.load.1m", "system.load.5m", "system.load.15m"} {
				v, _ := strconv.ParseFloat(f[i], 64)
				out = append(out, pt(n, v, nil))
			}
		}
	}
	var fs syscall.Statfs_t
	if syscall.Statfs("/", &fs) == nil {
		total := float64(fs.Blocks) * float64(fs.Bsize)
		out = append(out, pt("system.filesystem.total", total, map[string]string{"mountpoint": "/"}),
			pt("system.filesystem.used", total-float64(fs.Bavail)*float64(fs.Bsize), map[string]string{"mountpoint": "/"}))
	}
	return out
}
