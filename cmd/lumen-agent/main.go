// lumen-agent collects host metrics, systemd services and Docker containers, scrapes Prometheus endpoints,
// monitors Nextcloud instances and tails log files, then pushes everything to Lumen over OTLP/HTTP JSON.
// What it collects can be changed from the Lumen web UI (Hosts and Instances): the agent fetches its
// configuration from the server every minute and restarts its collectors when it changed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/danielingemar/lumen/internal/agent"
)

const pollRemote = 60 * time.Second

func main() {
	path := flag.String("config", "lumen-agent.json", "path to JSON config (optional; LUMEN_AGENT_* env vars also work)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(agent.Version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := agent.LoadConfig(*path, os.Getenv)
	if err != nil {
		log.Error("config error", "err", err)
		os.Exit(1)
	}
	if cfg.APIKey == "" {
		log.Warn("no API key configured; this only works against a Lumen running in dev mode")
	}
	hostname, _ := os.Hostname()
	snd := &agent.Sender{URL: cfg.URL, APIKey: cfg.APIKey}
	self := agent.NewSelf(hostname)
	self.SetLumenURL(cfg.URL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.MetricsListen != "" {
		srv := &http.Server{Addr: cfg.MetricsListen, Handler: self.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("metrics endpoint failed", "err", err)
			}
		}()
		go func() { <-ctx.Done(); srv.Close() }()
	}

	warned := false
	for {
		eff, rev := cfg, ""
		if !cfg.NoRemote {
			rc, err := agent.FetchRemote(ctx, cfg.URL, cfg.APIKey, hostname)
			switch {
			case err == nil:
				eff, rev = cfg.Merge(rc), rc.Revision
			case errors.Is(err, agent.ErrRemoteUnsupported):
			case !warned:
				warned = true
				log.Warn("could not fetch the configuration from the server; using the local one and retrying", "err", err)
			}
		}
		self.SetRejected(len(eff.RejectedLogPaths))
		if len(eff.RejectedLogPaths) > 0 {
			log.Warn("log paths from the server were refused by this machine's allow-list (set allowed_log_dirs in the agent config to change that)", "paths", eff.RejectedLogPaths)
		}
		runCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		n := start(runCtx, &wg, eff, snd, self, hostname, log)
		log.Info("lumen-agent running", "version", agent.Version, "host", hostname, "url", cfg.URL, "config_revision", rev,
			"host_metrics", eff.HostOn(), "systemd", eff.SystemdOn(), "containers", eff.ContainersOn(),
			"nextcloud_targets", len(eff.Nextcloud), "log_sources", n)

		changed := false
		if cfg.NoRemote {
			<-ctx.Done()
		} else {
			t := time.NewTicker(pollRemote)
		wait:
			for {
				select {
				case <-ctx.Done():
					break wait
				case <-t.C:
					if rc, err := agent.FetchRemote(ctx, cfg.URL, cfg.APIKey, hostname); err == nil && rc.Revision != rev {
						changed = true
						break wait
					}
				}
			}
			t.Stop()
		}
		cancel()
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		if changed {
			log.Info("configuration changed in the Lumen UI, restarting collectors")
		}
	}
}

// start launches the collectors for one configuration and returns the number of log sources.
func start(ctx context.Context, wg *sync.WaitGroup, cfg agent.Config, snd *agent.Sender, self *agent.Self, hostname string, log *slog.Logger) int {
	wg.Add(1)
	go func() {
		defer wg.Done()
		host := &agent.Host{Service: cfg.HostService}
		tick := time.NewTicker(time.Duration(cfg.IntervalSeconds) * time.Second)
		defer tick.Stop()
		var lastSvc time.Time
		lastWarn := map[string]string{}
		warn := func(what string, err error) { // log a problem once, and again only if it changes
			m := ""
			if err != nil {
				m = err.Error()
			}
			if m != "" && lastWarn[what] != m {
				log.Warn(what+" collection problem", "err", m)
			}
			lastWarn[what] = m
		}
		for {
			now := time.Now().UnixNano()
			byService := map[string][]agent.Point{}
			add := func(pts []agent.Point) {
				for _, p := range pts {
					byService[p.Service] = append(byService[p.Service], p)
				}
			}
			if cfg.HostOn() {
				add(host.Collect(now))
			}
			if time.Since(lastSvc) >= 30*time.Second { // services change slowly; every 30 s is plenty
				lastSvc = time.Now()
				if cfg.SystemdOn() {
					pts, err := agent.CollectSystemd(ctx, cfg.WatchServices, now)
					warn("systemd", err)
					for i := range pts {
						pts[i].Service = cfg.HostService
					}
					add(pts)
				}
				if cfg.ContainersOn() {
					pts, err := agent.CollectContainers(ctx, "", now)
					if errors.Is(err, agent.ErrNoDocker) {
						err = nil
					}
					warn("docker", err)
					for i := range pts {
						pts[i].Service = cfg.HostService
					}
					add(pts)
				}
			}
			for _, s := range cfg.Scrape {
				pts, err := agent.Scrape(ctx, s.URL, s.Service)
				self.ObserveScrape(s.URL, err)
				if err != nil {
					log.Warn("scrape failed", "url", s.URL, "err", err)
					continue
				}
				add(pts)
			}
			for _, nc := range cfg.Nextcloud {
				pts, err := agent.CollectNextcloud(ctx, nc, now)
				self.ObserveScrape(nc.URL, err)
				if err != nil {
					log.Warn("nextcloud poll problem", "url", nc.URL, "err", err)
				}
				add(pts)
			}
			if cfg.SelfOn() {
				add(self.Collect(agent.SelfService, now))
			}
			for svc, pts := range byService {
				if len(pts) == 0 {
					continue
				}
				body, _ := agent.MetricsPayload(svc, hostname, pts)
				if err := snd.Post(ctx, "/v1/metrics", body); err != nil {
					self.SendError("metrics")
					log.Error("send metrics failed", "service", svc, "err", err)
				} else {
					self.Sent("metrics", len(pts))
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	var tailers []*agent.Tailer
	for _, l := range cfg.Logs {
		tailers = append(tailers, &agent.Tailer{Patterns: l.Paths, Service: l.Service, Format: l.Format})
	}
	for _, nc := range cfg.Nextcloud {
		if nc.LogPath != "" {
			tailers = append(tailers, &agent.Tailer{Patterns: []string{nc.LogPath}, Service: nc.Service, Format: "nextcloud"})
		}
	}
	if len(tailers) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(2 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				for _, t := range tailers {
					if logs := t.Poll(); len(logs) > 0 {
						body, _ := agent.LogsPayload(t.Service, hostname, logs)
						if err := snd.Post(ctx, "/v1/logs", body); err != nil {
							self.SendError("logs")
							log.Error("send logs failed", "service", t.Service, "err", err)
						} else {
							self.Sent("logs", len(logs))
						}
					}
				}
			}
		}()
	}
	return len(tailers)
}
