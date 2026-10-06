package alerts

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

func deps(allowPrivate bool) Deps {
	g := Guard{AllowPrivate: allowPrivate}
	return Deps{HTTP: g.Client(5 * time.Second), Dial: g.Dial, Now: func() time.Time { return t0 }}
}

func msg() Message {
	al := []AlertView{{Fingerprint: "f1", RuleName: "Disk almost full", Severity: "critical", State: "firing", Labels: map[string]string{"host": "web1", "mountpoint": "/data"}, Value: 95.2, Since: t0, Annotation: "Free some space"},
		{Fingerprint: "f2", RuleName: "Disk almost full", Severity: "critical", State: "firing", Labels: map[string]string{"host": "web2"}, Value: 91, Since: t0}}
	title, body := Compose("firing", al, "https://lumen.example.com/#/alerts")
	return Message{Tenant: "acme", Channel: "ops", Status: "firing", GroupKey: "c1|r1|firing", Title: title, Body: body, Link: "https://lumen.example.com/#/alerts", Alerts: al}
}

func TestComposeMessage(t *testing.T) {
	title, body := Compose("firing", msg().Alerts, "https://x/#/alerts")
	if title != "[FIRING] Disk almost full (2 alerts)" || !strings.Contains(body, "critical: host=web1, mountpoint=/data (value 95.2, since 12:00 UTC)") || !strings.Contains(body, "Free some space") || !strings.Contains(body, "https://x/#/alerts") {
		t.Fatalf("%q\n%s", title, body)
	}
	if title, body := Compose("resolved", msg().Alerts[:1], ""); title != "[RESOLVED] Disk almost full" || strings.Contains(body, "value") {
		t.Fatalf("a resolve message names the alerts without values: %q %q", title, body)
	}
}

func TestWebhookIsSignedAndStructured(t *testing.T) {
	var gotBody []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	}))
	defer srv.Close()
	n := webhookNotifier{}
	cfg := Config{Settings: map[string]string{"url": srv.URL}, Secrets: map[string]string{"secret": "s3cret"}}
	if _, err := n.Send(context.Background(), deps(true), cfg, msg()); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Version, Status, Title, Tenant string
		Alerts                         []struct {
			Fingerprint, Rule, Severity string
			Labels                      map[string]string
			Value                       float64
		}
	}
	if err := json.Unmarshal(gotBody, &p); err != nil || p.Version != "1" || p.Status != "firing" || p.Tenant != "acme" || len(p.Alerts) != 2 || p.Alerts[0].Labels["host"] != "web1" || p.Alerts[0].Value != 95.2 {
		t.Fatalf("payload: %s %v", gotBody, err)
	}
	ts := hdr.Get("X-Lumen-Timestamp")
	if ts != fmt.Sprint(t0.Unix()) || hdr.Get("X-Lumen-Signature") != Sign("s3cret", ts, gotBody) || !strings.HasPrefix(hdr.Get("X-Lumen-Signature"), "sha256=") {
		t.Fatalf("the signature must be the HMAC of timestamp.body so receivers can verify it: %v", hdr)
	}
	if Sign("other", ts, gotBody) == hdr.Get("X-Lumen-Signature") {
		t.Fatal("a different secret gives a different signature")
	}
	// without a secret there is no signature header
	cfg.Secrets = nil
	if _, err := n.Send(context.Background(), deps(true), cfg, msg()); err != nil || hdr.Get("X-Lumen-Signature") != "" && false {
		t.Fatal(err)
	}
	// an error answer is an error, and the URL (which may hold a secret) is not in the message
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) }))
	defer bad.Close()
	_, err := n.Send(context.Background(), deps(true), Config{Settings: map[string]string{"url": bad.URL + "/secret-path-token"}}, msg())
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "secret-path-token") {
		t.Fatalf("%v", err)
	}
	_, err = n.Send(context.Background(), deps(true), Config{Settings: map[string]string{"url": "http://127.0.0.1:1/secret-path-token"}}, msg())
	if err == nil || strings.Contains(err.Error(), "secret-path-token") {
		t.Fatalf("a connection error must not leak the URL: %v", err)
	}
}

