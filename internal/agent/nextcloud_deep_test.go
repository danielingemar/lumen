package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeNC struct {
	mu        sync.Mutex
	hits      map[string]int
	lastcron  string
	mode      string
	errsJSON  string
	provCode  int
	davCode   int
	update    string // JSON for system.update, or ""
	statusErr bool
}

func (f *fakeNC) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits[r.URL.Path]++
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/status.php":
			if f.statusErr {
				http.Error(w, "boom", 500)
				return
			}
			w.Write([]byte(`{"installed":true,"maintenance":false,"needsDbUpgrade":false,"version":"34.0.1.2","versionstring":"34.0.1"}`))
		case strings.HasPrefix(r.URL.Path, "/ocs/v2.php/apps/provisioning_api/api/v1/config/apps/core/"):
			if f.provCode != 0 {
				w.WriteHeader(f.provCode)
				return
			}
			val := map[string]string{"lastcron": f.lastcron, "backgroundjobs_mode": f.mode, "cronErrors": f.errsJSON}[strings.TrimPrefix(r.URL.Path, "/ocs/v2.php/apps/provisioning_api/api/v1/config/apps/core/")]
			fmt.Fprintf(w, `{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{"data":%q}}}`, val)
		case strings.HasPrefix(r.URL.Path, "/remote.php/dav/files/"):
			if f.davCode != 0 {
				w.WriteHeader(f.davCode)
				return
			}
			w.WriteHeader(207)
			w.Write([]byte(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`))
		case r.URL.Path == "/ocs/v2.php/apps/serverinfo/api/v1/info":
			upd := ""
			if f.update != "" {
				upd = `,"update":` + f.update
			}
			fmt.Fprintf(w, `{"ocs":{"meta":{"status":"ok","statuscode":200},"data":{"nextcloud":{"system":{"version":"34.0.1.2","freespace":5000,"apps":{"num_installed":3,"num_updates_available":1}%s},"storage":{"num_users":2}},"server":{"php":{"version":"8.3"},"database":{"type":"pgsql","version":"17"}}}}}`, upd)
		default:
			http.NotFound(w, r)
		}
	})
}

func newFake(t *testing.T) (*fakeNC, *httptest.Server) {
	resetDeepChecks()
	f := &fakeNC{hits: map[string]int{}, mode: "cron", errsJSON: "[]"}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	return f, ts
}

func byName(pts []Point) map[string]Point {
	m := map[string]Point{}
	for _, p := range pts {
		m[p.Name] = p
	}
	return m
}

func TestBackgroundJobsAreReadWithAnAdminLogin(t *testing.T) {
	f, ts := newFake(t)
	nowNs := time.Now().UnixNano()
	f.lastcron = fmt.Sprint(time.Now().Add(-5 * time.Minute).Unix())
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Service: "ks", Username: "monitor", Password: "app-pass"}, nowNs)
	m := byName(pts)
	if err != nil && !strings.Contains(err.Error(), "serverinfo") { // serverinfo answers for an admin too; no error is expected from the deep checks
		t.Fatalf("%v", err)
	}
	age := m["nextcloud_cron_age_seconds"].Value
	if age < 295 || age > 305 || m["nextcloud_cron_last_run_timestamp_seconds"].Value == 0 || m["nextcloud_cron_errors"].Value != 0 {
		t.Fatalf("the last run was 5 minutes ago: %+v", m["nextcloud_cron_age_seconds"])
	}
	if p := m["nextcloud_cron_mode"]; p.Attrs["mode"] != "cron" || p.Attrs["instance"] == "" || p.Service != "ks" {
		t.Fatalf("%+v", p)
	}
	// errors that Nextcloud recorded for cron, and the default mode when none is set
	resetDeepChecks()
	f.errsJSON, f.mode = `[{"e":1},{"e":2}]`, ""
	pts, _ = CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "app-pass"}, nowNs)
	m = byName(pts)
	if m["nextcloud_cron_errors"].Value != 2 || m["nextcloud_cron_mode"].Attrs["mode"] != "ajax" {
		t.Fatalf("%+v %+v", m["nextcloud_cron_errors"], m["nextcloud_cron_mode"])
	}
	// cron has never run: a very large age, so that "older than an hour" is true
	resetDeepChecks()
	f.lastcron = ""
	pts, _ = CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "app-pass"}, nowNs)
	m = byName(pts)
	if m["nextcloud_cron_last_run_timestamp_seconds"].Value != 0 || m["nextcloud_cron_age_seconds"].Value < 3600*24*365 {
		t.Fatalf("never run: %+v", m["nextcloud_cron_age_seconds"])
	}
}

func TestBackgroundJobsNeedAnAdminAndSayWhy(t *testing.T) {
	f, ts := newFake(t)
	nowNs := time.Now().UnixNano()
	// a serverinfo token cannot read them, and that is not an error: it is just not available
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Token: "tok"}, nowNs)
	if err != nil || byName(pts)["nextcloud_cron_age_seconds"].Name != "" || byName(pts)["nextcloud_webdav_ok"].Name != "" {
		t.Fatalf("token only: %v %v", err, len(pts))
	}
	for code, want := range map[int]string{403: "must be a Nextcloud administrator", 401: "refused", 404: "enable the Provisioning API"} {
		resetDeepChecks()
		f.provCode = code
		pts, err = CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "u", Password: "p"}, nowNs)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("HTTP %d must say %q: %v", code, want, err)
		}
		if byName(pts)["nextcloud_cron_age_seconds"].Name != "" || byName(pts)["nextcloud_up"].Value != 1 {
			t.Errorf("HTTP %d: no cron points, but the instance is still reported up", code)
		}
	}
	// an instance that is down: nothing deeper is attempted, and the cron paths are never asked
	resetDeepChecks()
	f.provCode, f.statusErr = 0, true
	before := f.hits["/ocs/v2.php/apps/provisioning_api/api/v1/config/apps/core/lastcron"]
	pts, _ = CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "u", Password: "p"}, nowNs)
	if len(pts) != 1 || pts[0].Name != "nextcloud_up" || pts[0].Value != 0 || f.hits["/ocs/v2.php/apps/provisioning_api/api/v1/config/apps/core/lastcron"] != before {
		t.Fatalf("%+v", pts)
	}
}

func TestSlowChecksAreCached(t *testing.T) {
	f, ts := newFake(t)
	f.lastcron = fmt.Sprint(time.Now().Unix())
	tgt := NextcloudTarget{URL: ts.URL, Username: "u", Password: "p"}
	for i := 0; i < 5; i++ { // a collection every 15 seconds
		CollectNextcloud(context.Background(), tgt, time.Now().UnixNano())
	}
	if n := f.hits["/ocs/v2.php/apps/provisioning_api/api/v1/config/apps/core/lastcron"]; n != 1 {
		t.Fatalf("the cron status is asked for once a minute, not at every collection: %d", n)
	}
	var dav int
	for p, n := range f.hits {
		if strings.HasPrefix(p, "/remote.php/dav/") {
			dav += n
		}
	}
	if dav != 1 {
		t.Fatalf("the WebDAV check runs every five minutes: %d", dav)
	}
	// ... and it is asked again when the time is up
	deep.mu.Lock()
	for _, st := range deep.m {
		st.cronAt = time.Now().Add(-2 * time.Minute)
	}
	deep.mu.Unlock()
	CollectNextcloud(context.Background(), tgt, time.Now().UnixNano())
	if n := f.hits["/ocs/v2.php/apps/provisioning_api/api/v1/config/apps/core/lastcron"]; n != 2 {
		t.Fatalf("%d", n)
	}
}

func TestWebDAVLoginCheck(t *testing.T) {
	f, ts := newFake(t)
	now := time.Now().UnixNano()
	pts, _ := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "p"}, now)
	m := byName(pts)
	if m["nextcloud_webdav_ok"].Value != 1 || m["nextcloud_webdav_response_seconds"].Name == "" {
		t.Fatalf("%+v", m["nextcloud_webdav_ok"])
	}
	resetDeepChecks()
	f.davCode = 401
	pts, err := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "wrong"}, now)
	m = byName(pts)
	if err == nil || !strings.Contains(err.Error(), "WebDAV login was refused") || m["nextcloud_webdav_ok"].Value != 0 || m["nextcloud_webdav_response_seconds"].Name != "" {
		t.Fatalf("a refused login is a 0, with a reason: %v %+v", err, m["nextcloud_webdav_ok"])
	}
	resetDeepChecks()
	f.davCode = 500
	pts, err = CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Username: "monitor", Password: "p"}, now)
	if byName(pts)["nextcloud_webdav_ok"].Value != 0 || err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("a server error is a 0 too: %v", err)
	}
}

func TestUpdateAvailabilityFromServerinfo(t *testing.T) {
	f, ts := newFake(t)
	now := time.Now().UnixNano()
	get := func() ([]Point, map[string]Point) {
		resetDeepChecks()
		pts, _ := CollectNextcloud(context.Background(), NextcloudTarget{URL: ts.URL, Token: "tok"}, now)
		return pts, byName(pts)
	}
	f.update = `{"lastupdatedat":1759000000,"available":true,"available_version":"34.0.3"}`
	pts, m := get()
	var info Point
	for _, p := range pts {
		if p.Name == "nextcloud_info" {
			info = p
		}
	}
	if m["nextcloud_update_available"].Value != 1 || info.Attrs["update_version"] != "34.0.3" || m["nextcloud_update_checked_timestamp_seconds"].Value != 1759000000 {
		t.Fatalf("%+v %v", m["nextcloud_update_available"], info.Attrs)
	}
	f.update = `{"lastupdatedat":1759000000,"available":false}`
	pts, m = get()
	if p, ok := m["nextcloud_update_available"]; !ok || p.Value != 0 {
		t.Fatal("no update: 0")
	}
	f.update = ""
	_, m = get()
	if _, ok := m["nextcloud_update_available"]; ok {
		t.Fatal("when serverinfo does not say, nothing is reported (not a false 0)")
	}
}

func certServer(t *testing.T, notAfter time.Time) (*httptest.Server, *x509.Certificate) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "cloud.test"}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ts := httptest.NewUnstartedServer(http.NotFoundHandler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, cert
}

func TestCertificateExpiry(t *testing.T) {
	resetDeepChecks()
	defer func() { tlsRoots = nil }()
	soon := time.Now().Add(10 * 24 * time.Hour)
	ts, cert := certServer(t, soon)
	now := time.Now().UnixNano()
	pts, err := collectDeep(context.Background(), NextcloudTarget{URL: ts.URL}, now, true)
	m := byName(pts)
	if err != nil {
		t.Fatal(err)
	}
	days := m["nextcloud_tls_cert_expiry_seconds"].Value / 86400
	if math.Abs(days-10) > 0.1 || m["nextcloud_tls_cert_not_after_timestamp_seconds"].Value != float64(soon.Unix()) {
		t.Fatalf("expires in 10 days: %v", days)
	}
	if m["nextcloud_tls_cert_valid"].Value != 0 {
		t.Fatal("a certificate that no trusted authority signed is not valid")
	}
	// trusted, and for this name: valid
	resetDeepChecks()
	tlsRoots = x509.NewCertPool()
	tlsRoots.AddCert(cert)
	pts, _ = collectDeep(context.Background(), NextcloudTarget{URL: ts.URL}, now, true)
	if byName(pts)["nextcloud_tls_cert_valid"].Value != 1 {
		t.Fatal("trusted and for 127.0.0.1")
	}
	// an expired certificate is reported with a negative time, also when the instance is down (it is often why)
	resetDeepChecks()
	ts2, cert2 := certServer(t, time.Now().Add(-24*time.Hour))
	tlsRoots.AddCert(cert2)
	pts, _ = collectDeep(context.Background(), NextcloudTarget{URL: ts2.URL}, now, false)
	m = byName(pts)
	if m["nextcloud_tls_cert_expiry_seconds"].Value > -80000 || m["nextcloud_tls_cert_valid"].Value != 0 {
		t.Fatalf("expired a day ago: %+v", m["nextcloud_tls_cert_expiry_seconds"])
	}
	// plain http has no certificate, and an address nobody answers on is an error but not a crash
	resetDeepChecks()
	if pts, _ := collectDeep(context.Background(), NextcloudTarget{URL: "http://127.0.0.1:1"}, now, true); len(pts) != 0 {
		t.Fatal("http: no certificate points")
	}
	if _, err := collectDeep(context.Background(), NextcloudTarget{URL: "https://127.0.0.1:1"}, now, true); err == nil || !strings.Contains(err.Error(), "cannot read the TLS certificate") {
		t.Fatalf("%v", err)
	}
}
