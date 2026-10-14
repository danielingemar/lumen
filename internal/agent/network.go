package agent

import (
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

// nic is a network interface reduced to what the choice of addresses needs (so it can be tested without a network).
type nic struct {
	Name  string
	Flags net.Flags
	Addrs []net.IP
}

// virtualPrefixes are interfaces that carry container or hypervisor traffic, not the address of the machine itself.
var virtualPrefixes = []string{"docker", "br-", "veth", "cni", "flannel", "cali", "virbr", "vxlan", "tun", "tap", "kube", "podman", "lxc", "weave", "vmnet", "vboxnet", "utun", "awdl", "llw"}

// usableIPs lists the addresses that identify this machine: up, not loopback, not link-local, not on a virtual
// interface. IPv4 only, unless there is none (then a global IPv6 address). The primary address comes first.
func usableIPs(ifs []nic, primary string) []string {
	var v4, v6 []string
	seen := map[string]bool{}
	for _, n := range ifs {
		if n.Flags&net.FlagUp == 0 || n.Flags&net.FlagLoopback != 0 {
			continue
		}
		virtual := false
		for _, p := range virtualPrefixes {
			if strings.HasPrefix(strings.ToLower(n.Name), p) {
				virtual = true
			}
		}
		if virtual {
			continue
		}
		for _, ip := range n.Addrs {
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			s := ip.String()
			if seen[s] {
				continue
			}
			seen[s] = true
			if ip.To4() != nil {
				v4 = append(v4, s)
			} else if ip.IsGlobalUnicast() {
				v6 = append(v6, s)
			}
		}
	}
	out := v4
	if len(out) == 0 {
		out = v6
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i] == primary && out[j] != primary })
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// outboundIP is the local address the operating system would use to reach the Lumen server. Nothing is sent:
// connecting a UDP socket only makes the system pick a route.
func outboundIP(lumenURL string) string {
	u, err := url.Parse(lumenURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	if port == "" {
		return ""
	}
	c, err := net.DialTimeout("udp", net.JoinHostPort(u.Hostname(), port), 2*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsLoopback() && !a.IP.IsUnspecified() {
		return a.IP.String()
	}
	return ""
}

func systemNICs() []nic {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []nic
	for _, i := range ifs {
		as, _ := i.Addrs()
		n := nic{Name: i.Name, Flags: i.Flags}
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok {
				n.Addrs = append(n.Addrs, ipn.IP)
			}
		}
		out = append(out, n)
	}
	return out
}

// LocalIPs returns the address used to reach Lumen (the one to show first) and this machine's other usable addresses.
func LocalIPs(lumenURL string) (primary string, all []string) {
	primary = outboundIP(lumenURL)
	all = usableIPs(systemNICs(), primary)
	if primary == "" && len(all) > 0 {
		primary = all[0]
	}
	return
}
