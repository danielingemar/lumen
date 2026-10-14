package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// RegisterCore registers the notifiers of the open core.
func RegisterCore(r *Registry) {
	for _, n := range []Notifier{emailNotifier{}, webhookNotifier{}, slackNotifier{}, teamsNotifier{}, heartbeatNotifier{}} {
		r.Register(n)
	}
}

func statusOK(code int) bool { return code >= 200 && code < 300 }

// ---- webhook ----

type webhookNotifier struct{}

func (webhookNotifier) Type() string  { return "webhook" }
func (webhookNotifier) Label() string { return "Webhook" }
func (webhookNotifier) Fields() []Field {
	return []Field{
		{Key: "url", Label: "URL", Kind: "text", Required: true, Help: "Lumen sends a JSON POST here."},
		{Key: "secret", Label: "Signing secret", Kind: "secret", Help: "Optional. Each request carries X-Lumen-Timestamp and X-Lumen-Signature (sha256 HMAC of timestamp + \".\" + body) so the receiver can verify it."},
	}
}
func (webhookNotifier) Validate(c Config) error { return CheckURL(c.Get("url"), false) }

// Sign returns the signature header value for a webhook body.
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (webhookNotifier) Send(ctx context.Context, d Deps, c Config, m Message) (Result, error) {
	body, _ := json.Marshal(map[string]any{"version": "1", "status": m.Status, "title": m.Title, "group_key": m.GroupKey, "tenant": m.Tenant, "channel": m.Channel, "link": m.Link, "alerts": m.Alerts})
	h := map[string]string{"Content-Type": "application/json", "User-Agent": "Lumen-Alerts"}
	if s := c.Get("secret"); s != "" {
		ts := strconv.FormatInt(d.Now().Unix(), 10)
		h["X-Lumen-Timestamp"], h["X-Lumen-Signature"] = ts, Sign(s, ts, body)
	}
	code, _, err := do(ctx, d.HTTP, "POST", c.Get("url"), h, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	if !statusOK(code) {
		return Result{}, fmt.Errorf("the webhook answered HTTP %d", code)
	}
	return Result{}, nil
}

// ---- Slack ----

type slackNotifier struct{}

func (slackNotifier) Type() string  { return "slack" }
func (slackNotifier) Label() string { return "Slack" }
func (slackNotifier) Fields() []Field {
	return []Field{{Key: "webhook_url", Label: "Incoming webhook URL", Kind: "secret", Required: true, Help: "From Slack: Apps, Incoming Webhooks. It is a secret."}}
}
func (slackNotifier) Validate(c Config) error { return CheckURL(c.Get("webhook_url"), true) }
func (slackNotifier) Send(ctx context.Context, d Deps, c Config, m Message) (Result, error) {
	text := "*" + oneLine(m.Title) + "*\n" + m.Body
	body, _ := json.Marshal(map[string]string{"text": text})
	code, _, err := do(ctx, d.HTTP, "POST", c.Get("webhook_url"), map[string]string{"Content-Type": "application/json"}, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	if !statusOK(code) {
		return Result{}, fmt.Errorf("Slack answered HTTP %d", code)
	}
	return Result{}, nil
}

// ---- Microsoft Teams ----

type teamsNotifier struct{}

func (teamsNotifier) Type() string  { return "teams" }
func (teamsNotifier) Label() string { return "Microsoft Teams" }
func (teamsNotifier) Fields() []Field {
	return []Field{{Key: "webhook_url", Label: "Incoming webhook URL", Kind: "secret", Required: true, Help: "From the Teams channel: Connectors or Workflows. It is a secret."}}
}
func (teamsNotifier) Validate(c Config) error { return CheckURL(c.Get("webhook_url"), true) }
func (teamsNotifier) Send(ctx context.Context, d Deps, c Config, m Message) (Result, error) {
	colour := "D32F2F"
	if m.Status == "resolved" {
		colour = "2E7D32"
	}
	body, _ := json.Marshal(map[string]any{"@type": "MessageCard", "@context": "http://schema.org/extensions", "summary": oneLine(m.Title), "themeColor": colour, "title": oneLine(m.Title), "text": strings.ReplaceAll(m.Body, "\n", "\n\n")})
	code, _, err := do(ctx, d.HTTP, "POST", c.Get("webhook_url"), map[string]string{"Content-Type": "application/json"}, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	if !statusOK(code) {
		return Result{}, fmt.Errorf("Teams answered HTTP %d", code)
	}
	return Result{}, nil
}

// ---- heartbeat ----

type heartbeatNotifier struct{}

func (heartbeatNotifier) Type() string  { return "heartbeat" }
func (heartbeatNotifier) Label() string { return "Heartbeat (dead man's switch)" }
func (heartbeatNotifier) Fields() []Field {
	return []Field{
		{Key: "url", Label: "URL to call", Kind: "secret", Required: true, Help: "Lumen calls this address regularly. An outside service (for example Healthchecks or Uptime Kuma) alerts you when the calls stop, which means Lumen itself is down. It is a secret."},
		{Key: "interval_seconds", Label: "Every (seconds)", Kind: "number", Default: "60", Help: "30 to 3600."},
	}
}
func (heartbeatNotifier) Validate(c Config) error {
	if err := CheckURL(c.Get("url"), false); err != nil {
		return err
	}
	if v := c.Get("interval_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 30 || n > 3600 {
			return fmt.Errorf("the interval must be 30 to 3600 seconds")
		}
	}
	return nil
}
func (heartbeatNotifier) Send(context.Context, Deps, Config, Message) (Result, error) {
	return Result{}, nil // a heartbeat channel carries no alerts
}
func (heartbeatNotifier) Every(c Config) time.Duration {
	n, err := strconv.Atoi(c.Get("interval_seconds"))
	if err != nil || n < 30 || n > 3600 {
		n = 60
	}
	return time.Duration(n) * time.Second
}
func (heartbeatNotifier) Tick(ctx context.Context, d Deps, c Config) error {
	code, _, err := do(ctx, d.HTTP, "GET", c.Get("url"), map[string]string{"User-Agent": "Lumen-Heartbeat"}, nil)
	if err != nil {
		return err
	}
	if !statusOK(code) {
		return fmt.Errorf("the heartbeat address answered HTTP %d", code)
	}
	return nil
}

// ---- email ----

type emailNotifier struct{}

func (emailNotifier) Type() string  { return "email" }
func (emailNotifier) Label() string { return "Email (SMTP)" }
func (emailNotifier) Fields() []Field {
	return []Field{
		{Key: "to", Label: "Send to", Kind: "text", Required: true, Help: "One or more addresses, separated by commas (at most 10)."},
		{Key: "from", Label: "From", Kind: "text", Required: true, Help: "For example Lumen <lumen@example.com>."},
		{Key: "host", Label: "SMTP server", Kind: "text", Required: true},
		{Key: "port", Label: "Port", Kind: "number", Default: "587"},
		{Key: "security", Label: "Encryption", Kind: "select", Options: []string{"starttls", "tls", "none"}, Default: "starttls", Help: "starttls: port 587. tls: port 465. none only for a trusted internal relay."},
		{Key: "username", Label: "User name", Kind: "text", Help: "Leave empty if the server needs no login."},
		{Key: "password", Label: "Password", Kind: "secret"},
	}
}

func parseAddrs(list string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		a, err := mail.ParseAddress(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("%q is not an email address", strings.TrimSpace(p))
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("give at least one address")
	}
	if len(out) > 10 {
		return nil, fmt.Errorf("at most 10 addresses")
	}
	return out, nil
}

func (emailNotifier) Validate(c Config) error {
	if _, err := parseAddrs(c.Get("to")); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if _, err := mail.ParseAddress(c.Get("from")); err != nil {
		return fmt.Errorf("from: that is not an email address")
	}
	h := c.Get("host")
	if h == "" || strings.ContainsAny(h, " /\r\n:@") {
		return fmt.Errorf("the SMTP server must be a host name or address")
	}
	if p := c.Get("port"); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("the port must be 1-65535")
		}
	}
	switch c.Get("security") {
	case "", "starttls", "tls", "none":
	default:
		return fmt.Errorf("encryption must be starttls, tls or none")
	}
	if c.Get("username") != "" && c.Get("password") == "" {
		return fmt.Errorf("a password is needed when a user name is given")
	}
	return nil
}

