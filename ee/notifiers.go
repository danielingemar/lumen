//go:build enterprise

package ee

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielingemar/lumen/internal/alerts"
)

// RegisterEnterprise adds the ticket and on-call notifiers. The community core never imports this package.
func RegisterEnterprise(r *alerts.Registry) {
	for _, n := range []alerts.Notifier{pagerDuty{}, opsgenie{}, jira{}, serviceNow{}} {
		r.Register(n)
	}
}

func init() { RegisterEnterprise(alerts.Default) }

func ok2xx(c int) bool { return c >= 200 && c < 300 }

func clip(s string, n int) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func labelsText(l map[string]string) string {
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	// stable order
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return strings.Join(parts, ", ")
}

func summary(a alerts.AlertView) string {
	return clip(fmt.Sprintf("%s: %s", a.RuleName, labelsText(a.Labels)), 130)
}

func details(a alerts.AlertView, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Rule: %s\nSeverity: %s\nLabels: %s\nValue: %.4g\nSince: %s\n", a.RuleName, a.Severity, labelsText(a.Labels), a.Value, a.Since.UTC().Format("2006-01-02 15:04 UTC"))
	if a.Annotation != "" {
		fmt.Fprintf(&b, "\n%s\n", a.Annotation)
	}
	if link != "" {
		fmt.Fprintf(&b, "\nOpen in Lumen: %s\n", link)
	}
	return b.String()
}