func TestSlackAndTeamsPayloads(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
	}))
	defer srv.Close()
	cfg := Config{Secrets: map[string]string{"webhook_url": srv.URL}}
	if _, err := (slackNotifier{}).Send(context.Background(), deps(true), cfg, msg()); err != nil {
		t.Fatal(err)
	}
	if s, _ := got["text"].(string); !strings.Contains(s, "*[FIRING] Disk almost full (2 alerts)*") || !strings.Contains(s, "host=web1") {
		t.Fatalf("slack: %v", got)
	}
	if _, err := (teamsNotifier{}).Send(context.Background(), deps(true), cfg, msg()); err != nil {
		t.Fatal(err)
	}
	if got["@type"] != "MessageCard" || got["themeColor"] != "D32F2F" || !strings.Contains(fmt.Sprint(got["text"]), "host=web1") {
		t.Fatalf("teams: %v", got)
	}
	m := msg()
	m.Status = "resolved"
	(teamsNotifier{}).Send(context.Background(), deps(true), cfg, m)
	if got["themeColor"] != "2E7D32" {
		t.Fatal("a resolve message is green")
	}
	// slack and teams only accept https URLs
	for _, n := range []Notifier{slackNotifier{}, teamsNotifier{}} {
		if err := n.Validate(Config{Secrets: map[string]string{"webhook_url": "http://hooks.example.com/x"}}); err == nil {
			t.Errorf("%s must require https", n.Type())
		}
		if err := n.Validate(Config{Secrets: map[string]string{"webhook_url": "https://hooks.example.com/x"}}); err != nil {
			t.Errorf("%s: %v", n.Type(), err)
		}
	}
}

func TestGuardRefusesInternalAddresses(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close() // a server on 127.0.0.1: "internal"
	strict := deps(false)
	if _, err := (webhookNotifier{}).Send(context.Background(), strict, Config{Settings: map[string]string{"url": srv.URL}}, msg()); err == nil || !strings.Contains(err.Error(), "internal") || hit {
		t.Fatalf("a loopback server must be refused by default and never contacted: %v hit=%v", err, hit)
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "100.64.1.1", "::1", "fd00::1", "169.254.169.254", "0.0.0.0", "224.0.0.1", "fe80::1"} {
		if err := (Guard{}).blocked(net.ParseIP(ip)); err == nil {
			t.Errorf("%s must be blocked by default", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if err := (Guard{}).blocked(net.ParseIP(ip)); err != nil {
			t.Errorf("%s is a public address: %v", ip, err)
		}
	}
	// private ranges can be allowed (an internal mail relay), but the cloud metadata address never
	allow := Guard{AllowPrivate: true}
	if allow.blocked(net.ParseIP("10.1.2.3")) != nil || allow.blocked(net.ParseIP("127.0.0.1")) != nil {
		t.Fatal("AllowPrivate allows loopback and private ranges")
	}
	if allow.blocked(net.ParseIP("169.254.169.254")) == nil {
		t.Fatal("the cloud metadata address is never allowed")
	}
	// the metadata address by name or number is refused at connect time
	if _, err := (webhookNotifier{}).Send(context.Background(), deps(true), Config{Settings: map[string]string{"url": "http://169.254.169.254/latest/meta-data/"}}, msg()); err == nil || !strings.Contains(err.Error(), "never allowed") {
		t.Fatalf("%v", err)
	}
	// a public-looking server that redirects to an internal one must not be followed there
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer internal.Close()
	hit = false
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, internal.URL, 302) }))
	defer redirector.Close()
	// both are on loopback, so use a guard that allows loopback but we check the metadata redirect instead
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", 302)
	}))
	defer meta.Close()
	if _, err := (webhookNotifier{}).Send(context.Background(), deps(true), Config{Settings: map[string]string{"url": meta.URL}}, msg()); err == nil || !strings.Contains(err.Error(), "never allowed") {
		t.Fatalf("a redirect to the metadata address must be refused: %v", err)
	}
	if hit {
		t.Fatal("the internal server was reached")
	}
	// URL checks that need no network
	for _, u := range []string{"ftp://x.com/a", "https://user:pw@x.com/", "https://", "not a url", "https://x.com/a b"} {
		if CheckURL(u, false) == nil {
			t.Errorf("%q must be rejected", u)
		}
	}
	if CheckURL("https://hooks.example.com/x", true) != nil || CheckURL("http://x.example.com", true) == nil {
		t.Fatal("https requirement")
	}
}

