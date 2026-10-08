// Package alerts evaluates alert rules, keeps the state of alerts and delivers notifications.
//
// The core (this package) ships email, webhook, Slack, Teams and heartbeat. Enterprise adds ticket and on-call
// integrations by registering more Notifiers; this package never imports Enterprise code.
package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/danielingemar/lumen/internal/registry"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	KindMetric = "metric" // a metric over a window compared with a threshold
	KindStatus = "status" // host, instance, service or container is down
	KindLog    = "log"    // number of log lines over a window

	MaxRulesPerTenant    = 200
	MaxChannelsPerTenant = 50
	MaxSilencesPerTenant = 200
)

// Filter limits a metric to series with a label value.
type Filter struct {
	K string `json:"k"`
	V string `json:"v"`
}

// Rule is a condition, how often to check it, and how long it must hold.
type Rule struct {
	ID          string            `json:"id"`
	Tenant      string            `json:"tenant,omitempty"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Enabled     bool              `json:"enabled"`
	IntervalSec int               `json:"interval_sec"` // how often to evaluate (15..86400, default 60)
	ForSec      int               `json:"for_sec"`      // the condition must hold this long before the alert fires
	Severity    string            `json:"severity"`     // critical | warning | info
	Labels      map[string]string `json:"labels"`
	Annotation  string            `json:"annotation"`
	NoData      string            `json:"no_data"`         // ok | alert | keep: what to do when the query returns nothing
	Group       string            `json:"group,omitempty"` // only the hosts of this host group (empty: all hosts)

	// metric
	Metric    string   `json:"metric,omitempty"`
	Reduce    string   `json:"reduce,omitempty"` // avg | max | min | last | sum over the window
	Filters   []Filter `json:"filters,omitempty"`
	GroupBy   string   `json:"group_by,omitempty"` // one alert per value of this label
	WindowSec int      `json:"window_sec,omitempty"`
	Op        string   `json:"op,omitempty"`
	Threshold float64  `json:"threshold"`

	// status
	Status string `json:"status,omitempty"` // host_down | instance_down | service_down | container_down

	// log (uses WindowSec, Op, Threshold as a line count)
	Service     string `json:"service,omitempty"`
	Host        string `json:"host,omitempty"`
	LogSeverity string `json:"log_severity,omitempty"`
	Contains    string `json:"contains,omitempty"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

var (
	ops        = map[string]bool{">": true, ">=": true, "<": true, "<=": true, "==": true, "!=": true}
	reduces    = map[string]bool{"avg": true, "max": true, "min": true, "last": true, "sum": true}
	statuses   = map[string]bool{"host_down": true, "instance_down": true, "service_down": true, "container_down": true, "lumen_disk": true, "lumen_elasticsearch": true, "lumen_clickhouse": true}
	severities = map[string]bool{"critical": true, "warning": true, "info": true}
	labelKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,39}$`)
)

func clean(s string, max int) bool {
	return len(s) <= max && !strings.ContainsAny(s, "\r\n\x00\t")
}

// ValidLabels checks a label map (rule labels, silence matchers, channel matchers).
func ValidLabels(m map[string]string, what string) error {
	if len(m) > 10 {
		return fmt.Errorf("at most 10 %s", what)
	}
	for k, v := range m {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("%s: %q is not a valid label name (letters, digits, _ . -, at most 40)", what, k)
		}
		if !clean(v, 100) {
			return fmt.Errorf("%s: the value of %q is too long or has control characters", what, k)
		}
	}
	return nil
}

// Normalize fills in defaults and validates. It changes the rule in place.
func (r *Rule) Normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" || !clean(r.Name, 80) {
		return fmt.Errorf("the name must be 1-80 characters on one line")
	}
	if r.Group != "" {
		g, err := registry.NormalizeGroup(r.Group)
		if err != nil {
			return fmt.Errorf("the host group: %v", strings.TrimPrefix(err.Error(), "invalid: "))
		}
		r.Group = g
	}
	if r.Severity == "" {
		r.Severity = "warning"
	}
	if !severities[r.Severity] {
		return fmt.Errorf("severity must be critical, warning or info")
	}
	if r.IntervalSec == 0 {
		r.IntervalSec = 60
	}
	if r.IntervalSec < 15 || r.IntervalSec > 86400 {
		return fmt.Errorf("the interval must be between 15 seconds and 24 hours")
	}
	if r.ForSec < 0 || r.ForSec > 86400 {
		return fmt.Errorf("\"for\" must be between 0 and 24 hours")
	}
	if r.NoData == "" {
		r.NoData = "ok"
	}
	if r.NoData != "ok" && r.NoData != "alert" && r.NoData != "keep" {
		return fmt.Errorf("no_data must be ok, alert or keep")
	}
	if len(r.Annotation) > 500 || strings.ContainsAny(r.Annotation, "\x00") {
		return fmt.Errorf("the annotation is too long (500 characters)")
	}
	if r.Labels == nil {
		r.Labels = map[string]string{}
	}
	if err := ValidLabels(r.Labels, "labels"); err != nil {
		return err
	}
	switch r.Kind {
	case KindMetric:
		if r.Metric == "" || !clean(r.Metric, 200) {
			return fmt.Errorf("choose a metric")
		}
		if r.Reduce == "" {
			r.Reduce = "avg"
		}
		if !reduces[r.Reduce] {
			return fmt.Errorf("reduce must be avg, max, min, last or sum")
		}
		if len(r.Filters) > 5 {
			return fmt.Errorf("at most 5 filters")
		}
		for _, f := range r.Filters {
			if !labelKeyRe.MatchString(f.K) || !clean(f.V, 100) {
				return fmt.Errorf("a filter must be label = value")
			}
		}
		if r.GroupBy != "" && !labelKeyRe.MatchString(r.GroupBy) {
			return fmt.Errorf("group by must be a label name")
		}
	case KindStatus:
		if !statuses[r.Status] {
			return fmt.Errorf("status must be host_down, instance_down, service_down, container_down, lumen_disk, lumen_elasticsearch or lumen_clickhouse")
		}
		// the rules about Lumen itself use the threshold: how full a disk may be (percent), or how bad Elasticsearch may be (1 = not green, 2 = red)
		switch r.Status {
		case "lumen_disk", "lumen_clickhouse":
			if r.Threshold == 0 {
				r.Threshold = map[string]float64{"lumen_disk": 80, "lumen_clickhouse": 90}[r.Status]
			}
			if r.Threshold < 1 || r.Threshold > 100 {
				return fmt.Errorf("the disk limit is a percentage between 1 and 100")
			}
		case "lumen_elasticsearch":
			if r.Threshold == 0 {
				r.Threshold = 1
			}
			if r.Threshold != 1 && r.Threshold != 2 {
				return fmt.Errorf("for Elasticsearch the limit is 1 (not green) or 2 (red)")
			}
		}
		r.NoData = "ok"
	case KindLog:
		if r.GroupBy != "" && r.GroupBy != "severity" && r.GroupBy != "service" {
			return fmt.Errorf("for log rules, group by severity or service")
		}
		for _, s := range []string{r.Service, r.Host, r.LogSeverity, r.Contains} {
			if !clean(s, 200) {
				return fmt.Errorf("a log filter is too long or has control characters")
			}
		}
	default:
		return fmt.Errorf("kind must be metric, status or log")
	}
	if r.Kind != KindStatus {
		if r.WindowSec == 0 {
			r.WindowSec = 300
		}
		if r.WindowSec < 60 || r.WindowSec > 86400 {
			return fmt.Errorf("the window must be between 1 minute and 24 hours")
		}
		if !ops[r.Op] {
			return fmt.Errorf("the comparison must be one of > >= < <= == !=")
		}
		if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
			return fmt.Errorf("the threshold must be a number")
		}
	}
	return nil
}

// Compare applies a rule's comparison.
func Compare(v float64, op string, t float64) bool {
	switch op {
	case ">":
		return v > t
	case ">=":
		return v >= t
	case "<":
		return v < t
	case "<=":
		return v <= t
	case "==":
		return v == t
	case "!=":
		return v != t
	}
	return false
}

// Reduce combines the points of a window into one value; NaN points are ignored.
func Reduce(points [][2]float64, mode string) (float64, bool) {
	var n int
	var sum, mx, mn, last float64
	for _, p := range points {
		v := p[1]
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if n == 0 || v > mx {
			mx = v
		}
		if n == 0 || v < mn {
			mn = v
		}
		sum += v
		last = v
		n++
	}
	if n == 0 {
		return 0, false
	}
	switch mode {
	case "max":
		return mx, true
	case "min":
		return mn, true
	case "last":
		return last, true
	case "sum":
		return sum, true
	}
	return sum / float64(n), true
}

// Fingerprint identifies one alert of a rule: the rule and the labels of the series.
func Fingerprint(ruleID string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(ruleID))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k + "=" + labels[k]))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// Alert is one firing (or pending) instance of a rule for one set of labels.
type Alert struct {
	Fingerprint  string            `json:"fingerprint"`
	RuleID       string            `json:"rule_id"`
	Tenant       string            `json:"tenant,omitempty"`
	RuleName     string            `json:"rule_name"`
	Severity     string            `json:"severity"`
	Labels       map[string]string `json:"labels"`
	Annotation   string            `json:"annotation,omitempty"`
	State        string            `json:"state"` // pending | firing
	Since        time.Time         `json:"since"`
	FiringSince  time.Time         `json:"firing_since,omitempty"`
	Value        float64           `json:"value"`
	LastEval     time.Time         `json:"last_eval"`
	OKStreak     int               `json:"ok_streak,omitempty"`
	Acked        bool              `json:"acked,omitempty"`
	AckedBy      string            `json:"acked_by,omitempty"`
	AckedAt      time.Time         `json:"acked_at,omitempty"`
	LastNotified time.Time         `json:"last_notified,omitempty"`
	Refs         map[string]string `json:"refs,omitempty"` // channel id -> reference in an external system (a ticket key)
}

// MatchLabels are the labels silences and channel matchers see: the alert's labels plus alertname and severity.
func (a Alert) MatchLabels() map[string]string {
	m := map[string]string{"alertname": a.RuleName, "severity": a.Severity}
	for k, v := range a.Labels {
		m[k] = v
	}
	return m
}

// Silence mutes alerts whose labels match every matcher, until End.
type Silence struct {
	ID        string            `json:"id"`
	Tenant    string            `json:"tenant,omitempty"`
	Matchers  map[string]string `json:"matchers"`
	Start     time.Time         `json:"start"`
	End       time.Time         `json:"end"`
	Reason    string            `json:"reason"`
	CreatedBy string            `json:"created_by"`
}

func (s Silence) Active(now time.Time) bool { return !now.Before(s.Start) && now.Before(s.End) }

// Matches reports whether every matcher equals the alert's label ("*" matches any value that exists).
func Matches(matchers, labels map[string]string) bool {
	if len(matchers) == 0 {
		return false
	}
	for k, want := range matchers {
		got, ok := labels[k]
		if !ok || (want != "*" && got != want) {
			return false
		}
	}
	return true
}

// Event is an entry in the history of alerts.
type Event struct {
	ID       string            `json:"id"`
	Tenant   string            `json:"tenant,omitempty"`
	Time     time.Time         `json:"time"`
	Type     string            `json:"type"` // firing | resolved | ack | silence
	RuleName string            `json:"rule_name"`
	Severity string            `json:"severity"`
	Labels   map[string]string `json:"labels"`
	Value    float64           `json:"value"`
	By       string            `json:"by,omitempty"`
}
