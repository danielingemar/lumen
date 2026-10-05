//go:build linux

package agent

import (
	"os"
	"runtime"
	"strings"
	"syscall"
)

// Host collects host metrics from /proc and the file systems. Rates (CPU, disk I/O, network) are the difference
// between two calls, so the first call omits them.
//
// Names that existed before keep working (system.cpu.utilization, system.memory.used/total, system.load.*m,
// system.filesystem.used/total). New: CPU by state, memory by state, swap, every local disk's usage and inodes, disk
// I/O (throughput, operations, busy time per device), network traffic and errors per interface, processes, uptime.
type Host struct {
	Service string

	prevCPU  cpuTimes
	prevAt   int64
	prevDisk map[string]diskCounters
	prevNet  map[string]netCounters

	// prevIdle and prev are kept for tests written against the first version.
	prevIdle, prev uint64
}

func readFile(p string) (string, bool) {
	b, err := os.ReadFile(p)
	return string(b), err == nil
}

// wholeDisk reports whether a /proc/diskstats name is a whole disk (not a partition or a loop device). /sys/block lists
// whole disks only; without it the name decides.
func wholeDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "sr", "fd"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	if _, err := os.Stat("/sys/block"); err == nil {
		_, err := os.Stat("/sys/block/" + name)
		return err == nil
	}
	return wholeDiskByName(name)
}

func (h *Host) Collect(now int64) []Point {
	pt := func(name string, v float64, a map[string]string) Point {
		return Point{Service: h.Service, Name: name, Type: "gauge", Value: v, Attrs: a, TimeNs: now}
	}
	var out []Point
	dt := 0.0
	if h.prevAt > 0 && now > h.prevAt {
		dt = float64(now-h.prevAt) / 1e9
	}

	// ---- CPU ----
	if s, ok := readFile("/proc/stat"); ok {
		if cur, ok := parseCPUTimes(s); ok {
			if u, ok := cpuUtilization(h.prevCPU, cur); ok {
				out = append(out, pt("system.cpu.utilization", u, nil))
			}
			if sh, ok := cpuShares(h.prevCPU, cur); ok {
				for _, st := range []string{"user", "nice", "system", "iowait", "irq", "steal"} {
					out = append(out, pt("system.cpu.state", sh[st], map[string]string{"state": st}))
				}
			}
			h.prevCPU = cur
			h.prev, h.prevIdle = cur.total(), cur.idle+cur.iowait
		}
	}
	out = append(out, pt("system.cpu.count", float64(runtime.NumCPU()), nil))

	// ---- memory and swap ----
	if s, ok := readFile("/proc/meminfo"); ok {
		m := parseMeminfo(s)
		if t := m["MemTotal"]; t > 0 {
			out = append(out, pt("system.memory.total", t, nil), pt("system.memory.used", t-m["MemAvailable"], nil), pt("system.memory.available", m["MemAvailable"], nil))
			for st, v := range memoryStates(m) {
				out = append(out, pt("system.memory.usage", v, map[string]string{"state": st}))
			}
		}
		// a machine without swap reports 0 used, so "Swap in use" shows 0 B instead of "no data"
		st := m["SwapTotal"]
		out = append(out, pt("system.paging.total", st, nil),
			pt("system.paging.usage", st-m["SwapFree"], map[string]string{"state": "used"}), pt("system.paging.usage", m["SwapFree"], map[string]string{"state": "free"}))
	}

	// ---- load, processes, uptime ----
	if s, ok := readFile("/proc/loadavg"); ok {
		if l, ok := parseLoadavg(s); ok {
			out = append(out, pt("system.load.1m", l.l1, nil), pt("system.load.5m", l.l5, nil), pt("system.load.15m", l.l15, nil),
				pt("system.load.average", l.l1, map[string]string{"period": "1m"}), pt("system.load.average", l.l5, map[string]string{"period": "5m"}), pt("system.load.average", l.l15, map[string]string{"period": "15m"}),
				pt("system.processes.count", l.procs, nil), pt("system.processes.running", l.running, nil))
		}
	}
	if s, ok := readFile("/proc/uptime"); ok {
		if u, ok := parseUptime(s); ok {
			out = append(out, pt("system.uptime", u, nil))
		}
	}

	// ---- file systems: every local disk ----
	mounts := "/proc/mounts"
	if s, ok := readFile(mounts); ok {
		for _, m := range parseMounts(s) {
			var fs syscall.Statfs_t
			if syscall.Statfs(m.Point, &fs) != nil || fs.Blocks == 0 {
				continue
			}
			bs := float64(fs.Bsize)
			total, avail := float64(fs.Blocks)*bs, float64(fs.Bavail)*bs
			usedDF := float64(fs.Blocks-fs.Bfree) * bs
			a := map[string]string{"mountpoint": m.Point, "device": m.Device, "fstype": m.FSType}
			out = append(out, pt("system.filesystem.total", total, a), pt("system.filesystem.used", total-avail, a))
			if usedDF+avail > 0 {
				out = append(out, pt("system.filesystem.utilization", usedDF/(usedDF+avail), a))
			}
			if fs.Files > 0 {
				out = append(out, pt("system.filesystem.inodes.utilization", float64(fs.Files-fs.Ffree)/float64(fs.Files), a))
			}
		}
	}

	// ---- disk I/O ----
	if s, ok := readFile("/proc/diskstats"); ok {
		cur := parseDiskstats(s, wholeDisk)
		for dev, c := range cur {
			if p, ok := h.prevDisk[dev]; ok {
				if r, ok := diskRates(p, c, dt); ok {
					out = append(out,
						pt("system.disk.io.bytes_rate", r.readBps, map[string]string{"device": dev, "direction": "read"}), pt("system.disk.io.bytes_rate", r.writeBps, map[string]string{"device": dev, "direction": "write"}),
						pt("system.disk.io.ops_rate", r.readOps, map[string]string{"device": dev, "direction": "read"}), pt("system.disk.io.ops_rate", r.writeOps, map[string]string{"device": dev, "direction": "write"}),
						pt("system.disk.utilization", r.busy, map[string]string{"device": dev}))
				}
			}
		}
		h.prevDisk = cur
	}

	// ---- network ----
	if s, ok := readFile("/proc/net/dev"); ok {
		cur := parseNetDev(s)
		for dev, c := range cur {
			if p, ok := h.prevNet[dev]; ok {
				if r, ok := netRates(p, c, dt); ok {
					out = append(out,
						pt("system.network.io.bytes_rate", r.rxBps, map[string]string{"device": dev, "direction": "receive"}), pt("system.network.io.bytes_rate", r.txBps, map[string]string{"device": dev, "direction": "transmit"}),
						pt("system.network.packets_rate", r.rxPps, map[string]string{"device": dev, "direction": "receive"}), pt("system.network.packets_rate", r.txPps, map[string]string{"device": dev, "direction": "transmit"}),
						pt("system.network.errors_rate", r.errs, map[string]string{"device": dev, "kind": "errors"}), pt("system.network.errors_rate", r.drops, map[string]string{"device": dev, "kind": "drops"}))
				}
			}
		}
		h.prevNet = cur
	}
	h.prevAt = now
	return out
}