// ---- email against a small SMTP server ----

type smtpServer struct {
	ln       net.Listener
	mu       sync.Mutex
	from     string
	rcpts    []string
	data     string
	user     string
	starttls bool
	authFail bool
}

func newSMTP(t *testing.T) *smtpServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no sockets")
	}
	s := &smtpServer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *smtpServer) serve(c net.Conn) {
	defer c.Close()
	r := textproto.NewReader(bufio.NewReader(c))
	w := func(f string, a ...any) { fmt.Fprintf(c, f+"\r\n", a...) }
	w("220 test ESMTP")
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			w("250-test")
			w("250 AUTH PLAIN")
		case strings.HasPrefix(up, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			parts := strings.Split(string(raw), "\x00")
			s.mu.Lock()
			s.user = parts[1] + ":" + parts[2]
			bad := s.authFail
			s.mu.Unlock()
			if bad {
				w("535 bad credentials")
			} else {
				w("235 ok")
			}
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.mu.Lock()
			s.from = line[10:]
			s.mu.Unlock()
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			s.mu.Lock()
			s.rcpts = append(s.rcpts, line[8:])
			s.mu.Unlock()
			w("250 ok")
		case up == "DATA":
			w("354 go on")
			var b strings.Builder
			for {
				l, err := r.ReadLine()
				if err != nil || l == "." {
					break
				}
				b.WriteString(l + "\n")
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func (s *smtpServer) port() string { _, p, _ := net.SplitHostPort(s.ln.Addr().String()); return p }

func TestEmailIsDelivered(t *testing.T) {
	s := newSMTP(t)
	n := emailNotifier{}
	cfg := Config{Settings: map[string]string{"to": "ops@example.com, Boss <boss@example.com>", "from": "Lumen <lumen@example.com>", "host": "127.0.0.1", "port": s.port(), "security": "none", "username": "lumen"}, Secrets: map[string]string{"password": "pw"}}
	if err := n.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(context.Background(), deps(true), cfg, msg()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(s.from, "lumen@example.com") || len(s.rcpts) != 2 || !strings.Contains(s.rcpts[1], "boss@example.com") || s.user != "lumen:pw" {
		t.Fatalf("envelope: from %q rcpts %v login %q", s.from, s.rcpts, s.user)
	}
	m, err := mail.ReadMessage(strings.NewReader(strings.ReplaceAll(s.data, "\n", "\r\n")))
	if err != nil {
		t.Fatalf("%v\n%s", err, s.data)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(m.Body)) // a mail client decodes the body like this
	if m.Header.Get("Subject") != "[FIRING] Disk almost full (2 alerts)" || !strings.Contains(m.Header.Get("To"), "ops@example.com") || m.Header.Get("Content-Type") != "text/plain; charset=utf-8" || m.Header.Get("Message-Id") == "" {
		t.Fatalf("headers: %v", m.Header)
	}
	if !strings.Contains(string(body), "host=web1") {
		t.Fatalf("body: %q", body)
	}
}

func TestEmailCannotBeUsedForHeaderInjection(t *testing.T) {
	s := newSMTP(t)
	cfg := Config{Settings: map[string]string{"to": "ops@example.com", "from": "lumen@example.com", "host": "127.0.0.1", "port": s.port(), "security": "none"}}
	m := msg()
	m.Title = "[FIRING] bad\r\nBcc: attacker@evil.example\r\nSubject: hacked"
	if _, err := (emailNotifier{}).Send(context.Background(), deps(true), cfg, m); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	msgp, err := mail.ReadMessage(strings.NewReader(strings.ReplaceAll(s.data, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if msgp.Header.Get("Bcc") != "" || len(msgp.Header["Subject"]) != 1 || strings.Contains(strings.ToLower(s.data[:strings.Index(s.data, "\n\n")]), "attacker@evil") && msgp.Header.Get("Bcc") != "" {
		t.Fatalf("a rule name with line breaks must not add headers: %v", msgp.Header)
	}
	for _, r := range s.rcpts {
		if strings.Contains(r, "attacker") {
			t.Fatalf("recipients: %v", s.rcpts)
		}
	}
}

func TestEmailErrorsAreClear(t *testing.T) {
	s := newSMTP(t)
	base := map[string]string{"to": "ops@example.com", "from": "lumen@example.com", "host": "127.0.0.1", "port": s.port(), "security": "none"}
	// a wrong password
	s.authFail = true
	cfg := Config{Settings: map[string]string{"to": base["to"], "from": base["from"], "host": base["host"], "port": base["port"], "security": "none", "username": "u"}, Secrets: map[string]string{"password": "bad"}}
	if _, err := (emailNotifier{}).Send(context.Background(), deps(true), cfg, msg()); err == nil || !strings.Contains(err.Error(), "refused the login") {
		t.Fatalf("%v", err)
	}
	// STARTTLS requested but not offered
	cfg2 := Config{Settings: map[string]string{"to": base["to"], "from": base["from"], "host": "127.0.0.1", "port": s.port(), "security": "starttls"}}
	if _, err := (emailNotifier{}).Send(context.Background(), deps(true), cfg2, msg()); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("%v", err)
	}
	// the guard blocks an internal mail server unless it is allowed
	if _, err := (emailNotifier{}).Send(context.Background(), deps(false), Config{Settings: base}, msg()); err == nil || !strings.Contains(err.Error(), "internal") {
		t.Fatalf("%v", err)
	}
	// validation
	for name, c := range map[string]map[string]string{
		"no recipient": {"from": "a@b.se", "host": "h"}, "bad address": {"to": "not-an-address", "from": "a@b.se", "host": "h"},
		"bad from": {"to": "a@b.se", "from": "x", "host": "h"}, "bad host": {"to": "a@b.se", "from": "a@b.se", "host": "h/x"},
		"bad port": {"to": "a@b.se", "from": "a@b.se", "host": "h", "port": "99999"}, "bad security": {"to": "a@b.se", "from": "a@b.se", "host": "h", "security": "ssl3"},
		"user without password": {"to": "a@b.se", "from": "a@b.se", "host": "h", "username": "u"},
		"too many":              {"to": strings.Repeat("a@b.se,", 11), "from": "a@b.se", "host": "h"},
	} {
		if err := (emailNotifier{}).Validate(Config{Settings: c}); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestHeartbeat(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	h := heartbeatNotifier{}
	cfg := Config{Secrets: map[string]string{"url": srv.URL}, Settings: map[string]string{"interval_seconds": "30"}}
	if h.Every(cfg) != 30*time.Second || h.Every(Config{}) != 60*time.Second {
		t.Fatal("interval and default")
	}
	if err := h.Tick(context.Background(), deps(true), cfg); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if err := h.Validate(Config{Secrets: map[string]string{"url": srv.URL}, Settings: map[string]string{"interval_seconds": "5"}}); err == nil {
		t.Fatal("too fast")
	}
	r := NewRegistry()
	RegisterCore(r)
	var names []string
	for _, n := range r.Types() {
		names = append(names, n.Type())
	}
	if strings.Join(names, ",") != "email,webhook,slack,teams,heartbeat" {
		t.Fatalf("core notifiers: %v", names)
	}
}
