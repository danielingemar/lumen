package agent

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Parsers for the Linux /proc files the host collector reads. They are plain functions on text so that they can be
// tested on any platform and with recorded samples.

// ---- CPU ----

type cpuTimes struct{ user, nice, system, idle, iowait, irq, softirq, steal uint64 }

// parseCPUTimes reads the aggregate "cpu" line of /proc/stat.
func parseCPUTimes(stat string) (cpuTimes, bool) {
	line := strings.SplitN(stat, "\n", 2)[0]
	f := strings.Fields(line)
	if len(f) < 8 || f[0] != "cpu" {
		return cpuTimes{}, false
	}
	v := make([]uint64, 8)
	for i := range v {
		v[i], _ = strconv.ParseUint(f[i+1], 10, 64)
	}
	return cpuTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}, true
}

func (c cpuTimes) total() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

// utilization is the share of time the CPU was busy between two samples (iowait counts as idle).
func cpuUtilization(prev, cur cpuTimes) (float64, bool) {
	dt := cur.total() - prev.total()
	if prev.total() == 0 || cur.total() <= prev.total() {
		return 0, false
	}
	idle := (cur.idle + cur.iowait) - (prev.idle + prev.iowait)
	return 1 - float64(idle)/float64(dt), true
}

// cpuShares splits the time between two samples into states, each as a fraction (they add up to 1).
func cpuShares(prev, cur cpuTimes) (map[string]float64, bool) {
	if prev.total() == 0 || cur.total() <= prev.total() {
		return nil, false
	}
	dt := float64(cur.total() - prev.total())
	d := func(a, b uint64) float64 {
		if a < b {
			return 0
		}
		return float64(a-b) / dt
	}
	return map[string]float64{
		"user": d(cur.user, prev.user), "nice": d(cur.nice, prev.nice), "system": d(cur.system, prev.system),
		"iowait": d(cur.iowait, prev.iowait), "irq": d(cur.irq+cur.softirq, prev.irq+prev.softirq),
		"steal": d(cur.steal, prev.steal), "idle": d(cur.idle, prev.idle),
	}, true
}

// ---- memory ----

// parseMeminfo returns /proc/meminfo in bytes.
func parseMeminfo(text string) map[string]float64 {
	m := map[string]float64{}
	for _, ln := range strings.Split(text, "\n") {
		if f := strings.Fields(ln); len(f) >= 2 {
			v, _ := strconv.ParseFloat(f[1], 64)
			m[strings.TrimSuffix(f[0], ":")] = v * 1024
		}
	}
	return m
}

// memoryStates splits memory into used / cached / buffers / free; the four add up to the total.
func memoryStates(m map[string]float64) map[string]float64 {
	total, free := m["MemTotal"], m["MemFree"]
	buffers, cached := m["Buffers"], m["Cached"]+m["SReclaimable"]
	used := total - free - buffers - cached
	if used < 0 { // some kernels count differently: never report negative memory
		used = 0
		cached = total - free - buffers
		if cached < 0 {
			cached = 0
		}
	}
	return map[string]float64{"used": used, "cached": cached, "buffers": buffers, "free": free}
}

// ---- disk I/O ----

type diskCounters struct{ readOps, readSectors, writeOps, writeSectors, ioMs uint64 }

// parseDiskstats reads /proc/diskstats for the devices accepted by whole (partitions and loop devices are left out,
// otherwise the same I/O would be counted twice).
func parseDiskstats(text string, whole func(string) bool) map[string]diskCounters {
	out := map[string]diskCounters{}
	for _, ln := range strings.Split(text, "\n") {
		f := strings.Fields(ln)
		if len(f) < 14 || !whole(f[2]) {
			continue
		}
		u := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		out[f[2]] = diskCounters{readOps: u(3), readSectors: u(5), writeOps: u(7), writeSectors: u(9), ioMs: u(12)}
	}
	return out
}

type diskRate struct{ readBps, writeBps, readOps, writeOps, busy float64 }

// diskRates turns two samples dt seconds apart into rates. A counter that went backwards (reset, wrap) gives no sample.
func diskRates(prev, cur diskCounters, dt float64) (diskRate, bool) {
	if dt <= 0 || cur.readOps < prev.readOps || cur.writeOps < prev.writeOps || cur.readSectors < prev.readSectors || cur.writeSectors < prev.writeSectors || cur.ioMs < prev.ioMs {
		return diskRate{}, false
	}
	busy := float64(cur.ioMs-prev.ioMs) / (dt * 1000)
	if busy > 1 {
		busy = 1
	}
	return diskRate{
		readBps: float64(cur.readSectors-prev.readSectors) * 512 / dt, writeBps: float64(cur.writeSectors-prev.writeSectors) * 512 / dt,
		readOps: float64(cur.readOps-prev.readOps) / dt, writeOps: float64(cur.writeOps-prev.writeOps) / dt, busy: busy,
	}, true
}

