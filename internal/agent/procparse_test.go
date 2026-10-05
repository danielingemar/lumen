package agent

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCPUSharesAddUpAndMatchUtilization(t *testing.T) {
	p, ok1 := parseCPUTimes("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4 5 6 7 8 9 10\n")
	c, ok2 := parseCPUTimes("cpu  200 10 100 1600 100 10 10 20 0 0\n")
	if !ok1 || !ok2 {
		t.Fatal("parse")
	}
	sh, ok := cpuShares(p, c)
	if !ok {
		t.Fatal("shares")
	}
	// delta: user 100, nice 10, system 50, idle 800, iowait 50, irq 10+softirq 10, steal 20 = 1050
	want := map[string]float64{"user": 100.0 / 1050, "nice": 10.0 / 1050, "system": 50.0 / 1050, "iowait": 50.0 / 1050, "irq": 20.0 / 1050, "steal": 20.0 / 1050, "idle": 800.0 / 1050}
	sum := 0.0
	for k, v := range want {
		if !near(sh[k], v) {
			t.Errorf("%s: want %v got %v", k, v, sh[k])
		}
		sum += sh[k]
	}
	if !near(sum, 1) {
		t.Fatalf("the states of one interval add up to 1, got %v", sum)
	}
	u, _ := cpuUtilization(p, c)
	if !near(u, 1-(800.0+50)/1050) {
		t.Fatalf("utilization keeps its old meaning (iowait counts as idle): %v", u)
	}
	if _, ok := cpuShares(cpuTimes{}, c); ok {
		t.Fatal("no previous sample: no rate")
	}
	if _, ok := cpuShares(c, p); ok {
		t.Fatal("a counter that went backwards (reboot) gives no sample")
	}
	if _, ok := parseCPUTimes("cpu0 1 2 3"); ok {
		t.Fatal("garbage")
	}
}

const meminfo = `MemTotal:       16384000 kB
MemFree:         2000000 kB
MemAvailable:    9000000 kB
Buffers:          500000 kB
Cached:          6000000 kB
SReclaimable:     400000 kB
SwapTotal:       2097152 kB
SwapFree:        2000000 kB
`

func TestMemoryStatesAddUpToTotal(t *testing.T) {
	m := parseMeminfo(meminfo)
	st := memoryStates(m)
	total := 16384000.0 * 1024
	if !near(st["used"]+st["cached"]+st["buffers"]+st["free"], total) {
		t.Fatalf("used+cached+buffers+free must equal total (so the stacked chart is complete): %v", st)
	}
	if !near(st["free"], 2000000*1024) || !near(st["buffers"], 500000*1024) || !near(st["cached"], 6400000*1024) || !near(st["used"], (16384000-2000000-500000-6400000)*1024) {
		t.Fatalf("%v", st)
	}
	// a kernel that reports odd numbers must never give negative memory
	odd := memoryStates(map[string]float64{"MemTotal": 1000, "MemFree": 100, "Buffers": 100, "Cached": 900})
	for k, v := range odd {
		if v < 0 {
			t.Fatalf("%s negative: %v", k, odd)
		}
	}
}

const diskstats = `   8       0 sda 1000 10 20000 500 2000 20 40000 900 0 1500 1400 0 0 0 0
   8       1 sda1 900 10 18000 400 1900 20 38000 800 0 1400 1200 0 0 0 0
   7       0 loop0 50 0 100 5 0 0 0 0 0 10 5
 259       0 nvme0n1 5000 0 80000 300 6000 0 96000 700 0 900 1000 0 0 0 0 0 0
 259       1 nvme0n1p1 4900 0 79000 300 5900 0 95000 700 0 900 1000 0 0 0 0 0 0
 253       0 dm-0 800 0 16000 100 1500 0 30000 300 0 400 400 0 0 0 0
  11       0 sr0 0 0 0 0 0 0 0 0 0 0 0
`

func TestParseDiskstatsWholeDisksOnly(t *testing.T) {
	got := parseDiskstats(diskstats, wholeDiskByName)
	var names []string
	for n := range got {
		names = append(names, n)
	}
	if len(got) != 3 || got["sda"].readSectors != 20000 || got["nvme0n1"].writeSectors != 96000 || got["dm-0"].ioMs != 400 {
		t.Fatalf("whole disks and LVM only, never partitions, loop or optical devices: %v", names)
	}
	for _, bad := range []string{"sda1", "nvme0n1p1", "loop0", "sr0"} {
		if _, ok := got[bad]; ok {
			t.Errorf("%s must be left out: it would double count the disk's I/O", bad)
		}
	}
}

