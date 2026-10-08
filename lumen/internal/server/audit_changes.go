package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/perm"
)

// WithAudit gives the server the log in which every change somebody makes is recorded.
func (s *Server) WithAudit(l *audit.Log) *Server { s.audit = l; return s }

func (s *Server) auditLog() *audit.Log {
	if s.audit != nil {
		return s.audit
	}
	if s.ten != nil {
		return s.ten.Audit
	}
	return nil
}

// ---- recording what a request changed ----

// statusRec notes what a handler answered, so that only changes that went through are recorded.
type statusRec struct {
	http.ResponseWriter
	code int
}

func (s *statusRec) WriteHeader(c int) {
	if s.code == 0 {
		s.code = c
	}
	s.ResponseWriter.WriteHeader(c)
}
func (s *statusRec) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}
func (s *statusRec) Unwrap() http.ResponseWriter { return s.ResponseWriter }
func (s *statusRec) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// bodyTap keeps the first bytes of a request body while the handler reads it, so that the record can say what was changed
// ("created user alice") without every handler having to say it.
type bodyTap struct {
	io.ReadCloser
	buf bytes.Buffer
}

const tapMax = 8192

func (b *bodyTap) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.buf.Len() < tapMax {
		room := tapMax - b.buf.Len()
		if n < room {
			room = n
		}
		b.buf.Write(p[:room])
	}
	return n, err
}

// fields picks the few harmless things a request says. Never a password, a key, a token, a secret or an address that may carry
// one: only names. A password is only noted as "changed".
func (b *bodyTap) fields() map[string]string {
	out := map[string]string{}
	var m map[string]json.RawMessage
	if json.Unmarshal(b.buf.Bytes(), &m) != nil {
		return out
	}
	for _, k := range []string{"name", "username", "user", "group", "host", "address", "status", "kind", "role", "display_name", "dir", "from", "to", "title", "severity", "enabled", "port"} {
		var v any
		if raw, ok := m[k]; ok && json.Unmarshal(raw, &v) == nil {
			switch x := v.(type) {
			case string:
				if x != "" {
					out[k] = clipText(x, 80)
				}
			case bool, float64:
				out[k] = fmt.Sprint(x)
			}
		}
	}
	for _, k := range []string{"add", "remove", "groups", "hosts"} {
		var l []any
		if raw, ok := m[k]; ok && json.Unmarshal(raw, &l) == nil {
			out[k+"_n"] = fmt.Sprint(len(l))
		}
	}
	if _, ok := m["password"]; ok {
		out["password"] = "yes"
	}
	return out
}

func clipText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

type changeRule struct {
	method string
	re     *regexp.Regexp
	f      func(g []string, b map[string]string) (action, summary, target string)
}

func first(b map[string]string, keys ...string) string {
	for _, k := range keys {
		if b[k] != "" {
			return b[k]
		}
	}
	return ""
}

func rule(method, pattern string, f func(g []string, b map[string]string) (string, string, string)) changeRule {
	return changeRule{method, regexp.MustCompile("^" + pattern + "$"), f}
}

func plain(action, format string, field ...string) func([]string, map[string]string) (string, string, string) {
	return func(g []string, b map[string]string) (string, string, string) {
		args := make([]any, 0, len(g)+len(field))
		for _, x := range g[1:] {
			args = append(args, x)
		}
		for _, k := range field {
			args = append(args, first(b, strings.Split(k, "|")...))
		}
		target := ""
		if len(g) > 1 {
			target = g[1]
		} else if len(field) > 0 {
			target = first(b, strings.Split(field[0], "|")...)
		}
		return action, fmt.Sprintf(format, args...), target
	}
}

