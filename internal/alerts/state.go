package alerts

import "time"

// Sample is the result of evaluating a rule for one series: its labels, its value, and whether the condition holds.
type Sample struct {
	Labels map[string]string
	Value  float64
	Firing bool
}

// Transition is something that happened to an alert and that people should hear about.
type Transition struct {
	Type  string // firing | resolved
	Alert Alert
}

// DefaultRecoverAfter is how many evaluations in a row the condition must be false before a firing alert resolves.
// It stops a value that hovers around the threshold from flapping.
const DefaultRecoverAfter = 2

// Step applies one evaluation of a rule to the alerts it currently has (prev, keyed by fingerprint) and returns the
// new set and the transitions. noData says the query returned no series at all.
//
//	not present -> pending (condition true) -> firing (true for "for") -> resolved (false for recoverAfter evaluations)
//
// A pending alert whose condition turns false disappears without a message: nobody was told about it.
func Step(rule Rule, prev map[string]Alert, samples []Sample, noData bool, now time.Time, recoverAfter int) (map[string]Alert, []Transition) {
	if recoverAfter < 1 {
		recoverAfter = DefaultRecoverAfter
	}
	if noData {
		switch rule.NoData {
		case "keep":
			return prev, nil
		case "alert":
			samples = []Sample{{Labels: map[string]string{"nodata": "true"}, Firing: true}}
		}
	}
	next := map[string]Alert{}
	var tr []Transition
	seen := map[string]bool{}
	merged := func(l map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range rule.Labels {
			m[k] = v
		}
		for k, v := range l {
			m[k] = v
		}
		return m
	}
	for _, s := range samples {
		fp := Fingerprint(rule.ID, s.Labels)
		seen[fp] = true
		a, had := prev[fp]
		if !had {
			if !s.Firing {
				continue
			}
			a = Alert{Fingerprint: fp, RuleID: rule.ID, Tenant: rule.Tenant, State: "pending", Since: now}
		}
		a.RuleName, a.Severity, a.Annotation, a.Labels, a.Value, a.LastEval = rule.Name, rule.Severity, rule.Annotation, merged(s.Labels), s.Value, now
		if s.Firing {
			a.OKStreak = 0
			if a.State == "pending" && now.Sub(a.Since) >= time.Duration(rule.ForSec)*time.Second {
				a.State, a.FiringSince = "firing", now
				tr = append(tr, Transition{"firing", a})
			}
			next[fp] = a
			continue
		}
		if a.State == "pending" {
			continue // never fired: forget it
		}
		a.OKStreak++
		if a.OKStreak >= recoverAfter {
			tr = append(tr, Transition{"resolved", a})
			continue
		}
		next[fp] = a
	}
	// alerts that no sample mentions any more: the series is gone, which counts as "condition false"
	for fp, a := range prev {
		if seen[fp] {
			continue
		}
		if a.State == "pending" {
			continue
		}
		a.OKStreak++
		a.LastEval = now
		if a.OKStreak >= recoverAfter {
			tr = append(tr, Transition{"resolved", a})
			continue
		}
		next[fp] = a
	}
	return next, tr
}