func TestDiskRates(t *testing.T) {
	p := diskCounters{readOps: 1000, readSectors: 20000, writeOps: 2000, writeSectors: 40000, ioMs: 1500}
	c := diskCounters{readOps: 1100, readSectors: 22000, writeOps: 2300, writeSectors: 46000, ioMs: 2500}
	r, ok := diskRates(p, c, 10)
	if !ok || !near(r.readBps, 2000*512/10.0) || !near(r.writeBps, 6000*512/10.0) || !near(r.readOps, 10) || !near(r.writeOps, 30) || !near(r.busy, 0.1) {
		t.Fatalf("%+v %v", r, ok)
	}
	// busy time cannot exceed 100% even if the clock and the counters disagree a little
	if r, _ := diskRates(p, diskCounters{readOps: 1000, readSectors: 20000, writeOps: 2000, writeSectors: 40000, ioMs: 99999}, 1); r.busy != 1 {
		t.Fatalf("busy must be capped at 1: %v", r.busy)
	}
	if _, ok := diskRates(c, p, 10); ok {
		t.Fatal("counters that went backwards give no sample")
	}
	if _, ok := diskRates(p, c, 0); ok {
		t.Fatal("no elapsed time, no rate")
	}
}

const netdev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999   9999    0    0    0     0          0         0  9999999    9999    0    0    0     0       0          0
  eth0: 1000000   2000    1    2    0     0          0         0  500000    1500    3    4    0     0       0          0
docker0: 123456    100    0    0    0     0          0         0  654321     200    0    0    0     0       0          0
veth1a2b: 5555      50    0    0    0     0          0         0    4444      40    0    0    0     0       0          0
br-3f2a9c: 777       7    0    0    0     0          0         0     888       8    0    0    0     0       0          0
 ens192: 42 1 0 0 0 0 0 0 43 2 0 0 0 0 0 0
`

func TestNetDevAndRates(t *testing.T) {
	n := parseNetDev(netdev)
	if len(n) != 2 || n["eth0"].rxBytes != 1000000 || n["eth0"].txErrs != 3 || n["ens192"].txBytes != 43 {
		t.Fatalf("physical interfaces only: %v", n)
	}
	cur := netCounters{rxBytes: 3000000, rxPackets: 4000, rxErrs: 2, rxDrop: 3, txBytes: 1500000, txPackets: 2500, txErrs: 4, txDrop: 6}
	r, ok := netRates(n["eth0"], cur, 10)
	if !ok || !near(r.rxBps, 200000) || !near(r.txBps, 100000) || !near(r.rxPps, 200) || !near(r.txPps, 100) || !near(r.errs, (1+1)/10.0) || !near(r.drops, (1+2)/10.0) {
		t.Fatalf("%+v", r)
	}
	if _, ok := netRates(cur, n["eth0"], 10); ok {
		t.Fatal("counter reset")
	}
}

const mounts = `sysfs /sys sysfs rw,nosuid 0 0
proc /proc proc rw 0 0
/dev/sda2 / ext4 rw,relatime 0 0
/dev/sda1 /boot/efi vfat rw 0 0
tmpfs /run tmpfs rw 0 0
overlay /var/lib/docker/overlay2/abc/merged overlay rw 0 0
/dev/mapper/vg-data /data xfs rw 0 0
/dev/mapper/vg-data /srv/bind xfs rw 0 0
/dev/sdb1 /mnt/my\040disk ext4 rw 0 0
nfs.example:/export /mnt/nfs nfs4 rw 0 0
/dev/loop3 /snap/core/1 squashfs ro 0 0
/dev/sdc1 /var/lib/docker/volumes ext4 rw 0 0
`

func TestParseMounts(t *testing.T) {
	got := parseMounts(mounts)
	var pts []string
	for _, m := range got {
		pts = append(pts, m.Point)
	}
	if !reflect.DeepEqual(pts, []string{"/", "/boot/efi", "/data", "/mnt/my disk"}) {
		t.Fatalf("local disks only: no pseudo, overlay, snap, container or network file systems; one entry per device; spaces unescaped: %v", pts)
	}
	for _, m := range got {
		if m.Point == "/data" && (m.Device != "/dev/mapper/vg-data" || m.FSType != "xfs") {
			t.Fatalf("%+v", m)
		}
	}
	many := strings.Repeat("/dev/x1 /m1 ext4 rw 0 0\n", 1)
	for i := 0; i < 30; i++ {
		many += "/dev/d" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + " /mnt/d" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + " ext4 rw 0 0\n"
	}
	if len(parseMounts(many)) != 16 {
		t.Fatal("at most 16 file systems")
	}
}

func TestLoadAndUptime(t *testing.T) {
	l, ok := parseLoadavg("0.52 0.58 0.59 3/1234 56789\n")
	if !ok || l.l1 != 0.52 || l.l5 != 0.58 || l.l15 != 0.59 || l.running != 3 || l.procs != 1234 {
		t.Fatalf("%+v", l)
	}
	if _, ok := parseLoadavg("x"); ok {
		t.Fatal("garbage")
	}
	if u, ok := parseUptime("350735.47 234388.90\n"); !ok || u != 350735.47 {
		t.Fatal(u, ok)
	}
	if _, ok := parseUptime(""); ok {
		t.Fatal("empty")
	}
}