func (emailNotifier) Send(ctx context.Context, d Deps, c Config, m Message) (Result, error) {
	to, err := parseAddrs(c.Get("to"))
	if err != nil {
		return Result{}, err
	}
	from, err := mail.ParseAddress(c.Get("from"))
	if err != nil {
		return Result{}, fmt.Errorf("from: not an email address")
	}
	host, port, sec := c.Get("host"), c.Get("port"), c.Get("security")
	if port == "" {
		port = "587"
	}
	if sec == "" {
		sec = "starttls"
	}
	conn, err := d.Dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return Result{}, fmt.Errorf("cannot connect to the mail server: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second)) // a network deadline is always real time, never the engine's clock
	if sec == "tls" {
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return Result{}, fmt.Errorf("TLS handshake with the mail server failed: %w", err)
		}
		conn = tc
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return Result{}, fmt.Errorf("the mail server did not greet us: %w", err)
	}
	defer cl.Close()
	if sec == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return Result{}, fmt.Errorf("the mail server does not offer STARTTLS; choose another encryption or fix the server")
		}
		if err := cl.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return Result{}, fmt.Errorf("STARTTLS failed: %w", err)
		}
	}
	if u := c.Get("username"); u != "" {
		if err := cl.Auth(smtp.PlainAuth("", u, c.Get("password"), host)); err != nil {
			return Result{}, fmt.Errorf("the mail server refused the login: %w", err)
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return Result{}, fmt.Errorf("sender refused: %w", err)
	}
	for _, a := range to {
		if err := cl.Rcpt(a.Address); err != nil {
			return Result{}, fmt.Errorf("recipient %s refused: %w", a.Address, err)
		}
	}
	w, err := cl.Data()
	if err != nil {
		return Result{}, err
	}
	if _, err := w.Write(buildMail(from, to, m, d.Now())); err != nil {
		return Result{}, err
	}
	if err := w.Close(); err != nil {
		return Result{}, fmt.Errorf("the mail server did not accept the message: %w", err)
	}
	_ = cl.Quit()
	return Result{}, nil
}

// buildMail makes the message. Every header value is built from validated addresses or from text with line breaks
// removed and encoded, so nothing a rule or a label contains can add a header (header injection).
func buildMail(from *mail.Address, to []*mail.Address, m Message, now time.Time) []byte {
	var rcpt []string
	for _, a := range to {
		rcpt = append(rcpt, a.String())
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	dom := "lumen"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		dom = from.Address[i+1:]
	}
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from.String())
	h("To", strings.Join(rcpt, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", oneLine(m.Title)))
	h("Date", now.UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+dom+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	h("X-Lumen-Status", oneLine(m.Status))
	b.WriteString("\r\n")
	q := quotedprintable.NewWriter(&b)
	_, _ = q.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n")))
	_ = q.Close()
	return b.Bytes()
}
