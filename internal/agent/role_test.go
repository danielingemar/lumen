package agent

import (
	"strings"
	"testing"
	"time"
)

func cfgOf(t *testing.T, env map[string]string) Config {
	c, err := LoadConfig("/nonexistent", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAnAgentKnowsWhetherItIsAHostOrOnlyChecksInstances(t *testing.T) {
	nc := map[string]string{"LUMEN_AGENT_NEXTCLOUD_URL": "https://cloud.example.com"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range nc {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"a plain agent":                          {map[string]string{}, "host"},
		"a host that also checks an instance":    {nc, "host"},
		"host metrics off, nothing to check":     {map[string]string{"LUMEN_AGENT_HOST_METRICS": "false"}, "host"},
		"host metrics off, one instance checked": {with(map[string]string{"LUMEN_AGENT_HOST_METRICS": "false"}), "checker"},
		"told to be a checker":                   {map[string]string{"LUMEN_AGENT_ROLE": "checker"}, "checker"},
		"told to be a host":                      {with(map[string]string{"LUMEN_AGENT_HOST_METRICS": "false", "LUMEN_AGENT_ROLE": "host"}), "host"},
		"a nonsense role is ignored":             {with(map[string]string{"LUMEN_AGENT_HOST_METRICS": "false", "LUMEN_AGENT_ROLE": "boss"}), "checker"},
	} {
		if got := cfgOf(t, c.env).Role(); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	// watching services means the machine is being monitored, even without host metrics
	c := cfgOf(t, with(map[string]string{"LUMEN_AGENT_HOST_METRICS": "false"}))
	c.WatchServices = []string{"nginx"}
	if c.Role() != "host" {
		t.Fatal("an agent that watches services on its machine is a host")
	}
}

func TestTheHostNameCanBeChosen(t *testing.T) {
	if cfgOf(t, map[string]string{"LUMEN_AGENT_HOSTNAME": "  nc-checker-acme "}).Hostname != "nc-checker-acme" {
		t.Fatal("trimmed")
	}
	if cfgOf(t, map[string]string{}).Hostname != "" {
		t.Fatal("by default the machine's own name is used")
	}
	if cfgOf(t, map[string]string{"LUMEN_AGENT_HOSTNAME": strings.Repeat("a", 200)}).Hostname != "" {
		t.Fatal("an absurd name is ignored")
	}
}

func TestTheInfoLineSaysWhatTheAgentIs(t *testing.T) {
	s := NewSelf("web1")
	role := func() (string, bool) {
		for _, p := range s.Collect("host", time.Now().UnixNano()) {
			if p.Name == "lumen_agent_info" {
				v, ok := p.Attrs["role"]
				return v, ok
			}
		}
		return "", false
	}
	if _, ok := role(); ok {
		t.Fatal("before it is set, nothing is claimed")
	}
	s.SetRole("checker")
	if v, ok := role(); !ok || v != "checker" {
		t.Fatalf("%q %v", v, ok)
	}
}
