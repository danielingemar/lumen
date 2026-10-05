package agent

import (
	"net"
	"reflect"
	"testing"
	"time"
)

func timeNow() time.Time { return time.Now() }

func ips(s ...string) []net.IP {
	var o []net.IP
	for _, x := range s {
		o = append(o, net.ParseIP(x))
	}
	return o
}

const up = net.FlagUp

func TestUsableIPs(t *testing.T) {
	ifs := []nic{
		{"lo", up | net.FlagLoopback, ips("127.0.0.1", "::1")},
		{"eth0", up, ips("192.168.20.131", "fe80::1", "2001:db8::5")},
		{"eth1", up, ips("10.0.0.7", "169.254.3.3")},
		{"docker0", up, ips("172.17.0.1")},
		{"br-3f2a9c", up, ips("172.18.0.1")},
		{"veth12ab", up, ips("172.19.0.9")},
		{"virbr0", up, ips("192.168.122.1")},
		{"wg0", 0, ips("10.9.0.2")},           // down
		{"eth0:1", up, ips("192.168.20.131")}, // duplicate of eth0
	}
	got := usableIPs(ifs, "")
	if !reflect.DeepEqual(got, []string{"192.168.20.131", "10.0.0.7"}) {
		t.Fatalf("machine addresses only: no loopback, link-local, IPv6 (IPv4 exists), container bridges, down interfaces or duplicates: %v", got)
	}
	// the address used to reach Lumen is listed first
	if got := usableIPs(ifs, "10.0.0.7"); !reflect.DeepEqual(got, []string{"10.0.0.7", "192.168.20.131"}) {
		t.Fatalf("primary first: %v", got)
	}
	// IPv6-only machines fall back to the global IPv6 address
	if got := usableIPs([]nic{{"eth0", up, ips("fe80::1", "2001:db8::5")}}, ""); !reflect.DeepEqual(got, []string{"2001:db8::5"}) {
		t.Fatalf("ipv6 fallback: %v", got)
	}
	if got := usableIPs([]nic{{"lo", up | net.FlagLoopback, ips("127.0.0.1")}, {"docker0", up, ips("172.17.0.1")}}, ""); len(got) != 0 {
		t.Fatalf("a machine with only loopback and docker has no address to show: %v", got)
	}
	many := []nic{{"eth0", up, ips("10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6", "10.0.0.7", "10.0.0.8")}}
	if got := usableIPs(many, ""); len(got) != 6 {
		t.Fatalf("at most 6 addresses: %v", got)
	}
}

func TestOutboundIP(t *testing.T) {
	if outboundIP("") != "" || outboundIP("::not a url::") != "" || outboundIP("ftp://x") != "" {
		t.Fatal("garbage gives no address")
	}
	// Lumen on the same machine (loopback): the route says 127.0.0.1, which identifies nothing, so it is ignored
	if got := outboundIP("http://127.0.0.1:4318"); got != "" {
		t.Fatalf("loopback must not be reported as the machine's address: %q", got)
	}
	// LocalIPs never reports loopback and never panics without a network
	p, all := LocalIPs("http://127.0.0.1:4318")
	for _, a := range append([]string{p}, all...) {
		if ip := net.ParseIP(a); a != "" && (ip == nil || ip.IsLoopback()) {
			t.Fatalf("bad address %q", a)
		}
	}
}

func TestInfoMetricCarriesAddresses(t *testing.T) {
	s := NewSelf("web1")
	s.ip, s.ips, s.ipAt = "192.168.20.131", []string{"192.168.20.131", "10.0.0.7"}, timeNow()
	var info *Point
	for _, p := range s.Collect("lumen-agent", 1) {
		if p.Name == "lumen_agent_info" {
			p := p
			info = &p
		}
	}
	if info == nil || info.Attrs["ip"] != "192.168.20.131" || info.Attrs["ips"] != "192.168.20.131,10.0.0.7" || info.Attrs["host"] != "web1" {
		t.Fatalf("info metric: %+v", info)
	}
	s2 := NewSelf("noip")
	s2.ipAt = timeNow() // no address found: the labels are left out rather than sent empty
	for _, p := range s2.Collect("lumen-agent", 1) {
		if p.Name == "lumen_agent_info" {
			if _, ok := p.Attrs["ip"]; ok {
				t.Fatal("no empty ip label")
			}
		}
	}
}
