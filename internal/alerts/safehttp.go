package alerts

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Guard stops the server from being used to reach things it should not. A channel's URL or mail server is typed by a
// user, and the server connects to it from inside the network, so by default it refuses loopback, private,
// link-local (cloud metadata) and similar addresses. The check is made on the address actually being connected to,
// after name resolution, so it also holds for redirects and for a name that resolves to an internal address.
//
// Link-local addresses (169.254.0.0/16, where cloud providers keep instance credentials) are refused even when
// AllowPrivate is set.
type Guard struct {
	AllowPrivate bool // allow loopback and private ranges, for example an internal mail relay
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func (g Guard) blocked(ip net.IP) error {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	switch {
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("link-local address %s is never allowed (cloud metadata lives there)", ip)
	case ip.IsUnspecified() || ip.IsMulticast():
		return fmt.Errorf("address %s is not allowed", ip)
	case ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip):
		if g.AllowPrivate {
			return nil
		}
		return fmt.Errorf("address %s is internal; internal destinations must be allowed by the administrator (LUMEN_ALERT_ALLOW_PRIVATE)", ip)
	}
	return nil
}

func (g Guard) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("not an IP address: %s", host)
	}
	return g.blocked(ip)
}

// Dial connects like net.Dialer, but only to addresses the guard allows.
func (g Guard) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: g.control}
	return d.DialContext(ctx, network, addr)
}

// Client is an HTTP client that uses the guard for every connection, follows at most three redirects, ignores
// proxy settings from the environment and never reuses connections.
func (g Guard) Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           g.Dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
				return fmt.Errorf("redirect to %s is not allowed", req.URL.Scheme)
			}
			return nil
		},
	}
}

// CheckURL does the checks that need no network: a http(s) URL with a host and without credentials in it.
func CheckURL(raw string, needHTTPS bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("the URL must look like https://example.com/path")
	}
	if u.Scheme != "https" && (needHTTPS || u.Scheme != "http") {
		if needHTTPS {
			return fmt.Errorf("the URL must start with https://")
		}
		return fmt.Errorf("the URL must start with http:// or https://")
	}
	if u.User != nil {
		return fmt.Errorf("do not put a user name or password in the URL")
	}
	if strings.ContainsAny(raw, "\r\n\x00 ") {
		return fmt.Errorf("the URL contains characters that are not allowed")
	}
	return nil
}

// do sends a request and returns the status and at most 64 KB of the answer.
func do(ctx context.Context, c *http.Client, method, rawURL string, headers map[string]string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, cleanErr(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

// cleanErr removes the URL from a transport error: URLs of webhooks carry secrets (Slack, Teams).
func cleanErr(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return fmt.Errorf("%s: %v", ue.Op, ue.Err)
	}
	return err
}