var wholeDiskRe = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+|md\d+|dm-\d+)$`)

// wholeDiskByName is the fallback when /sys/block cannot tell whole disks from partitions.
func wholeDiskByName(name string) bool { return wholeDiskRe.MatchString(name) }

// ---- network ----

type netCounters struct{ rxBytes, rxPackets, rxErrs, rxDrop, txBytes, txPackets, txErrs, txDrop uint64 }

// isVirtualNIC: container bridges, virtual ethernet pairs and similar carry traffic that is not the machine's own.
func isVirtualNIC(name string) bool {
	n := strings.ToLower(name)
	if n == "lo" {
		return true
	}
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// parseNetDev reads /proc/net/dev, leaving out loopback and virtual interfaces.
func parseNetDev(text string) map[string]netCounters {
	out := map[string]netCounters{}
	for _, ln := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(ln, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" || isVirtualNIC(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		u := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		out[name] = netCounters{rxBytes: u(0), rxPackets: u(1), rxErrs: u(2), rxDrop: u(3), txBytes: u(8), txPackets: u(9), txErrs: u(10), txDrop: u(11)}
	}
	return out
}

type netRate struct{ rxBps, txBps, rxPps, txPps, errs, drops float64 }

func netRates(prev, cur netCounters, dt float64) (netRate, bool) {
	if dt <= 0 || cur.rxBytes < prev.rxBytes || cur.txBytes < prev.txBytes || cur.rxPackets < prev.rxPackets || cur.txPackets < prev.txPackets {
		return netRate{}, false
	}
	sub := func(a, b uint64) float64 {
		if a < b {
			return 0
		}
		return float64(a - b)
	}
	return netRate{
		rxBps: float64(cur.rxBytes-prev.rxBytes) / dt, txBps: float64(cur.txBytes-prev.txBytes) / dt,
		rxPps: float64(cur.rxPackets-prev.rxPackets) / dt, txPps: float64(cur.txPackets-prev.txPackets) / dt,
		errs: (sub(cur.rxErrs, prev.rxErrs) + sub(cur.txErrs, prev.txErrs)) / dt, drops: (sub(cur.rxDrop, prev.rxDrop) + sub(cur.txDrop, prev.txDrop)) / dt,
	}, true
}

// ---- file systems ----

type mountInfo struct{ Device, Point, FSType string }

var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "vfat": true, "exfat": true,
	"ntfs": true, "ntfs3": true, "f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true}

var skipMountPrefixes = []string{"/var/lib/docker", "/var/lib/containers", "/var/lib/kubelet", "/run", "/snap", "/proc", "/sys", "/dev"}

func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// parseMounts lists local disks that are worth monitoring. Network file systems are left out on purpose: asking an
// unreachable NFS server for its size can block the agent. A device mounted several times (bind mounts, subvolumes) is
// listed once, under its shortest mount point.
func parseMounts(text string) []mountInfo {
	best := map[string]mountInfo{}
	for _, ln := range strings.Split(text, "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 || !realFS[f[2]] {
			continue
		}
		m := mountInfo{Device: unescapeMount(f[0]), Point: unescapeMount(f[1]), FSType: f[2]}
		skip := false
		for _, p := range skipMountPrefixes {
			if m.Point == p || strings.HasPrefix(m.Point, p+"/") {
				skip = true
			}
		}
		if skip {
			continue
		}
		if cur, ok := best[m.Device]; !ok || len(m.Point) < len(cur.Point) {
			best[m.Device] = m
		}
	}
	out := make([]mountInfo, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Point < out[j].Point })
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}

// ---- load, processes, uptime ----

type loadAvg struct {
	l1, l5, l15    float64
	running, procs float64
}

func parseLoadavg(text string) (loadAvg, bool) {
	f := strings.Fields(text)
	if len(f) < 4 {
		return loadAvg{}, false
	}
	var l loadAvg
	l.l1, _ = strconv.ParseFloat(f[0], 64)
	l.l5, _ = strconv.ParseFloat(f[1], 64)
	l.l15, _ = strconv.ParseFloat(f[2], 64)
	if r, t, ok := strings.Cut(f[3], "/"); ok {
		l.running, _ = strconv.ParseFloat(r, 64)
		l.procs, _ = strconv.ParseFloat(t, 64)
	}
	return l, true
}

func parseUptime(text string) (float64, bool) {
	f := strings.Fields(text)
	if len(f) < 1 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}