// send makes a JSON request and returns the status and at most 64 KB of the answer. Errors never contain the URL.
func send(ctx context.Context, c *http.Client, method, rawURL string, headers map[string]string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Lumen-Alerts")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok {
			return 0, nil, fmt.Errorf("%s: %v", ue.Op, ue.Err)
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func checkHTTPS(raw, what string) error {
	if err := alerts.CheckURL(raw, false); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// ---------------- PagerDuty (Events API v2) ----------------

type pagerDuty struct{}

func (pagerDuty) Type() string  { return "pagerduty" }
func (pagerDuty) Label() string { return "PagerDuty" }
func (pagerDuty) Fields() []alerts.Field {
	return []alerts.Field{
		{Key: "routing_key", Label: "Integration (routing) key", Kind: "secret", Required: true, Help: "From the PagerDuty service: Integrations, Events API v2."},
		{Key: "api_url", Label: "Events API URL", Kind: "text", Help: "Leave empty for https://events.pagerduty.com/v2/enqueue."},
	}
}
func (pagerDuty) Validate(c alerts.Config) error {
	if c.Get("routing_key") == "" {
		return fmt.Errorf("the routing key is required")
	}
	if u := c.Get("api_url"); u != "" {
		return checkHTTPS(u, "Events API URL")
	}
	return nil
}
func (pagerDuty) Send(ctx context.Context, d alerts.Deps, c alerts.Config, m alerts.Message) (alerts.Result, error) {
	endpoint := c.Get("api_url")
	if endpoint == "" {
		endpoint = "https://events.pagerduty.com/v2/enqueue"
	}
	sev := map[string]string{"critical": "critical", "warning": "warning", "info": "info"}
	for _, a := range m.Alerts {
		body := map[string]any{"routing_key": c.Get("routing_key"), "dedup_key": a.Fingerprint}
		if m.Status == "resolved" {
			body["event_action"] = "resolve"
		} else {
			body["event_action"] = "trigger"
			body["payload"] = map[string]any{"summary": summary(a), "source": "Lumen", "severity": sev[a.Severity], "custom_details": map[string]any{"labels": a.Labels, "value": a.Value, "annotation": a.Annotation, "since": a.Since}}
			if m.Link != "" {
				body["links"] = []map[string]string{{"href": m.Link, "text": "Open in Lumen"}}
			}
		}
		code, _, err := send(ctx, d.HTTP, "POST", endpoint, nil, body)
		if err != nil {
			return alerts.Result{}, err
		}
		if !ok2xx(code) {
			return alerts.Result{}, fmt.Errorf("PagerDuty answered HTTP %d", code)
		}
	}
	return alerts.Result{}, nil
}

// ---------------- Opsgenie ----------------

type opsgenie struct{}

func (opsgenie) Type() string  { return "opsgenie" }
func (opsgenie) Label() string { return "Opsgenie" }
func (opsgenie) Fields() []alerts.Field {
	return []alerts.Field{
		{Key: "api_key", Label: "API key", Kind: "secret", Required: true, Help: "From Opsgenie: Settings, Integrations, API."},
		{Key: "region", Label: "Region", Kind: "select", Options: []string{"us", "eu"}, Default: "us"},
		{Key: "api_url", Label: "API URL", Kind: "text", Help: "Leave empty to use the region's address."},
	}
}
func (opsgenie) Validate(c alerts.Config) error {
	if c.Get("api_key") == "" {
		return fmt.Errorf("the API key is required")
	}
	switch c.Get("region") {
	case "", "us", "eu":
	default:
		return fmt.Errorf("the region must be us or eu")
	}
	if u := c.Get("api_url"); u != "" {
		return checkHTTPS(u, "API URL")
	}
	return nil
}
func (opsgenie) Send(ctx context.Context, d alerts.Deps, c alerts.Config, m alerts.Message) (alerts.Result, error) {
	base := strings.TrimRight(c.Get("api_url"), "/")
	if base == "" {
		base = "https://api.opsgenie.com"
		if c.Get("region") == "eu" {
			base = "https://api.eu.opsgenie.com"
		}
	}
	h := map[string]string{"Authorization": "GenieKey " + c.Get("api_key")}
	prio := map[string]string{"critical": "P1", "warning": "P3", "info": "P5"}
	for _, a := range m.Alerts {
		var code int
		var err error
		if m.Status == "resolved" {
			code, _, err = send(ctx, d.HTTP, "POST", base+"/v2/alerts/"+url.PathEscape(a.Fingerprint)+"/close?identifierType=alias", h, map[string]string{"source": "Lumen", "note": "Resolved in Lumen"})
		} else {
			code, _, err = send(ctx, d.HTTP, "POST", base+"/v2/alerts", h, map[string]any{"message": summary(a), "alias": a.Fingerprint, "description": details(a, m.Link), "priority": prio[a.Severity], "source": "Lumen", "details": a.Labels, "tags": []string{"lumen", a.Severity}})
		}
		if err != nil {
			return alerts.Result{}, err
		}
		if !ok2xx(code) {
			return alerts.Result{}, fmt.Errorf("Opsgenie answered HTTP %d", code)
		}
	}
	return alerts.Result{}, nil
}

// ---------------- Jira ----------------

type jira struct{}

func (jira) Type() string  { return "jira" }
func (jira) Label() string { return "Jira" }
func (jira) Fields() []alerts.Field {
	return []alerts.Field{
		{Key: "base_url", Label: "Jira address", Kind: "text", Required: true, Help: "For example https://yourcompany.atlassian.net"},
		{Key: "email", Label: "User (email)", Kind: "text", Required: true},
		{Key: "api_token", Label: "API token or password", Kind: "secret", Required: true, Help: "Jira Cloud: an API token from the Atlassian account."},
		{Key: "project_key", Label: "Project key", Kind: "text", Required: true, Help: "For example OPS."},
		{Key: "issue_type", Label: "Issue type", Kind: "text", Default: "Task"},
		{Key: "done_transition", Label: "Transition that closes a ticket", Kind: "text", Default: "Done", Help: "When an alert resolves, Lumen comments and moves the ticket with this transition, if it exists."},
	}
}
func (jira) Validate(c alerts.Config) error {
	if err := checkHTTPS(c.Get("base_url"), "Jira address"); err != nil {
		return err
	}
	for _, k := range []string{"email", "api_token", "project_key"} {
		if c.Get(k) == "" {
			return fmt.Errorf("%s is required", k)
		}
	}
	if strings.ContainsAny(c.Get("project_key"), " /\r\n") {
		return fmt.Errorf("the project key must be a short code such as OPS")
	}
	return nil
}
func (jira) Send(ctx context.Context, d alerts.Deps, c alerts.Config, m alerts.Message) (alerts.Result, error) {
	base := strings.TrimRight(c.Get("base_url"), "/")
	h := map[string]string{"Authorization": basic(c.Get("email"), c.Get("api_token"))}
	issueType := c.Get("issue_type")
	if issueType == "" {
		issueType = "Task"
	}
	res := alerts.Result{Refs: map[string]string{}}
	comment := func(key, text string) error {
		code, _, err := send(ctx, d.HTTP, "POST", base+"/rest/api/2/issue/"+url.PathEscape(key)+"/comment", h, map[string]string{"body": text})
		if err != nil {
			return err
		}
		if !ok2xx(code) {
			return fmt.Errorf("Jira answered HTTP %d when commenting on %s", code, key)
		}
		return nil
	}
	for _, a := range m.Alerts {
		switch {
		case m.Status == "firing" && a.Ref == "": // a new ticket
			code, body, err := send(ctx, d.HTTP, "POST", base+"/rest/api/2/issue", h, map[string]any{"fields": map[string]any{
				"project": map[string]string{"key": c.Get("project_key")}, "issuetype": map[string]string{"name": issueType},
				"summary": summary(a), "description": details(a, m.Link), "labels": []string{"lumen"}}})
			if err != nil {
				return res, err
			}
			if !ok2xx(code) {
				return res, fmt.Errorf("Jira answered HTTP %d when creating the ticket (check the project key and the issue type)", code)
			}
			var out struct{ Key string }
			if json.Unmarshal(body, &out) != nil || out.Key == "" {
				return res, fmt.Errorf("Jira did not return a ticket key")
			}
			res.Refs[a.Fingerprint] = out.Key
		case m.Status == "firing": // still firing: a comment on the ticket the alert already has
			if err := comment(a.Ref, "Still firing: "+labelsText(a.Labels)+fmt.Sprintf(" (value %.4g)", a.Value)); err != nil {
				return res, err
			}
		case a.Ref != "": // resolved: comment, then close the ticket if the workflow has the transition
			if err := comment(a.Ref, "Resolved in Lumen."); err != nil {
				return res, err
			}
			want := c.Get("done_transition")
			if want == "" {
				want = "Done"
			}
			code, body, err := send(ctx, d.HTTP, "GET", base+"/rest/api/2/issue/"+url.PathEscape(a.Ref)+"/transitions", h, nil)
			if err != nil {
				return res, err
			}
			if !ok2xx(code) {
				continue
			}
			var tr struct{ Transitions []struct{ ID, Name string } }
			_ = json.Unmarshal(body, &tr)
			for _, t := range tr.Transitions {
				if strings.EqualFold(t.Name, want) {
					if code, _, err := send(ctx, d.HTTP, "POST", base+"/rest/api/2/issue/"+url.PathEscape(a.Ref)+"/transitions", h, map[string]any{"transition": map[string]string{"id": t.ID}}); err != nil {
						return res, err
					} else if !ok2xx(code) {
						return res, fmt.Errorf("Jira refused to move %s to %q (HTTP %d)", a.Ref, want, code)
					}
					break
				}
			}
		}
	}
	return res, nil
}

// ---------------- ServiceNow ----------------

type serviceNow struct{}

func (serviceNow) Type() string  { return "servicenow" }
func (serviceNow) Label() string { return "ServiceNow" }
func (serviceNow) Fields() []alerts.Field {
	return []alerts.Field{
		{Key: "instance_url", Label: "Instance address", Kind: "text", Required: true, Help: "For example https://yourcompany.service-now.com"},
		{Key: "username", Label: "User name", Kind: "text", Required: true},
		{Key: "password", Label: "Password", Kind: "secret", Required: true},
	}
}
func (serviceNow) Validate(c alerts.Config) error {
	if err := checkHTTPS(c.Get("instance_url"), "Instance address"); err != nil {
		return err
	}
	if c.Get("username") == "" || c.Get("password") == "" {
		return fmt.Errorf("user name and password are required")
	}
	return nil
}
func (serviceNow) Send(ctx context.Context, d alerts.Deps, c alerts.Config, m alerts.Message) (alerts.Result, error) {
	base := strings.TrimRight(c.Get("instance_url"), "/") + "/api/now/table/incident"
	h := map[string]string{"Authorization": basic(c.Get("username"), c.Get("password"))}
	urgency := map[string]string{"critical": "1", "warning": "2", "info": "3"}
	res := alerts.Result{Refs: map[string]string{}}
	patch := func(id string, body map[string]string) error {
		code, _, err := send(ctx, d.HTTP, "PATCH", base+"/"+url.PathEscape(id), h, body)
		if err != nil {
			return err
		}
		if !ok2xx(code) {
			return fmt.Errorf("ServiceNow answered HTTP %d when updating the incident", code)
		}
		return nil
	}
	for _, a := range m.Alerts {
		switch {
		case m.Status == "firing" && a.Ref == "":
			code, body, err := send(ctx, d.HTTP, "POST", base, h, map[string]string{"short_description": summary(a), "description": details(a, m.Link), "urgency": urgency[a.Severity], "impact": urgency[a.Severity], "correlation_id": a.Fingerprint})
			if err != nil {
				return res, err
			}
			if !ok2xx(code) {
				return res, fmt.Errorf("ServiceNow answered HTTP %d when creating the incident", code)
			}
			var out struct{ Result struct{ Sys_id string } }
			if json.Unmarshal(body, &out) != nil || out.Result.Sys_id == "" {
				return res, fmt.Errorf("ServiceNow did not return an incident")
			}
			res.Refs[a.Fingerprint] = out.Result.Sys_id
		case m.Status == "firing":
			if err := patch(a.Ref, map[string]string{"work_notes": "Still firing: " + labelsText(a.Labels)}); err != nil {
				return res, err
			}
		case a.Ref != "":
			if err := patch(a.Ref, map[string]string{"state": "6", "close_code": "Solved (Permanently)", "close_notes": "Resolved in Lumen"}); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}
