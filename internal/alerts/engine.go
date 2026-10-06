package alerts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielingemar/lumen/internal/docstore"
	"github.com/danielingemar/lumen/internal/secretbox"
)

const (
	collRules    = "alert_rules"
	collState    = "alert_state"
	collEvents   = "alert_events"
	collSilences = "alert_silences"
	collChannels = "alert_channels"
)

// Collections lists the document collections the engine uses (the backup includes them).
var Collections = []string{collRules, collState, collEvents, collSilences, collChannels}

var ErrInvalid = errors.New("invalid")
var ErrNotFound = errors.New("not found")

func invalid(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(f, a...)) }

func newID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("no randomness available")
	}
	return prefix + hex.EncodeToString(b)
}

// Channel is a destination for messages. Secrets are stored encrypted and never returned.
type Channel struct {
	ID         string            `json:"id"`
	Tenant     string            `json:"tenant,omitempty"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Enabled    bool              `json:"enabled"`
	Settings   map[string]string `json:"settings"`
	SecretsEnc map[string]string `json:"secrets_enc,omitempty"`
	Severities []string          `json:"severities"` // empty = all
	Match      map[string]string `json:"match"`      // label matchers, all must match; empty = all alerts
	Created    time.Time         `json:"created"`
	Updated    time.Time         `json:"updated"`
}

// Health tells whether a channel has been working lately.
type Health struct {
	LastOK    time.Time `json:"last_ok,omitempty"`
	LastError time.Time `json:"last_error,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// ChannelOut is a channel as the interface sees it: no secrets, only whether they are set.
type ChannelOut struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	TypeLabel  string            `json:"type_label"`
	Enabled    bool              `json:"enabled"`
	Settings   map[string]string `json:"settings"`
	HasSecret  map[string]bool   `json:"has_secret"`
	Severities []string          `json:"severities"`
	Match      map[string]string `json:"match"`
	Health     Health            `json:"health"`
}

// ChannelIn is a create or update request. An empty secret keeps the stored one; ClearSecrets removes secrets.
type ChannelIn struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Enabled      bool              `json:"enabled"`
	Settings     map[string]string `json:"settings"`
	Secrets      map[string]string `json:"secrets"`
	ClearSecrets []string          `json:"clear_secrets"`
	Severities   []string          `json:"severities"`
	Match        map[string]string `json:"match"`
}