// changeRules say, in words, what a request that changes something did. A request that is not here is still recorded, as
// "POST /api/v1/...", so that nothing is ever missing from the log; this table only makes the common ones readable.
var changeRules = []changeRule{
	rule("POST", `/api/v1/users`, plain("user.create", "created the user %s", "username|name")),
	rule("PUT", `/api/v1/users/([^/]+)`, func(g []string, b map[string]string) (string, string, string) {
		switch {
		case b["password"] != "":
			return "user.password", "reset the password of " + g[1], g[1]
		case b["group"] != "":
			return "user.group", "put " + g[1] + " in the group " + b["group"], g[1]
		}
		return "user.update", "changed the user " + g[1], g[1]
	}),
	rule("DELETE", `/api/v1/users/([^/]+)`, plain("user.delete", "deleted the user %s")),
	rule("POST", `/api/v1/password`, plain("password.change", "changed their own password")),
	rule("POST", `/api/v1/groups`, plain("group.create", "created the group %s", "name")),
	rule("PUT", `/api/v1/groups/([^/]+)`, plain("group.update", "changed the group %s (permissions or name)")),
	rule("DELETE", `/api/v1/groups/([^/]+)`, plain("group.delete", "deleted the group %s")),
	rule("POST", `/api/v1/keys`, plain("key.create", "created the API key %s", "name")),
	rule("DELETE", `/api/v1/keys/([^/]+)`, plain("key.delete", "deleted an API key (%s)")),
	rule("PUT", `/api/v1/hosts/([^/]+)`, plain("host.update", "changed the settings of the host %s")),
	rule("DELETE", `/api/v1/hosts/([^/]+)`, plain("host.delete", "removed the host %s")),
	rule("POST", `/api/v1/hosts/([^/]+)/remove`, plain("host.remove", "removed the host %s from the list")),
	rule("POST", `/api/v1/hosts/([^/]+)/update`, plain("host.update-agent", "asked the agent on %s to update itself")),
	rule("POST", `/api/v1/hosts-update-all`, plain("host.update-all", "asked every agent that can to update itself")),
	rule("POST", `/api/v1/instances`, plain("instance.create", "added the instance %s", "name")),
	rule("PUT", `/api/v1/instances/([^/]+)`, plain("instance.update", "changed the instance %s", "name")),
	rule("DELETE", `/api/v1/instances/([^/]+)`, plain("instance.delete", "removed the instance %s")),
	rule("POST", `/api/v1/instance-keys/(.+)/remove`, plain("instance.remove", "took the instance %s off the list")),
	rule("POST", `/api/v1/host-groups/members`, func(g []string, b map[string]string) (string, string, string) {
		return "hostgroup.members", fmt.Sprintf("changed the host group %s (%s added, %s removed)", b["group"], orZero(b["add_n"]), orZero(b["remove_n"])), b["group"]
	}),
	rule("POST", `/api/v1/host-groups/rename`, plain("hostgroup.rename", "renamed the host group %s to %s", "from", "to")),
	rule("POST", `/api/v1/host-groups/delete`, plain("hostgroup.delete", "deleted the host group %s", "group")),
	rule("POST", `/api/v1/services/dismiss`, plain("service.dismiss", "took %s off the Home list", "name")),
	rule("POST", `/api/v1/alerts/rules`, plain("alert.rule.create", "created the alert rule %s", "name")),
	rule("PUT", `/api/v1/alerts/rules/([^/]+)`, plain("alert.rule.update", "changed the alert rule %s")),
	rule("DELETE", `/api/v1/alerts/rules/([^/]+)`, plain("alert.rule.delete", "deleted the alert rule %s")),
	rule("POST", `/api/v1/alerts/silences`, plain("alert.silence.create", "silenced alerts")),
	rule("DELETE", `/api/v1/alerts/silences/([^/]+)`, plain("alert.silence.delete", "ended the silence %s")),
	rule("POST", `/api/v1/alerts/ack/([^/]+)`, plain("alert.ack", "acknowledged an alert")),
	rule("DELETE", `/api/v1/alerts/ack/([^/]+)`, plain("alert.unack", "took back the acknowledgement of an alert")),
	rule("POST", `/api/v1/notifications/channels`, plain("channel.create", "created the notification channel %s (%s)", "name", "kind")),
	rule("PUT", `/api/v1/notifications/channels/([^/]+)`, plain("channel.update", "changed the notification channel %s")),
	rule("DELETE", `/api/v1/notifications/channels/([^/]+)`, plain("channel.delete", "deleted a notification channel (%s)")),
	rule("POST", `/api/v1/dashboards`, plain("dashboard.create", "created the dashboard %s", "name")),
	rule("PUT", `/api/v1/dashboards/([^/]+)`, plain("dashboard.update", "changed the dashboard %s", "name")),
	rule("DELETE", `/api/v1/dashboards/([^/]+)`, plain("dashboard.delete", "deleted a dashboard (%s)")),
	rule("PUT", `/api/v1/settings/branding`, plain("settings.branding", "changed the site name or logo")),
	rule("PUT", `/api/v1/settings/oidc`, plain("settings.oidc", "changed the single sign-on (OIDC) settings")),
	rule("PUT", `/api/v1/settings/license`, plain("settings.license", "installed a licence")),
	rule("DELETE", `/api/v1/settings/license`, plain("settings.license", "removed the licence")),
	rule("PUT", `/api/v1/support-access`, plain("support.access", "changed who may support this tenant")),
	rule("POST", `/api/v1/support-access/grant`, plain("support.grant", "let the provider in for a while")),
	rule("DELETE", `/api/v1/support-access/grant`, plain("support.revoke", "ended the provider's access")),
	rule("POST", `/api/v1/backups/run`, plain("backup.run", "started a backup")),
	rule("POST", `/api/v1/backups/([^/]+)/load`, plain("backup.load", "loaded the backup of %s")),
	rule("DELETE", `/api/v1/backups/([^/]+)/load`, plain("backup.unload", "unloaded the backup of %s")),
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// notAChange lists requests that use POST for something that changes nothing.
var notAChange = map[string]bool{"/api/v1/alerts/backtest": true, "/api/v1/backup-location/check": true, "/api/v1/settings/oidc/test": true}
var testChannelRe = regexp.MustCompile(`^/api/v1/notifications/channels/[^/]+/test$`)

// explicitlyAudited are the paths whose handlers write their own, fuller, entry.
func explicitlyAudited(path string) bool {
	return strings.HasPrefix(path, "/api/v1/operator/") || path == "/api/v1/backup-location"
}

// describeChange turns a request into words.
func describeChange(method, path string, b map[string]string) (action, summary, target string) {
	for _, c := range changeRules {
		if c.method == method {
			if g := c.re.FindStringSubmatch(path); g != nil {
				return c.f(g, b)
			}
		}
	}
	return "http." + method, method + " " + path, path
}

// auditChange records a request that changed something and went through, by whoever made it. It is called by the guard of every
// route, so a route that is added later is recorded too.
func (s *Server) auditChange(r *http.Request, id edition.Identity, code int, tap *bodyTap) {
	log := s.auditLog()
	if log == nil || id.Acting || code >= 400 || r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return // an operator inside a tenant has their own, fuller, entries (support.*)
	}
	if notAChange[r.URL.Path] || testChannelRe.MatchString(r.URL.Path) || explicitlyAudited(r.URL.Path) {
		return
	}
	var f map[string]string
	if tap != nil {
		f = tap.fields()
	}
	action, summary, target := describeChange(r.Method, r.URL.Path, f)
	who, via := id.User, "password"
	if who == "" || id.ViaKey {
		if who == "" {
			who = "an API key"
		}
		via = "api key"
	}
	if err := log.Add(audit.Entry{Tenant: id.Tenant, Actor: who, Via: via, IP: remoteHost(r), Action: action, Summary: summary, Target: target}); err != nil {
		s.log.Error("audit entry could not be written", "action", action, "err", err)
	}
}

