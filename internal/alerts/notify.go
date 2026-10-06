package alerts

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Field describes one setting of a notifier, so the interface can build a form without knowing the notifier.
type Field struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Help     string   `json:"help,omitempty"`
	Kind     string   `json:"kind"` // text | secret | select | number
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
}

// Config is a channel's settings. Secrets are stored encrypted and never shown again.
type Config struct {
	Settings map[string]string
	Secrets  map[string]string
}

func (c Config) Get(k string) string {
	if v, ok := c.Settings[k]; ok {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(c.Secrets[k])
}

// AlertView is an alert as a notifier sees it.
type AlertView struct {
	Fingerprint string            `json:"fingerprint"`
	RuleName    string            `json:"rule"`
	Severity    string            `json:"severity"`
	State       string            `json:"state"`
	Labels      map[string]string `json:"labels"`
	Value       float64           `json:"value"`
	Since       time.Time         `json:"since"`
	Annotation  string            `json:"annotation,omitempty"`
	Ref         string            `json:"-"` // this channel's reference for the alert in an external system, from an earlier message
}

// Message is what a channel is asked to send: a group of alerts that fired, or that resolved.
type Message struct {
	Tenant   string      `json:"tenant"`
	Channel  string      `json:"channel"`
	Status   string      `json:"status"` // firing | resolved
	GroupKey string      `json:"group_key"`
	Title    string      `json:"title"`
	Body     string      `json:"-"`
	Link     string      `json:"link,omitempty"`
	Alerts   []AlertView `json:"alerts"`
}

// Result lets a notifier report references (a ticket key) that the engine stores and gives back on the resolve message.
type Result struct {
	Refs map[string]string // alert fingerprint -> reference
}

// Deps is what a notifier uses to reach the network. Both go through the guard that refuses internal addresses.
type Deps struct {
	HTTP *http.Client
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Now  func() time.Time
}

// Notifier delivers messages to one kind of destination. Enterprise registers more of these.
type Notifier interface {
	Type() string
	Label() string
	Fields() []Field
	// Validate checks the settings (secrets included) before a channel is saved.
	Validate(cfg Config) error
	Send(ctx context.Context, d Deps, cfg Config, m Message) (Result, error)
}

// Edition is implemented by notifiers that need a licence ("enterprise"); all others belong to Community.
type Edition interface{ Edition() string }

// EditionOf is the edition a notifier belongs to.
func EditionOf(n Notifier) string {
	if e, ok := n.(Edition); ok {
		return e.Edition()
	}
	return "community"
}

// Licenser says whether the features of an edition may be used now. The licence manager implements it.
type Licenser interface{ Allows(edition string) bool }

// Periodic is implemented by notifiers that also do something on a timer (the heartbeat).
type Periodic interface {
	Every(cfg Config) time.Duration
	Tick(ctx context.Context, d Deps, cfg Config) error
}

// Registry holds the known notifier types.
type Registry struct {
	mu    sync.RWMutex
	m     map[string]Notifier
	order []string
}

func NewRegistry() *Registry { return &Registry{m: map[string]Notifier{}} }

// Register adds (or replaces) a notifier type.
func (r *Registry) Register(n Notifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[n.Type()]; !ok {
		r.order = append(r.order, n.Type())
	}
	r.m[n.Type()] = n
}

func (r *Registry) Get(t string) (Notifier, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.m[t]
	return n, ok
}

// Types lists the notifiers in the order they were registered.
func (r *Registry) Types() []Notifier {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Notifier, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, r.m[t])
	}
	return out
}

// Default is the registry used by the engine. The core registers its notifiers here; Enterprise adds to it.
var Default = NewRegistry()

func init() { RegisterCore(Default) }

func oneLine(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(s)
	return strings.TrimSpace(s)
}

func labelText(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+oneLine(l[k]))
	}
	return strings.Join(parts, ", ")
}

// Compose builds the title and the plain-text body of a message from its alerts.
func Compose(status string, alerts []AlertView, link string) (title, body string) {
	tag := strings.ToUpper(status)
	if len(alerts) == 0 {
		return "[" + tag + "]", ""
	}
	title = fmt.Sprintf("[%s] %s", tag, oneLine(alerts[0].RuleName))
	if len(alerts) > 1 {
		title += fmt.Sprintf(" (%d alerts)", len(alerts))
	}
	var b strings.Builder
	for _, a := range alerts {
		fmt.Fprintf(&b, "- %s: %s", a.Severity, labelText(a.Labels))
		if status == "firing" {
			fmt.Fprintf(&b, " (value %s, since %s)", trimFloat(a.Value), a.Since.UTC().Format("15:04 UTC"))
		}
		if a.Annotation != "" {
			fmt.Fprintf(&b, "\n  %s", oneLine(a.Annotation))
		}
		b.WriteString("\n")
	}
	if link != "" {
		fmt.Fprintf(&b, "\n%s\n", link)
	}
	return title, b.String()
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.4g", v)
	return s
}