// Delivery is one attempt to deliver a message, for the delivery log.
type Delivery struct {
	Time      time.Time `json:"time"`
	Tenant    string    `json:"tenant,omitempty"`
	ChannelID string    `json:"channel_id"`
	Channel   string    `json:"channel"`
	Status    string    `json:"status"` // firing | resolved | test
	Alerts    int       `json:"alerts"`
	OK        bool      `json:"ok"`
	Attempt   int       `json:"attempt"`
	GaveUp    bool      `json:"gave_up,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// AlertOut is an alert with its silence status.
type AlertOut struct {
	Alert
	Silenced  bool   `json:"silenced"`
	SilenceID string `json:"silence_id,omitempty"`
}

type group struct {
	ch      Channel
	ruleID  string
	status  string
	alerts  map[string]Alert
	readyAt time.Time
}

type delivery struct {
	ch      Channel
	msg     Message
	attempt int
	due     time.Time
}

// Engine evaluates rules, keeps alert state and delivers notifications. Use Run for a server; the tests call
// EvalDue and Dispatch directly with a fake clock.
type Engine struct {
	DB           docstore.Backend
	Box          *secretbox.Box
	Eval         *Evaluator
	Reg          *Registry
	Guard        Guard
	PublicURL    string
	Log          *slog.Logger
	Now          func() time.Time
	GroupWait    time.Duration   // how long related alerts are collected into one message (default 30 s)
	RepeatEvery  time.Duration   // how often a firing alert is announced again (default 4 h)
	RecoverAfter int             // calm evaluations before an alert resolves (default 2)
	Backoff      []time.Duration // waits before each retry of a failed delivery

	mu         sync.Mutex
	loaded     bool
	rules      map[string]Rule
	channels   map[string]Channel
	silences   map[string]Silence
	alerts     map[string]Alert // tenant|fingerprint
	nextDue    map[string]time.Time
	lastErr    map[string]string
	queued     map[string]time.Time
	groups     map[string]*group
	retry      []*delivery
	health     map[string]Health
	deliveries []Delivery
	hbNext     map[string]time.Time
	refreshed  time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func (e *Engine) reg() *Registry {
	if e.Reg != nil {
		return e.Reg
	}
	return Default
}
func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}
func (e *Engine) groupWait() time.Duration {
	if e.GroupWait > 0 {
		return e.GroupWait
	}
	return 30 * time.Second
}
func (e *Engine) repeatEvery() time.Duration {
	if e.RepeatEvery > 0 {
		return e.RepeatEvery
	}
	return 4 * time.Hour
}
func (e *Engine) backoff() []time.Duration {
	if e.Backoff != nil {
		return e.Backoff
	}
	return []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
}
func (e *Engine) deps() Deps {
	return Deps{HTTP: e.Guard.Client(15 * time.Second), Dial: e.Guard.Dial, Now: e.now}
}

func ctx10() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func akey(tenant, fp string) string { return tenant + "|" + fp }

// ---- loading and saving ----

func listAll[T any](db docstore.Backend, coll string) ([]T, error) {
	c, cancel := ctx10()
	defer cancel()
	docs, err := db.List(c, coll, nil, 10000)
	if err != nil {
		return nil, err
	}
	var out []T
	for _, d := range docs {
		var v T
		if d.Decode(&v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

func (e *Engine) put(coll, id string, v any) {
	c, cancel := ctx10()
	defer cancel()
	if err := e.DB.Put(c, coll, id, v, ""); err != nil {
		e.log().Error("alerts: could not save", "collection", coll, "err", err)
	}
}

func (e *Engine) del(coll, id string) {
	c, cancel := ctx10()
	defer cancel()
	if err := e.DB.Delete(c, coll, id); err != nil && !errors.Is(err, docstore.ErrNotFound) {
		e.log().Error("alerts: could not delete", "collection", coll, "err", err)
	}
}

// Load reads rules, channels, silences and the state of alerts from the document store. It is safe to call again.
func (e *Engine) Load() error {
	rules, err := listAll[Rule](e.DB, collRules)
	if err != nil {
		return err
	}
	chans, err := listAll[Channel](e.DB, collChannels)
	if err != nil {
		return err
	}
	sils, err := listAll[Silence](e.DB, collSilences)
	if err != nil {
		return err
	}
	st, err := listAll[Alert](e.DB, collState)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules, e.channels, e.silences, e.alerts = map[string]Rule{}, map[string]Channel{}, map[string]Silence{}, map[string]Alert{}
	for _, r := range rules {
		e.rules[r.ID] = r
	}
	for _, c := range chans {
		e.channels[c.ID] = c
	}
	for _, s := range sils {
		e.silences[s.ID] = s
	}
	for _, a := range st {
		e.alerts[akey(a.Tenant, a.Fingerprint)] = a
	}
	if e.nextDue == nil {
		e.nextDue, e.lastErr, e.queued, e.groups, e.health, e.hbNext = map[string]time.Time{}, map[string]string{}, map[string]time.Time{}, map[string]*group{}, map[string]Health{}, map[string]time.Time{}
	}
	e.loaded, e.refreshed = true, e.now()
	return nil
}

func (e *Engine) ensureLoaded() {
	e.mu.Lock()
	l, age := e.loaded, e.now().Sub(e.refreshed)
	e.mu.Unlock()
	if !l || age > 30*time.Second {
		if err := e.Load(); err != nil {
			e.log().Error("alerts: could not load", "err", err)
		}
	}
}

// ---- silences ----

func (e *Engine) silencedBy(a Alert, now time.Time) (Silence, bool) {
	for _, s := range e.silences {
		if s.Tenant == a.Tenant && s.Active(now) && Matches(s.Matchers, a.MatchLabels()) {
			return s, true
		}
	}
	return Silence{}, false
}

// ---- history ----

func (e *Engine) history(typ string, a Alert, by string) {
	ev := Event{ID: newID("e_"), Tenant: a.Tenant, Time: e.now().UTC(), Type: typ, RuleName: a.RuleName, Severity: a.Severity, Labels: a.Labels, Value: a.Value, By: by}
	e.put(collEvents, ev.ID, ev)
}

// History returns the most recent events of a tenant, newest first.
func (e *Engine) History(tenant string, limit int) []Event {
	c, cancel := ctx10()
	defer cancel()
	docs, _ := e.DB.List(c, collEvents, map[string]string{"tenant": tenant}, 5000)
	var out []Event
	for _, d := range docs {
		var ev Event
		if d.Decode(&ev) == nil {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Prune keeps the newest keep events per tenant.
func (e *Engine) Prune(keep int) {
	c, cancel := ctx10()
	defer cancel()
	docs, _ := e.DB.List(c, collEvents, nil, 20000)
	byTenant := map[string][]Event{}
	for _, d := range docs {
		var ev Event
		if d.Decode(&ev) == nil {
			byTenant[ev.Tenant] = append(byTenant[ev.Tenant], ev)
		}
	}
	for _, evs := range byTenant {
		sort.Slice(evs, func(i, j int) bool { return evs[i].Time.After(evs[j].Time) })
		for i := keep; i < len(evs); i++ {
			e.del(collEvents, evs[i].ID)
		}
	}
}

// ---- evaluation ----

// EvalDue evaluates every rule that is due. A rule that cannot be evaluated keeps its alerts as they are.
func (e *Engine) EvalDue(ctx context.Context) {
	e.ensureLoaded()
	now := e.now()
	e.mu.Lock()
	var due []Rule
	for _, r := range e.rules {
		if r.Enabled && !e.nextDue[r.ID].After(now) {
			due = append(due, r)
		}
	}
	e.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	type result struct {
		rule    Rule
		samples []Sample
		noData  bool
		err     error
	}
	results := make([]result, len(due))
	for i, r := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r Rule) {
			defer wg.Done()
			defer func() { <-sem }()
			c, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			s, nd, err := e.Eval.Eval(c, r, now)
			results[i] = result{r, s, nd, err}
		}(i, r)
	}
	wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, res := range results {
		r := res.rule
		if cur, ok := e.rules[r.ID]; !ok || !cur.Enabled {
			continue // deleted or switched off while it was being evaluated
		}
		e.nextDue[r.ID] = now.Add(time.Duration(r.IntervalSec) * time.Second)
		if res.err != nil {
			if e.lastErr[r.ID] != res.err.Error() {
				e.log().Warn("alerts: rule could not be evaluated", "rule", r.Name, "err", res.err)
			}
			e.lastErr[r.ID] = res.err.Error()
			continue
		}
		delete(e.lastErr, r.ID)
		e.applyLocked(r, res.samples, res.noData, now)
	}
	e.repeatsLocked(now)
}

func (e *Engine) applyLocked(r Rule, samples []Sample, noData bool, now time.Time) {
	prev := map[string]Alert{}
	for k, a := range e.alerts {
		if a.RuleID == r.ID {
			prev[a.Fingerprint] = a
			_ = k
		}
	}
	next, trs := Step(r, prev, samples, noData, now, e.RecoverAfter)
	for fp := range prev {
		if _, ok := next[fp]; !ok {
			delete(e.alerts, akey(r.Tenant, fp))
			e.del(collState, akey(r.Tenant, fp))
		}
	}
	for fp, a := range next {
		old, had := prev[fp]
		// carry what the engine owns across evaluations
		if had {
			a.Acked, a.AckedBy, a.AckedAt, a.LastNotified, a.Refs = old.Acked, old.AckedBy, old.AckedAt, old.LastNotified, old.Refs
		}
		e.alerts[akey(r.Tenant, fp)] = a
		e.put(collState, akey(r.Tenant, fp), a)
	}
	for _, t := range trs {
		a := t.Alert
		if cur, ok := prev[a.Fingerprint]; ok { // the snapshot must carry the engine's fields (last notified, ticket references)
			a.LastNotified, a.Refs, a.Acked = cur.LastNotified, cur.Refs, cur.Acked
		}
		e.history(t.Type, a, "")
		e.enqueueLocked(t.Type, a, now)
	}
}

// repeatsLocked announces firing alerts that nobody has been told about (not yet notified, or a delivery gave up)
// and repeats the announcement of long-running ones.
func (e *Engine) repeatsLocked(now time.Time) {
	for k, a := range e.alerts {
		if a.State != "firing" || a.Acked {
			continue
		}
		if _, silenced := e.silencedBy(a, now); silenced {
			continue
		}
		last := e.queued[k]
		if a.LastNotified.After(last) {
			last = a.LastNotified
		}
		if last.IsZero() {
			last = a.FiringSince
			if a.LastNotified.IsZero() {
				e.enqueueLocked("firing", a, now) // never announced: do it now (for example after a restart, or when a silence ended)
				continue
			}
		}
		if now.Sub(last) >= e.repeatEvery() {
			e.enqueueLocked("repeat", a, now)
		}
	}
}

func matchAll(m, l map[string]string) bool {
	for k, want := range m {
		got, ok := l[k]
		if !ok || (want != "*" && got != want) {
			return false
		}
	}
	return true
}

func (c Channel) accepts(a Alert) bool {
	if len(c.Severities) > 0 {
		ok := false
		for _, s := range c.Severities {
			ok = ok || s == a.Severity
		}
		if !ok {
			return false
		}
	}
	return matchAll(c.Match, a.MatchLabels())
}

// enqueueLocked puts an alert event into the groups of the channels that want it.
func (e *Engine) enqueueLocked(typ string, a Alert, now time.Time) {
	status := "firing"
	if typ == "resolved" {
		status = "resolved"
		if a.LastNotified.IsZero() {
			return // nobody heard about it firing, so nobody needs to hear that it is over
		}
	}
	if _, silenced := e.silencedBy(a, now); silenced {
		return
	}
	k := akey(a.Tenant, a.Fingerprint)
	if status == "firing" {
		e.queued[k] = now
	}
	for _, ch := range e.channels {
		if ch.Tenant != a.Tenant || !ch.Enabled || !ch.accepts(a) {
			continue
		}
		if n, ok := e.reg().Get(ch.Type); !ok || n.Type() == "heartbeat" {
			continue
		}
		gk := ch.ID + "|" + a.RuleID + "|" + status
		g := e.groups[gk]
		if g == nil {
			g = &group{ch: ch, ruleID: a.RuleID, status: status, alerts: map[string]Alert{}, readyAt: now.Add(e.groupWait())}
			e.groups[gk] = g
		}
		g.alerts[a.Fingerprint] = a
	}
}

// ---- delivery ----

func (e *Engine) config(ch Channel) (Config, error) {
	cfg := Config{Settings: ch.Settings, Secrets: map[string]string{}}
	for k, sealed := range ch.SecretsEnc {
		v, err := e.Box.Open(sealed)
		if err != nil {
			return cfg, fmt.Errorf("the stored secret %q cannot be decrypted (was LUMEN_SECRET_KEY changed?)", k)
		}
		cfg.Secrets[k] = v
	}
	return cfg, nil
}

func (e *Engine) link() string {
	if e.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(e.PublicURL, "/") + "/#/alerts"
}

func views(status string, alerts []Alert, chID string) []AlertView {
	out := make([]AlertView, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, AlertView{Fingerprint: a.Fingerprint, RuleName: a.RuleName, Severity: a.Severity, State: status, Labels: a.Labels, Value: a.Value, Since: a.Since, Annotation: a.Annotation, Ref: a.Refs[chID]})
	}
	sort.Slice(out, func(i, j int) bool { return labelText(out[i].Labels) < labelText(out[j].Labels) })
	return out
}

// Dispatch sends the groups that are ready, retries failed deliveries and calls heartbeats that are due.
func (e *Engine) Dispatch(ctx context.Context) {
	e.ensureLoaded()
	now := e.now()
	e.mu.Lock()
	for k, g := range e.groups {
		if g.readyAt.After(now) {
			continue
		}
		var as []Alert
		for _, a := range g.alerts {
			as = append(as, a)
		}
		v := views(g.status, as, g.ch.ID)
		title, body := Compose(g.status, v, e.link())
		e.retry = append(e.retry, &delivery{ch: g.ch, msg: Message{Tenant: g.ch.Tenant, Channel: g.ch.Name, Status: g.status, GroupKey: k, Title: title, Body: body, Link: e.link(), Alerts: v}, due: now})
		delete(e.groups, k)
	}
	var todo, keep []*delivery
	for _, d := range e.retry {
		if d.due.After(now) {
			keep = append(keep, d)
		} else {
			todo = append(todo, d)
		}
	}
	e.retry = keep
	e.mu.Unlock()
	sort.Slice(todo, func(i, j int) bool { return todo[i].msg.GroupKey < todo[j].msg.GroupKey })
	for _, d := range todo {
		e.deliver(ctx, d, now)
	}
	e.heartbeats(ctx, now)
}

func (e *Engine) deliver(ctx context.Context, d *delivery, now time.Time) {
	n, ok := e.reg().Get(d.ch.Type)
	var res Result
	var err error
	if !ok {
		err = fmt.Errorf("this channel type is not available in this edition")
	} else {
		var cfg Config
		if cfg, err = e.config(d.ch); err == nil {
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			res, err = n.Send(c, e.deps(), cfg, d.msg)
			cancel()
		}
	}
	d.attempt++
	rec := Delivery{Time: now, Tenant: d.ch.Tenant, ChannelID: d.ch.ID, Channel: d.ch.Name, Status: d.msg.Status, Alerts: len(d.msg.Alerts), Attempt: d.attempt, OK: err == nil}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.health[d.ch.ID] = Health{LastOK: now}
		if d.msg.Status == "firing" {
			for _, v := range d.msg.Alerts {
				k := akey(d.ch.Tenant, v.Fingerprint)
				if a, ok := e.alerts[k]; ok {
					a.LastNotified = now
					if ref := res.Refs[v.Fingerprint]; ref != "" {
						if a.Refs == nil {
							a.Refs = map[string]string{}
						}
						a.Refs[d.ch.ID] = ref
					}
					e.alerts[k] = a
					e.put(collState, k, a)
				}
			}
		}
	} else {
		rec.Error = err.Error()
		h := e.health[d.ch.ID]
		h.LastError, h.Error = now, err.Error()
		e.health[d.ch.ID] = h
		bo := e.backoff()
		if d.attempt <= len(bo) {
			d.due = now.Add(bo[d.attempt-1])
			e.retry = append(e.retry, d)
		} else {
			rec.GaveUp = true
			e.log().Error("alerts: gave up delivering", "channel", d.ch.Name, "err", err)
		}
	}
	e.deliveries = append(e.deliveries, rec)
	if len(e.deliveries) > 300 {
		e.deliveries = e.deliveries[len(e.deliveries)-300:]
	}
}

func (e *Engine) heartbeats(ctx context.Context, now time.Time) {
	e.mu.Lock()
	var chs []Channel
	for _, ch := range e.channels {
		if ch.Enabled && ch.Type == "heartbeat" && !e.hbNext[ch.ID].After(now) {
			chs = append(chs, ch)
		}
	}
	e.mu.Unlock()
	for _, ch := range chs {
		n, ok := e.reg().Get(ch.Type)
		p, isP := n.(Periodic)
		if !ok || !isP {
			continue
		}
		cfg, err := e.config(ch)
		if err == nil {
			c, cancel := context.WithTimeout(ctx, 15*time.Second)
			err = p.Tick(c, e.deps(), cfg)
			cancel()
		}
		e.mu.Lock()
		e.hbNext[ch.ID] = now.Add(p.Every(cfg))
		h := e.health[ch.ID]
		if err != nil {
			h.LastError, h.Error = now, err.Error()
		} else {
			h = Health{LastOK: now}
		}
		e.health[ch.ID] = h
		e.mu.Unlock()
	}
}

// Run evaluates and delivers until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	if err := e.Load(); err != nil {
		e.log().Error("alerts: could not start", "err", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		prune := time.NewTicker(10 * time.Minute)
		defer prune.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-prune.C:
				e.Prune(1000)
			case <-t.C:
				e.EvalDue(ctx)
			}
		}
	}()
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.Dispatch(ctx)
			}
		}
	}()
	wg.Wait()
}

// ---- reading ----

// ListAlerts returns the alerts of a tenant, most severe first.
func (e *Engine) ListAlerts(tenant string) []AlertOut {
	e.ensureLoaded()
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	rank := map[string]int{"critical": 0, "warning": 1, "info": 2}
	out := []AlertOut{}
	for _, a := range e.alerts {
		if a.Tenant != tenant {
			continue
		}
		o := AlertOut{Alert: a}
		if s, ok := e.silencedBy(a, now); ok {
			o.Silenced, o.SilenceID = true, s.ID
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == "firing"
		}
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		return out[i].Since.Before(out[j].Since)
	})
	return out
}

// Counts returns how many alerts of a tenant are firing and pending (silenced ones are not counted as firing).
func (e *Engine) Counts(tenant string) (firing, pending int) {
	for _, a := range e.ListAlerts(tenant) {
		switch {
		case a.State == "pending":
			pending++
		case !a.Silenced:
			firing++
		}
	}
	return
}

// Deliveries returns the recent delivery log of a tenant, newest first.
func (e *Engine) Deliveries(tenant string) []Delivery {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Delivery
	for i := len(e.deliveries) - 1; i >= 0; i-- {
		if e.deliveries[i].Tenant == tenant {
			out = append(out, e.deliveries[i])
		}
	}
	return out
}

// RuleHealth returns the last evaluation error of each rule of a tenant (empty = healthy).
func (e *Engine) RuleHealth(tenant string) map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for id, msg := range e.lastErr {
		if r, ok := e.rules[id]; ok && r.Tenant == tenant {
			out[id] = msg
		}
	}
	return out
}

// ---- acknowledging ----

func (e *Engine) Ack(tenant, fingerprint, user string, ack bool) error {
	e.ensureLoaded()
	e.mu.Lock()
	defer e.mu.Unlock()
	k := akey(tenant, fingerprint)
	a, ok := e.alerts[k]
	if !ok {
		return ErrNotFound
	}
	a.Acked = ack
	if ack {
		a.AckedBy, a.AckedAt = user, e.now()
		e.history("ack", a, user)
	} else {
		a.AckedBy, a.AckedAt = "", time.Time{}
	}
	e.alerts[k] = a
	e.put(collState, k, a)
	return nil
}

// ---- rules ----

func (e *Engine) ListRules(tenant string) []Rule {
	e.ensureLoaded()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Rule{}
	for _, r := range e.rules {
		if r.Tenant == tenant {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (e *Engine) GetRule(tenant, id string) (Rule, bool) {
	e.ensureLoaded()
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.rules[id]
	return r, ok && r.Tenant == tenant
}

// PutRule creates (id empty) or replaces a rule of a tenant.
func (e *Engine) PutRule(tenant, id string, in Rule) (Rule, error) {
	e.ensureLoaded()
	if err := in.Normalize(); err != nil {
		return Rule{}, invalid("%v", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now().UTC()
	if id == "" {
		n := 0
		for _, r := range e.rules {
			if r.Tenant == tenant {
				n++
			}
		}
		if n >= MaxRulesPerTenant {
			return Rule{}, invalid("at most %d rules", MaxRulesPerTenant)
		}
		in.ID, in.Created = newID("r_"), now
	} else {
		cur, ok := e.rules[id]
		if !ok || cur.Tenant != tenant {
			return Rule{}, ErrNotFound
		}
		in.ID, in.Created = id, cur.Created
	}
	in.Tenant, in.Updated = tenant, now
	e.rules[in.ID] = in
	delete(e.nextDue, in.ID) // evaluate soon
	delete(e.lastErr, in.ID)
	e.put(collRules, in.ID, in)
	if !in.Enabled {
		e.clearRuleAlertsLocked(in)
	}
	return in, nil
}

func (e *Engine) clearRuleAlertsLocked(r Rule) {
	for k, a := range e.alerts {
		if a.RuleID == r.ID {
			delete(e.alerts, k)
			e.del(collState, k)
		}
	}
	for k, g := range e.groups {
		if g.ruleID == r.ID {
			delete(e.groups, k)
		}
	}
}

func (e *Engine) DeleteRule(tenant, id string) error {
	e.ensureLoaded()
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.rules[id]
	if !ok || r.Tenant != tenant {
		return ErrNotFound
	}
	e.clearRuleAlertsLocked(r)
	delete(e.rules, id)
	delete(e.nextDue, id)
	delete(e.lastErr, id)
	e.del(collRules, id)
	return nil
}

// ---- silences ----

func (e *Engine) ListSilences(tenant string) []Silence {
	e.ensureLoaded()
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Silence{}
	for id, s := range e.silences {
		if s.Tenant != tenant {
			continue
		}
		if now.After(s.End.Add(24 * time.Hour)) { // expired a day ago: tidy up
			delete(e.silences, id)
			e.del(collSilences, id)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].End.After(out[j].End) })
	return out
}

func (e *Engine) CreateSilence(tenant string, matchers map[string]string, d time.Duration, reason, by string) (Silence, error) {
	e.ensureLoaded()
	if len(matchers) == 0 {
		return Silence{}, invalid("a silence needs at least one matcher, for example host = web1")
	}
	if err := ValidLabels(matchers, "matchers"); err != nil {
		return Silence{}, invalid("%v", err)
	}
	if d < time.Minute || d > 90*24*time.Hour {
		return Silence{}, invalid("a silence lasts between 1 minute and 90 days")
	}
	if len(reason) > 200 || strings.ContainsAny(reason, "\r\n\x00") {
		return Silence{}, invalid("the reason is too long or has line breaks")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.silences {
		if s.Tenant == tenant && s.Active(e.now()) {
			n++
		}
	}
	if n >= MaxSilencesPerTenant {
		return Silence{}, invalid("at most %d active silences", MaxSilencesPerTenant)
	}
	now := e.now().UTC()
	s := Silence{ID: newID("s_"), Tenant: tenant, Matchers: matchers, Start: now, End: now.Add(d), Reason: reason, CreatedBy: by}
	e.silences[s.ID] = s
	e.put(collSilences, s.ID, s)
	e.history("silence", Alert{Tenant: tenant, RuleName: "silence", Severity: "info", Labels: matchers}, by)
	return s, nil
}

func (e *Engine) DeleteSilence(tenant, id string) error {
	e.ensureLoaded()
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.silences[id]
	if !ok || s.Tenant != tenant {
		return ErrNotFound
	}
	delete(e.silences, id)
	e.del(collSilences, id)
	return nil
}

// ---- channels ----

func fieldKinds(n Notifier) (settings, secrets map[string]bool) {
	settings, secrets = map[string]bool{}, map[string]bool{}
	for _, f := range n.Fields() {
		if f.Kind == "secret" {
			secrets[f.Key] = true
		} else {
			settings[f.Key] = true
		}
	}
	return
}

func (e *Engine) out(c Channel) ChannelOut {
	o := ChannelOut{ID: c.ID, Name: c.Name, Type: c.Type, Enabled: c.Enabled, Settings: c.Settings, HasSecret: map[string]bool{}, Severities: c.Severities, Match: c.Match, Health: e.health[c.ID]}
	if o.Severities == nil {
		o.Severities = []string{}
	}
	if o.Match == nil {
		o.Match = map[string]string{}
	}
	for k := range c.SecretsEnc {
		o.HasSecret[k] = true
	}
	if n, ok := e.reg().Get(c.Type); ok {
		o.TypeLabel = n.Label()
	} else {
		o.TypeLabel = c.Type + " (not available in this edition)"
	}
	return o
}

func (e *Engine) ListChannels(tenant string) []ChannelOut {
	e.ensureLoaded()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []ChannelOut{}
	for _, c := range e.channels {
		if c.Tenant == tenant {
			out = append(out, e.out(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// PutChannel creates (id empty) or updates a channel. Secrets are encrypted before they are stored.
func (e *Engine) PutChannel(tenant, id string, in ChannelIn) (ChannelOut, error) {
	e.ensureLoaded()
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || !clean(in.Name, 60) {
		return ChannelOut{}, invalid("the name must be 1-60 characters on one line")
	}
	n, ok := e.reg().Get(in.Type)
	if !ok {
		return ChannelOut{}, invalid("unknown channel type %q (it may need Lumen Enterprise)", in.Type)
	}
	for _, s := range in.Severities {
		if !severities[s] {
			return ChannelOut{}, invalid("severities can be critical, warning and info")
		}
	}
	if err := ValidLabels(in.Match, "label matchers"); err != nil {
		return ChannelOut{}, invalid("%v", err)
	}
	setKeys, secKeys := fieldKinds(n)
	e.mu.Lock()
	defer e.mu.Unlock()
	var cur Channel
	if id != "" {
		c, ok := e.channels[id]
		if !ok || c.Tenant != tenant {
			return ChannelOut{}, ErrNotFound
		}
		cur = c
		if c.Type != in.Type {
			return ChannelOut{}, invalid("the type of a channel cannot be changed; create a new channel")
		}
	} else {
		cnt := 0
		for _, c := range e.channels {
			if c.Tenant == tenant {
				cnt++
			}
		}
		if cnt >= MaxChannelsPerTenant {
			return ChannelOut{}, invalid("at most %d channels", MaxChannelsPerTenant)
		}
	}
	ch := Channel{ID: cur.ID, Tenant: tenant, Name: in.Name, Type: in.Type, Enabled: in.Enabled, Settings: map[string]string{}, SecretsEnc: map[string]string{}, Severities: in.Severities, Match: in.Match, Created: cur.Created}
	if id == "" {
		ch.ID, ch.Created = newID("c_"), e.now().UTC()
	}
	for k, v := range in.Settings {
		if !setKeys[k] {
			return ChannelOut{}, invalid("unknown setting %q", k)
		}
		if !clean(strings.TrimSpace(v), 500) {
			return ChannelOut{}, invalid("the value of %q is too long or has control characters", k)
		}
		if v = strings.TrimSpace(v); v != "" {
			ch.Settings[k] = v
		}
	}
	for k, v := range cur.SecretsEnc {
		ch.SecretsEnc[k] = v
	}
	for _, k := range in.ClearSecrets {
		delete(ch.SecretsEnc, k)
	}
	for k, v := range in.Secrets {
		if !secKeys[k] {
			return ChannelOut{}, invalid("unknown secret %q", k)
		}
		if v = strings.TrimSpace(v); v != "" {
			if !clean(v, 1000) {
				return ChannelOut{}, invalid("the value of %q is too long or has control characters", k)
			}
			ch.SecretsEnc[k] = e.Box.Seal(v)
		}
	}
	cfg, err := e.config(ch)
	if err != nil {
		return ChannelOut{}, invalid("%v", err)
	}
	for _, f := range n.Fields() {
		if f.Required && cfg.Get(f.Key) == "" {
			return ChannelOut{}, invalid("%s is required", f.Label)
		}
	}
	if err := n.Validate(cfg); err != nil {
		return ChannelOut{}, invalid("%v", err)
	}
	ch.Updated = e.now().UTC()
	e.channels[ch.ID] = ch
	e.put(collChannels, ch.ID, ch)
	delete(e.hbNext, ch.ID)
	return e.out(ch), nil
}

func (e *Engine) DeleteChannel(tenant, id string) error {
	e.ensureLoaded()
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.channels[id]
	if !ok || c.Tenant != tenant {
		return ErrNotFound
	}
	delete(e.channels, id)
	delete(e.health, id)
	for k, g := range e.groups {
		if g.ch.ID == id {
			delete(e.groups, k)
		}
	}
	e.del(collChannels, id)
	return nil
}

// TestChannel sends a test message through a saved channel right now and returns what happened.
func (e *Engine) TestChannel(ctx context.Context, tenant, id string) error {
	e.ensureLoaded()
	e.mu.Lock()
	ch, ok := e.channels[id]
	e.mu.Unlock()
	if !ok || ch.Tenant != tenant {
		return ErrNotFound
	}
	n, ok := e.reg().Get(ch.Type)
	if !ok {
		return invalid("this channel type is not available in this edition")
	}
	cfg, err := e.config(ch)
	if err != nil {
		return err
	}
	now := e.now()
	v := []AlertView{{Fingerprint: "test", RuleName: "Lumen test alert", Severity: "info", State: "firing", Labels: map[string]string{"test": "true"}, Value: 1, Since: now, Annotation: "This is a test from Lumen. Nothing is wrong."}}
	title, body := Compose("firing", v, e.link())
	title = strings.Replace(title, "[FIRING]", "[TEST]", 1)
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = n.Send(c, e.deps(), cfg, Message{Tenant: tenant, Channel: ch.Name, Status: "firing", GroupKey: "test", Title: title, Body: body, Link: e.link(), Alerts: v})
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := Delivery{Time: now, Tenant: tenant, ChannelID: ch.ID, Channel: ch.Name, Status: "test", Alerts: 1, Attempt: 1, OK: err == nil}
	if err != nil {
		rec.Error = err.Error()
		h := e.health[ch.ID]
		h.LastError, h.Error = now, err.Error()
		e.health[ch.ID] = h
	} else {
		e.health[ch.ID] = Health{LastOK: now}
	}
	e.deliveries = append(e.deliveries, rec)
	return err
}

// Templates are ready-made rules the administrator can add with one click.
func Templates() []Rule {
	return []Rule{
		{Name: "Host down", Kind: KindStatus, Status: "host_down", Severity: "critical", IntervalSec: 30, Annotation: "An agent has not reported for 2 minutes: the machine, its network or the agent is down.", Enabled: true},
		{Name: "Nextcloud instance down", Kind: KindStatus, Status: "instance_down", Severity: "critical", IntervalSec: 30, Annotation: "The agent cannot reach this Nextcloud (or the agent stopped reporting).", Enabled: true},
		{Name: "Service failed", Kind: KindStatus, Status: "service_down", Severity: "warning", IntervalSec: 30, ForSec: 60, Annotation: "A systemd service is failed, or a watched service is not running.", Enabled: true},
		{Name: "Container down", Kind: KindStatus, Status: "container_down", Severity: "warning", IntervalSec: 30, ForSec: 60, Annotation: "A Docker container is not running.", Enabled: true},
		{Name: "Disk almost full", Kind: KindMetric, Metric: "system.filesystem.utilization", Reduce: "max", GroupBy: "mountpoint", WindowSec: 300, Op: ">", Threshold: 0.9, Severity: "warning", ForSec: 300, Annotation: "A disk is more than 90 percent full.", Enabled: true},
		{Name: "High CPU", Kind: KindMetric, Metric: "system.cpu.utilization", Reduce: "avg", GroupBy: "host", WindowSec: 600, Op: ">", Threshold: 0.9, Severity: "warning", ForSec: 600, Annotation: "CPU use has been above 90 percent for 10 minutes.", Enabled: true},
		{Name: "Nextcloud response time", Kind: KindMetric, Metric: "nextcloud_status_response_seconds", Reduce: "avg", GroupBy: "instance", WindowSec: 300, Op: ">", Threshold: 2, Severity: "warning", ForSec: 300, Annotation: "A Nextcloud instance answers slower than 2 seconds.", Enabled: true},
		{Name: "Many errors in the logs", Kind: KindLog, LogSeverity: "ERROR", GroupBy: "service", WindowSec: 300, Op: ">", Threshold: 20, Severity: "warning", Annotation: "More than 20 ERROR log lines in 5 minutes.", Enabled: true},
	}
}

// Backtest replays an (unsaved) rule over a period of a tenant's history.
func (e *Engine) Backtest(ctx context.Context, tenant string, r Rule, from, to time.Time) ([]Interval, error) {
	if r.Name == "" {
		r.Name = "preview"
	}
	if err := r.Normalize(); err != nil {
		return nil, invalid("%v", err)
	}
	r.Tenant = tenant
	iv, err := e.Eval.Backtest(ctx, r, from, to)
	if err != nil && r.Kind == KindStatus {
		return nil, invalid("%v", err)
	}
	return iv, err
}

var _ = json.Marshal
