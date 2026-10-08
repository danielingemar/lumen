package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Checks that go deeper than status.php and serverinfo:
//
//	background jobs   when Nextcloud's cron last ran, from the provisioning API's app configuration (core/lastcron: the same
//	                  value Nextcloud's own admin overview checks, and complains about after an hour). Needs an administrator
//	                  user with an app password: a serverinfo token cannot read it.
//	TLS certificate   how long the certificate of an https instance is valid, and whether it is valid for the name at all.
//	WebDAV login      whether a user can log in and list their files: it touches the web server, PHP, the database and the
//	                  storage, so it finds failures that status.php does not.
//
// Each has its own interval and is cached, so that a collection every 15 seconds does not ask an instance or open a TLS
// connection every time.

const (
	cronEvery = 60 * time.Second
	tlsEvery  = 10 * time.Minute
	davEvery  = 5 * time.Minute

	// usageEvery: the storage used is for invoicing, not for alerting, and reading it asks for every account, so it is
	// read rarely. maxUsagePages bounds the work on a very large instance (500 accounts per page).
	usageEvery    = 15 * time.Minute
	maxUsagePages = 200
)

// usageInfo is what the accounts of an instance use together.
type usageInfo struct {
	usedBytes         float64
	enabled, disabled int
	partial           bool // the page limit was reached: the numbers are a lower bound
}

type cronInfo struct {
	lastRun int64
	mode    string
	errs    int
}

type tlsInfo struct {
	notAfter time.Time
	valid    bool
}

type davInfo struct {
	ok   bool
	took time.Duration
}

type deepState struct {
	cronAt, tlsAt, davAt time.Time
	cron                 cronInfo
	cronErr              error
	tls                  tlsInfo
	tlsErr               error
	dav                  davInfo
	davErr               error
	usageAt              time.Time
	usage                usageInfo
	usageErr             error
}

var deep = struct {
	mu sync.Mutex
	m  map[string]*deepState
}{m: map[string]*deepState{}}

// tlsRoots is the set of trusted roots for the certificate check; nil means the system's (tests set it).
var tlsRoots *x509.CertPool

// resetDeepChecks forgets what was cached (tests).
func resetDeepChecks() {
	deep.mu.Lock()
	deep.m = map[string]*deepState{}
	deep.mu.Unlock()
}

// provValue reads one app-configuration value through the provisioning API.
func provValue(ctx context.Context, base, user, pass, app, key string) (string, error) {
	body, code, _, err := ncGet(ctx, base+"/ocs/v2.php/apps/provisioning_api/api/v1/config/apps/"+url.PathEscape(app)+"/"+url.PathEscape(key)+"?format=json", func(r *http.Request) {
		r.Header.Set("OCS-APIRequest", "true")
		r.SetBasicAuth(user, pass)
	})
	if err != nil {
		return "", err
	}
	switch {
	case code == 401:
		return "", fmt.Errorf("the user name or app password was refused (HTTP 401)")
	case code == 403:
		return "", fmt.Errorf("the user must be a Nextcloud administrator to read background-job status (HTTP 403)")
	case code == 404 || code == 405:
		return "", fmt.Errorf("the Provisioning API is not available (HTTP %d): enable the Provisioning API app", code)
	case code != 200:
		return "", fmt.Errorf("the provisioning API returned HTTP %d", code)
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", fmt.Errorf("the provisioning API did not return JSON")
	}
	return str(m, "ocs", "data", "data"), nil
}

// errNoAccountList: this Nextcloud has no account list. It is not an error worth showing on the instance (the storage used
// is simply not reported, and the invoicing view says so).
var errNoAccountList = fmt.Errorf("this Nextcloud does not offer the account list (needs the Provisioning API), so the storage used is not reported")

