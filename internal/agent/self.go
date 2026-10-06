package agent

import (
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is set at build time: -ldflags "-X github.com/danielingemar/lumen/internal/agent.Version=1.2.3"
var Version = "dev"

// SelfService is the service.name the agent reports its own metrics under.
const SelfService = "lumen-agent"

// Self tracks the agent's own health. The same numbers are (a) included in the regular
// push to Lumen, so a silent or failing agent is visible, and (b) served at /metrics in
// Prometheus format for external scrapers.
type Self struct {
	selfUpdate  string // what this agent can do about updating itself ("systemd", "exec" or "")
	mu          sync.Mutex
	start       time.Time
	host        string
	sent        map[string]float64 // signal -> items sent
	sendErrors  map[string]float64 // signal -> failed batches
	scrapes     map[string]float64 // target -> scrapes
	scrapeErrs  map[string]float64 // target -> failed scrapes
	lastSuccess map[string]time.Time
	rejected    int
	lumenURL    string
	ipAt        time.Time
	ip          string
	ips         []string
}

// SetLumenURL tells the agent which server it reports to, so it can work out which of its addresses reaches it.
func (s *Self) SetLumenURL(u string) {
	s.mu.Lock()
	s.lumenURL = u
	s.ipAt = time.Time{}
	s.mu.Unlock()
}

// SetRejected records how many server-supplied log paths were refused by the local allow-list.
func (s *Self) SetRejected(n int) { s.mu.Lock(); s.rejected = n; s.mu.Unlock() }

func NewSelf(host string) *Self {
	return &Self{start: time.Now(), host: host, sent: map[string]float64{}, sendErrors: map[string]float64{},
		scrapes: map[string]float64{}, scrapeErrs: map[string]float64{}, lastSuccess: map[string]time.Time{}}
}

// Sent records a successfully delivered batch of n items for a signal ("metrics" or "logs").
func (s *Self) Sent(signal string, n int) {
	s.mu.Lock()
	s.sent[signal] += float64(n)
	s.lastSuccess[signal] = time.Now()
	s.mu.Unlock()
}

// SendError records a batch that could not be delivered after all retries.
func (s *Self) SendError(signal string) {
	s.mu.Lock()
	s.sendErrors[signal]++
	s.mu.Unlock()
}

// ObserveScrape records one scrape attempt. The target label is host+path only, so
// credentials or tokens in the URL are never exported.
func (s *Self) ObserveScrape(rawURL string, err error) {
	t := rawURL
	if u, perr := url.Parse(rawURL); perr == nil && u.Host != "" {
		t = u.Host + u.Path
	}
	s.mu.Lock()
	s.scrapes[t]++
	if err != nil {
		s.scrapeErrs[t]++
	}
	s.mu.Unlock()
}

// Collect returns the agent's own metrics as points for the given service name.
func (s *Self) Collect(service string, now int64) []Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Point
	add := func(name, typ string, v float64, a map[string]string) {
		out = append(out, Point{Service: service, Name: name, Type: typ, Value: v, Attrs: a, TimeNs: now})
	}
	if time.Since(s.ipAt) > 5*time.Minute { // addresses rarely change; looking them up costs a route lookup
		s.ip, s.ips = LocalIPs(s.lumenURL)
		s.ipAt = time.Now()
	}
	info := map[string]string{"version": Version, "host": s.host, "os": runtime.GOOS + "/" + runtime.GOARCH}
	if s.selfUpdate != "" {
		info["self_update"] = s.selfUpdate
	}
	if s.ip != "" {
		info["ip"] = s.ip
		info["ips"] = strings.Join(s.ips, ",")
	}
	add("lumen_agent_info", "gauge", 1, info)
	add("lumen_agent_uptime_seconds", "gauge", time.Since(s.start).Seconds(), nil)
	add("lumen_agent_log_paths_rejected", "gauge", float64(s.rejected), nil)
	for sig, v := range s.sent {
		add("lumen_agent_items_sent_total", "sum", v, map[string]string{"signal": sig})
	}
	for sig, v := range s.sendErrors {
		add("lumen_agent_send_errors_total", "sum", v, map[string]string{"signal": sig})
	}
	for sig, t := range s.lastSuccess {
		add("lumen_agent_last_successful_send_timestamp_seconds", "gauge", float64(t.Unix()), map[string]string{"signal": sig})
	}
	for t, v := range s.scrapes {
		add("lumen_agent_scrapes_total", "sum", v, map[string]string{"target": t})
	}
	for t, v := range s.scrapeErrs {
		add("lumen_agent_scrape_errors_total", "sum", v, map[string]string{"target": t})
	}
	return out
}

func escLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// WritePrometheus renders points in the Prometheus text exposition format.
func WritePrometheus(w http.ResponseWriter, pts []Point) {
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].Name < pts[j].Name })
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	last := ""
	for _, p := range pts {
		if p.Name != last {
			typ := "gauge"
			if p.Type == "sum" {
				typ = "counter"
			}
			fmt.Fprintf(w, "# TYPE %s %s\n", p.Name, typ)
			last = p.Name
		}
		keys := make([]string, 0, len(p.Attrs))
		for k := range p.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lbl := make([]string, 0, len(keys))
		for _, k := range keys {
			lbl = append(lbl, fmt.Sprintf(`%s="%s"`, k, escLabel(p.Attrs[k])))
		}
		if len(lbl) > 0 {
			fmt.Fprintf(w, "%s{%s} %g\n", p.Name, strings.Join(lbl, ","), p.Value)
		} else {
			fmt.Fprintf(w, "%s %g\n", p.Name, p.Value)
		}
	}
}

// Handler serves /metrics (and /healthz) for external Prometheus-style scrapers.
func (s *Self) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		WritePrometheus(w, s.Collect(SelfService, time.Now().UnixNano()))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return mux
}

// SetSelfUpdate says how this agent can update itself, so that the Lumen UI knows whether to offer a one-click update.
func (s *Self) SetSelfUpdate(mode string) {
	s.mu.Lock()
	s.selfUpdate = mode
	s.mu.Unlock()
}