// ---- sign-ins ----

var failedLogins = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// auditSignIn records a sign-in or a failed one. A failure is recorded once a minute for each person and place at most, so that a
// password-guessing run does not fill the log.
func (s *Server) auditSignIn(r *http.Request, user, tenant string, ok bool, how string) {
	log := s.auditLog()
	if log == nil {
		return
	}
	e := audit.Entry{Tenant: tenant, Actor: user, IP: remoteHost(r), Via: how}
	if ok {
		e.Action, e.Summary = "login", "signed in"
	} else {
		key := user + "|" + remoteHost(r)
		failedLogins.Lock()
		last := failedLogins.at[key]
		if time.Since(last) < time.Minute {
			failedLogins.Unlock()
			return
		}
		failedLogins.at[key] = time.Now()
		if len(failedLogins.at) > 5000 {
			failedLogins.at = map[string]time.Time{key: time.Now()}
		}
		failedLogins.Unlock()
		e.Action, e.Summary = "login.failed", "failed to sign in"
	}
	if err := log.Add(e); err != nil {
		s.log.Error("audit entry could not be written", "action", e.Action, "err", err)
	}
}

// ---- reading the log ----

func (s *Server) routesAudit(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/audit-log", s.need(perm.Settings, false, s.getAuditLog))
}

func parseWhen(v string, end bool) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			if end && layout == "2006-01-02" {
				t = t.Add(24*time.Hour - time.Nanosecond)
			}
			return t
		}
	}
	return time.Time{}
}

func (s *Server) getAuditLog(w http.ResponseWriter, r *http.Request, id edition.Identity) {
	log := s.auditLog()
	if log == nil {
		writeJSON(w, map[string]any{"data": []audit.Entry{}})
		return
	}
	q := r.URL.Query()
	f := audit.Filter{Actor: q.Get("actor"), Q: q.Get("q"), From: parseWhen(q.Get("from"), false), To: parseWhen(q.Get("to"), true)}
	fmt.Sscan(q.Get("limit"), &f.Limit)
	out := log.Query(id.Tenant, f)
	if out == nil {
		out = []audit.Entry{}
	}
	if q.Get("format") != "csv" {
		writeJSON(w, map[string]any{"data": out})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="lumen-audit-log.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time_utc", "who", "via", "ip", "what", "action", "target", "detail"})
	for _, e := range out {
		_ = cw.Write([]string{e.Time.UTC().Format(time.RFC3339), safeCell(e.Actor), e.Via, e.IP, safeCell(e.Summary), e.Action, safeCell(e.Target), safeCell(e.Detail)})
	}
	cw.Flush()
}

// safeCell stops a spreadsheet from running a cell that starts like a formula.
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