// fetchUsage adds up what the accounts of an instance use, from the provisioning API's account list (one request per 500
// accounts). It is the number a hosting provider invoices on: the files in the users' own storage, as Nextcloud counts them
// against their quota. It needs an administrator.
func fetchUsage(ctx context.Context, base, user, pass string) (usageInfo, error) {
	var u usageInfo
	const page = 500
	for i := 0; i < maxUsagePages; i++ {
		body, code, _, err := ncGet(ctx, base+"/ocs/v2.php/cloud/users/details?format=json&limit="+strconv.Itoa(page)+"&offset="+strconv.Itoa(i*page), func(r *http.Request) {
			r.Header.Set("OCS-APIRequest", "true")
			r.SetBasicAuth(user, pass)
		})
		if err != nil {
			return u, err
		}
		switch {
		case code == 401:
			return u, fmt.Errorf("the user name or app password was refused (HTTP 401)")
		case code == 403:
			return u, fmt.Errorf("the user must be a Nextcloud administrator to read the storage used (HTTP 403)")
		case code == 404 || code == 405:
			return u, errNoAccountList
		case code != 200:
			return u, fmt.Errorf("the account list returned HTTP %d", code)
		}
		var m struct {
			OCS struct {
				Data struct {
					Users map[string]struct {
						Enabled any `json:"enabled"`
						Quota   struct {
							Used any `json:"used"`
						} `json:"quota"`
					} `json:"users"`
				} `json:"data"`
			} `json:"ocs"`
		}
		if json.Unmarshal(body, &m) != nil {
			return u, fmt.Errorf("the account list was not JSON")
		}
		for _, a := range m.OCS.Data.Users {
			if b, ok := a.Enabled.(bool); ok && !b || a.Enabled == "0" || a.Enabled == float64(0) {
				u.disabled++
			} else {
				u.enabled++
			}
			switch v := a.Quota.Used.(type) {
			case float64:
				u.usedBytes += v
			case string:
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					u.usedBytes += n
				}
			}
		}
		if len(m.OCS.Data.Users) < page {
			return u, nil
		}
	}
	u.partial = true
	return u, nil
}

func fetchCron(ctx context.Context, base, user, pass string) (cronInfo, error) {
	var c cronInfo
	v, err := provValue(ctx, base, user, pass, "core", "lastcron")
	if err != nil {
		return c, err
	}
	if v != "" {
		n, perr := strconv.ParseFloat(v, 64)
		if perr != nil {
			return c, fmt.Errorf("core/lastcron is not a number: %q", v)
		}
		c.lastRun = int64(n)
	}
	// the rest is context: failing to read it does not hide the last run
	if m, err := provValue(ctx, base, user, pass, "core", "backgroundjobs_mode"); err == nil {
		c.mode = m
	}
	if c.mode == "" {
		c.mode = "ajax" // Nextcloud's default when nothing is set
	}
	if e, err := provValue(ctx, base, user, pass, "core", "cronErrors"); err == nil && strings.TrimSpace(e) != "" {
		var list []any
		if json.Unmarshal([]byte(e), &list) == nil {
			c.errs = len(list)
		}
	}
	return c, nil
}

// fetchTLS reads the certificate of an https address. The name is checked against the certificate and the chain against
// the system's roots, but a certificate that fails that is still read, so that its date can be reported.
func fetchTLS(ctx context.Context, u *url.URL) (tlsInfo, error) {
	var t tlsInfo
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 8 * time.Second}, Config: &tls.Config{ServerName: host, InsecureSkipVerify: true}} //nolint:gosec // only to read the certificate; it is verified below
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, err := d.DialContext(cctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return t, fmt.Errorf("cannot read the TLS certificate: %w", err)
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return t, fmt.Errorf("the server presented no certificate")
	}
	leaf := certs[0]
	t.notAfter = leaf.NotAfter
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	opts := x509.VerifyOptions{Intermediates: inter, Roots: tlsRoots}
	if net.ParseIP(host) == nil {
		opts.DNSName = host
	}
	_, verr := leaf.Verify(opts)
	t.valid = verr == nil
	return t, nil
}

// fetchDAV logs in with the user and asks for the top of their files.
func fetchDAV(ctx context.Context, base, user, pass string) (davInfo, error) {
	var d davInfo
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", base+"/remote.php/dav/files/"+url.PathEscape(user)+"/", strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getlastmodified/></d:prop></d:propfind>`))
	if err != nil {
		return d, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	req.SetBasicAuth(user, pass)
	start := time.Now()
	resp, err := ncClient.Do(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	d.took = time.Since(start)
	switch {
	case resp.StatusCode == 207:
		d.ok = true
	case resp.StatusCode == 401:
		return d, fmt.Errorf("the WebDAV login was refused (HTTP 401): check the user name and app password")
	default:
		return d, fmt.Errorf("the WebDAV check returned HTTP %d", resp.StatusCode)
	}
	return d, nil
}

// collectDeep runs the checks that are due and returns their points. up says whether status.php answered: the checks that
// need Nextcloud to be running stay quiet when it is not (that is already reported), but the certificate is always read,
// since an expired one is a common reason for an instance to be down.
func collectDeep(ctx context.Context, t NextcloudTarget, nowNs int64, up bool) ([]Point, error) {
	base := strings.TrimRight(t.URL, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, nil
	}
	svc := t.Service
	if svc == "" {
		svc = "nextcloud"
	}
	inst := func(extra map[string]string) map[string]string {
		m := map[string]string{"instance": u.Host}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	pt := func(name string, v float64, a map[string]string) Point {
		return Point{Service: svc, Name: name, Type: "gauge", Value: v, Attrs: a, TimeNs: nowNs}
	}
	now := time.Unix(0, nowNs)
	if nowNs < 1e15 { // a caller that gave seconds
		now = time.Unix(nowNs, 0)
	}
	wall := time.Now()
	deep.mu.Lock()
	st := deep.m[base]
	if st == nil {
		st = &deepState{}
		deep.m[base] = st
	}
	deep.mu.Unlock()

	var pts []Point
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	admin := t.Username != "" && t.Password != ""

	if admin && up {
		if wall.Sub(st.cronAt) >= cronEvery {
			st.cron, st.cronErr = fetchCron(ctx, base, t.Username, t.Password)
			st.cronAt = wall
		}
		if st.cronErr == nil {
			pts = append(pts,
				pt("nextcloud_cron_last_run_timestamp_seconds", float64(st.cron.lastRun), inst(nil)),
				pt("nextcloud_cron_age_seconds", float64(now.Unix()-st.cron.lastRun), inst(nil)), // never run: a very large age, which is what an alert should see
				pt("nextcloud_cron_errors", float64(st.cron.errs), inst(nil)),
				pt("nextcloud_cron_mode", 1, inst(map[string]string{"mode": st.cron.mode})))
		}
		note(st.cronErr)
		if wall.Sub(st.davAt) >= davEvery {
			st.dav, st.davErr = fetchDAV(ctx, base, t.Username, t.Password)
			st.davAt = wall
		}
		if st.davErr == nil || st.dav.took > 0 {
			ok := 0.0
			if st.dav.ok {
				ok = 1
			}
			pts = append(pts, pt("nextcloud_webdav_ok", ok, inst(nil)))
			if st.dav.ok {
				pts = append(pts, pt("nextcloud_webdav_response_seconds", st.dav.took.Seconds(), inst(nil)))
			}
		}
		note(st.davErr)
		if wall.Sub(st.usageAt) >= usageEvery {
			st.usage, st.usageErr = fetchUsage(ctx, base, t.Username, t.Password)
			st.usageAt = wall
		}
		if st.usageErr == nil && !st.usage.partial { // a lower bound would under-bill, so it is not reported
			pts = append(pts,
				pt("nextcloud_storage_used_bytes", st.usage.usedBytes, inst(nil)),
				pt("nextcloud_accounts", float64(st.usage.enabled), inst(map[string]string{"state": "enabled"})),
				pt("nextcloud_accounts", float64(st.usage.disabled), inst(map[string]string{"state": "disabled"})))
		}
		if st.usageErr != errNoAccountList {
			note(st.usageErr)
		}
	}
	if u.Scheme == "https" {
		if wall.Sub(st.tlsAt) >= tlsEvery {
			st.tls, st.tlsErr = fetchTLS(ctx, u)
			st.tlsAt = wall
		}
		if st.tlsErr == nil {
			valid := 0.0
			if st.tls.valid {
				valid = 1
			}
			pts = append(pts,
				pt("nextcloud_tls_cert_not_after_timestamp_seconds", float64(st.tls.notAfter.Unix()), inst(nil)),
				pt("nextcloud_tls_cert_expiry_seconds", st.tls.notAfter.Sub(now).Seconds(), inst(nil)), // negative once it has expired
				pt("nextcloud_tls_cert_valid", valid, inst(nil)))
		}
		note(st.tlsErr)
	}
	return pts, firstErr
}

// boolAt reads a true/false value.
func boolAt(m map[string]any, path ...string) (bool, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return false, false
		}
		if cur, ok = mm[p]; !ok {
			return false, false
		}
	}
	b, ok := cur.(bool)
	return b, ok
}
